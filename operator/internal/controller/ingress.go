// Package controller contains the horus operator's reconcilers.
package controller

import (
	"context"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/warewave/horus/pkg/api"
	horus "github.com/warewave/horus/pkg/client"
	"github.com/warewave/horus/pkg/ingressdns"
)

// Horus is the subset of the horus API client the operator uses.
// *client.Client from pkg/client implements it.
type Horus interface {
	ReplaceOwner(ctx context.Context, namespace, name string, recs []api.Record) (api.OwnerRecordsResponse, error)
	DeleteOwner(ctx context.Context, namespace, name string) error
	ListOwners(ctx context.Context) ([]string, error)
}

// Event reasons.
const (
	ReasonRegistered        = "Registered"
	ReasonHostnameConflict  = "HostnameConflict"
	ReasonHostSkipped       = "HostSkipped"
	ReasonInvalidAnnotation = "InvalidAnnotation"
	ReasonRejected          = "Rejected"
)

// IngressReconciler keeps horus records in sync with annotated Ingresses.
//
// Ownership: every Ingress is one owner on the server ("<cluster>/<ns>/<name>";
// the cluster prefix comes from the token). Each reconcile sends the complete
// desired record set (PUT), so reconciles are idempotent and need no diffing
// here. Deleted Ingresses are cleaned up on the delete event, and anything
// missed while the operator was down is removed by GarbageCollector.
type IngressReconciler struct {
	client.Client
	Horus    Horus
	Zone     string // the cluster's zone, from GET /v1/whoami
	Recorder record.EventRecorder

	// PendingRequeue is how often to re-check an Ingress that has no load
	// balancer address yet. Status updates also trigger a reconcile.
	PendingRequeue time.Duration
	// ConflictRequeue is how often to retry an Ingress with conflicting names
	// (the other owner may release them).
	ConflictRequeue time.Duration
}

// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile implements reconcile.Reconciler.
func (r *IngressReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var ing networkingv1.Ingress
	if err := r.Get(ctx, req.NamespacedName, &ing); err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(1).Info("ingress gone, removing records")
			return ctrl.Result{}, r.Horus.DeleteOwner(ctx, req.Namespace, req.Name)
		}
		return ctrl.Result{}, err
	}
	if !ing.DeletionTimestamp.IsZero() || !ingressdns.Enabled(ing.Annotations) {
		logger.V(1).Info("ingress not registered (deleting or opted out), removing records")
		return ctrl.Result{}, r.Horus.DeleteOwner(ctx, req.Namespace, req.Name)
	}

	plan, err := ingressdns.Desired(inputFrom(&ing), r.Zone)
	if err != nil {
		// Malformed annotations: tell the user, do not touch existing records,
		// do not requeue (the next edit of the Ingress triggers a reconcile).
		r.Recorder.Event(&ing, corev1.EventTypeWarning, ReasonInvalidAnnotation, err.Error())
		return ctrl.Result{}, nil
	}
	for _, s := range plan.Skipped {
		r.Recorder.Eventf(&ing, corev1.EventTypeWarning, ReasonHostSkipped, "%s: %s", s.Host, s.Reason)
	}
	if plan.Pending {
		logger.Info("waiting for a load balancer address")
		return ctrl.Result{RequeueAfter: r.PendingRequeue}, nil
	}

	resp, err := r.Horus.ReplaceOwner(ctx, ing.Namespace, ing.Name, plan.Records)
	if err != nil {
		if horus.IsStatus(err, http.StatusUnprocessableEntity) {
			r.Recorder.Event(&ing, corev1.EventTypeWarning, ReasonRejected, err.Error())
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err // transient: exponential backoff
	}
	for _, c := range resp.Conflicts {
		r.Recorder.Eventf(&ing, corev1.EventTypeWarning, ReasonHostnameConflict,
			"%s is already owned by %s; not registered", c.Name, c.HeldBy)
	}
	if resp.Changed() {
		r.Recorder.Eventf(&ing, corev1.EventTypeNormal, ReasonRegistered,
			"horus records updated: %d added, %d updated, %d removed", resp.Added, resp.Updated, resp.Removed)
		logger.Info("records updated", "added", resp.Added, "updated", resp.Updated, "removed", resp.Removed)
	}
	if len(resp.Conflicts) > 0 {
		return ctrl.Result{RequeueAfter: r.ConflictRequeue}, nil
	}
	return ctrl.Result{}, nil
}

func inputFrom(ing *networkingv1.Ingress) ingressdns.Input {
	in := ingressdns.Input{Annotations: ing.Annotations}
	for _, rule := range ing.Spec.Rules {
		in.Hosts = append(in.Hosts, rule.Host)
	}
	for _, lb := range ing.Status.LoadBalancer.Ingress {
		in.LoadBalancer = append(in.LoadBalancer, ingressdns.LBEntry{IP: lb.IP, Hostname: lb.Hostname})
	}
	return in
}

// annotated passes events for Ingresses that are, or just stopped being,
// opted in. Unannotated Ingresses never cause API calls to horus.
func annotated() predicate.Funcs {
	on := func(o client.Object) bool { return o != nil && ingressdns.Enabled(o.GetAnnotations()) }
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return on(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return on(e.ObjectOld) || on(e.ObjectNew) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return on(e.Object) },
		GenericFunc: func(e event.GenericEvent) bool { return on(e.Object) },
	}
}

// SetupWithManager registers the reconciler.
func (r *IngressReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.PendingRequeue == 0 {
		r.PendingRequeue = 30 * time.Second
	}
	if r.ConflictRequeue == 0 {
		r.ConflictRequeue = 5 * time.Minute
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("horus-ingress").
		For(&networkingv1.Ingress{}, builder.WithPredicates(annotated())).
		Complete(r)
}
