package controller

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/Warewave-Technology/horus/pkg/api"
	horus "github.com/Warewave-Technology/horus/pkg/client"
	"github.com/Warewave-Technology/horus/pkg/ingressdns"
)

type fakeHorus struct {
	mu        sync.Mutex
	owners    map[string][]api.Record // "ns/name" -> records
	conflicts map[string]string       // name -> holder, returned as conflicts
	whoami    api.WhoAmI
	reject    bool // answer ReplaceOwner with 422
}

func (f *fakeHorus) WhoAmI(context.Context) (api.WhoAmI, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.whoami, nil
}

func newFakeHorus() *fakeHorus {
	return &fakeHorus{owners: map[string][]api.Record{}, conflicts: map[string]string{}}
}

func (f *fakeHorus) ReplaceOwner(_ context.Context, ns, name string, recs []api.Record) (api.OwnerRecordsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var resp api.OwnerRecordsResponse
	if f.reject {
		return resp, &horus.APIError{Status: http.StatusUnprocessableEntity, Body: api.Error{Error: "invalid: outside the cluster's zones"}}
	}
	var kept []api.Record
	for _, r := range recs {
		if h, ok := f.conflicts[r.Name]; ok {
			resp.Conflicts = append(resp.Conflicts, api.Conflict{Name: r.Name, HeldBy: h})
			continue
		}
		kept = append(kept, r)
	}
	resp.Removed = len(f.owners[ns+"/"+name])
	resp.Added = len(kept)
	f.owners[ns+"/"+name] = kept
	return resp, nil
}

func (f *fakeHorus) DeleteOwner(_ context.Context, ns, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.owners, ns+"/"+name)
	return nil
}

func (f *fakeHorus) ListOwners(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for o := range f.owners {
		out = append(out, o)
	}
	return out, nil
}

func ingress(name string, annotated bool, hosts []string, ip string) *networkingv1.Ingress {
	ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, Annotations: map[string]string{}}}
	if annotated {
		ing.Annotations[ingressdns.AnnotationRegister] = "true"
	}
	for _, h := range hosts {
		ing.Spec.Rules = append(ing.Spec.Rules, networkingv1.IngressRule{Host: h})
	}
	if ip != "" {
		ing.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: ip}}
	}
	return ing
}

func setup(t *testing.T, objs ...*networkingv1.Ingress) (*IngressReconciler, *fakeHorus, *record.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	b := fake.NewClientBuilder().WithScheme(scheme)
	for _, o := range objs {
		b = b.WithObjects(o)
	}
	h := newFakeHorus()
	rec := record.NewFakeRecorder(100)
	return &IngressReconciler{
		Client: b.Build(), Horus: h, Zones: NewZones([]string{"prod.internal.example."}), Recorder: rec,
		PendingRequeue: 1, ConflictRequeue: 2,
	}, h, rec
}

func reconcile(t *testing.T, r *IngressReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func drain(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func TestRegisterAndRemove(t *testing.T) {
	r, h, rec := setup(t, ingress("web", true, []string{"web.prod.internal.example", "x.other.example"}, "10.0.0.1"))
	reconcile(t, r, "web")
	if got := h.owners["default/web"]; len(got) != 1 || got[0].Data != "10.0.0.1" {
		t.Fatalf("records: %+v", got)
	}
	events := strings.Join(drain(rec), "\n")
	if !strings.Contains(events, ReasonRegistered) || !strings.Contains(events, ReasonHostSkipped) {
		t.Fatalf("events: %s", events)
	}

	// Annotation removed -> records deleted.
	var ing networkingv1.Ingress
	_ = r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "web"}, &ing)
	delete(ing.Annotations, ingressdns.AnnotationRegister)
	if err := r.Update(context.Background(), &ing); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, "web")
	if _, ok := h.owners["default/web"]; ok {
		t.Fatal("records should be removed after opt-out")
	}

	// Ingress gone -> DeleteOwner is idempotent.
	reconcile(t, r, "missing")
}

func TestPendingWithoutLB(t *testing.T) {
	r, h, _ := setup(t, ingress("web", true, []string{"web.prod.internal.example"}, ""))
	if res := reconcile(t, r, "web"); res.RequeueAfter != 1 {
		t.Fatalf("expected requeue, got %+v", res)
	}
	if len(h.owners) != 0 {
		t.Fatal("nothing should be written while pending")
	}
}

func TestConflictRequeues(t *testing.T) {
	r, h, rec := setup(t, ingress("web", true, []string{"web.prod.internal.example"}, "10.0.0.1"))
	h.conflicts["web.prod.internal.example."] = "manual"
	if res := reconcile(t, r, "web"); res.RequeueAfter != 2 {
		t.Fatalf("expected conflict requeue, got %+v", res)
	}
	if !strings.Contains(strings.Join(drain(rec), "\n"), ReasonHostnameConflict) {
		t.Fatal("expected conflict event")
	}
}

func TestInvalidAnnotation(t *testing.T) {
	ing := ingress("web", true, []string{"web.prod.internal.example"}, "10.0.0.1")
	ing.Annotations[ingressdns.AnnotationTTL] = "soon"
	r, h, rec := setup(t, ing)
	reconcile(t, r, "web")
	if len(h.owners) != 0 || !strings.Contains(strings.Join(drain(rec), "\n"), ReasonInvalidAnnotation) {
		t.Fatal("invalid annotation must only produce an event")
	}
}

func TestGarbageCollector(t *testing.T) {
	r, h, _ := setup(t,
		ingress("keep", true, nil, ""),
		ingress("optout", false, nil, ""),
	)
	h.owners["default/keep"] = nil
	h.owners["default/optout"] = nil
	h.owners["default/gone"] = nil
	gc := &GarbageCollector{Reader: r.Client, Horus: h}
	n, err := gc.RunOnce(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("removed %d, err %v", n, err)
	}
	if _, ok := h.owners["default/keep"]; !ok || len(h.owners) != 1 {
		t.Fatalf("owners left: %v", h.owners)
	}
}

func TestSeveralZones(t *testing.T) {
	r, h, rec := setup(t, ingress("web", true,
		[]string{"web.prod.internal.example", "web.edge.example", "x.other.example"}, "10.0.0.1"))
	r.Zones.Set([]string{"prod.internal.example.", "edge.example."})
	reconcile(t, r, "web")
	var names []string
	for _, rr := range h.owners["default/web"] {
		names = append(names, rr.Name)
	}
	if strings.Join(names, ",") != "web.edge.example.,web.prod.internal.example." {
		t.Fatalf("records: %v", names)
	}
	if events := strings.Join(drain(rec), "\n"); !strings.Contains(events, "x.other.example") {
		t.Fatalf("expected the out-of-zone host to be skipped: %s", events)
	}
}

func TestZonesOf(t *testing.T) {
	if got := ZonesOf(api.WhoAmI{Zones: []string{"b.", "a."}, Zone: "b."}); strings.Join(got, ",") != "a.,b." {
		t.Fatalf("zones: %v", got)
	}
	// Servers older than v0.2.0 only send "zone".
	if got := ZonesOf(api.WhoAmI{Zone: "a."}); strings.Join(got, ",") != "a." {
		t.Fatalf("legacy zone: %v", got)
	}
	if got := ZonesOf(api.WhoAmI{}); len(got) != 0 {
		t.Fatalf("no zones: %v", got)
	}
}

func TestZoneWatcherRequeuesOnChange(t *testing.T) {
	r, h, _ := setup(t,
		ingress("a", true, nil, ""),
		ingress("b", true, nil, ""),
		ingress("off", false, nil, ""),
	)
	events := make(chan event.GenericEvent, 10)
	w := &ZoneWatcher{Horus: h, Zones: r.Zones, Reader: r.Client, Events: events}

	h.whoami = api.WhoAmI{Cluster: "prod", Zones: []string{"prod.internal.example."}}
	if changed, err := w.Refresh(context.Background()); err != nil || changed || len(events) != 0 {
		t.Fatalf("unchanged zones: changed=%v err=%v events=%d", changed, err, len(events))
	}

	h.whoami.Zones = []string{"edge.example.", "prod.internal.example."}
	changed, err := w.Refresh(context.Background())
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if got := strings.Join(r.Zones.Get(), ","); got != "edge.example.,prod.internal.example." {
		t.Fatalf("zones: %s", got)
	}
	close(events)
	var queued []string
	for e := range events {
		queued = append(queued, e.Object.GetName())
	}
	sort.Strings(queued)
	if strings.Join(queued, ",") != "a,b" {
		t.Fatalf("queued: %v (only opted-in Ingresses)", queued)
	}
}

func TestRejectedRefreshesZones(t *testing.T) {
	r, h, rec := setup(t, ingress("web", true, []string{"web.prod.internal.example"}, "10.0.0.1"))
	h.reject = true
	refreshed, changed := 0, false
	r.RefreshZones = func(context.Context) (bool, error) { refreshed++; return changed, nil }

	// Zones unchanged: the rejection is real and reported.
	reconcile(t, r, "web")
	if refreshed != 1 || !strings.Contains(strings.Join(drain(rec), "\n"), ReasonRejected) {
		t.Fatalf("refreshed=%d", refreshed)
	}
	// Zones changed under us: no event, the watcher requeues everything.
	changed = true
	reconcile(t, r, "web")
	if refreshed != 2 || len(drain(rec)) != 0 {
		t.Fatalf("refreshed=%d, expected no event after a zone change", refreshed)
	}
}
