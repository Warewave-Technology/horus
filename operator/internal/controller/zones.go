package controller

import (
	"context"
	"slices"
	"sort"
	"sync"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/Warewave-Technology/horus/pkg/api"
	"github.com/Warewave-Technology/horus/pkg/ingressdns"
)

// WhoAmIer is the part of the horus client the zone watcher needs.
type WhoAmIer interface {
	WhoAmI(ctx context.Context) (api.WhoAmI, error)
}

// ZonesOf returns the zones a whoami response grants, sorted. Servers older
// than v0.2.0 only send the single "zone" field.
func ZonesOf(w api.WhoAmI) []string {
	zones := slices.Clone(w.Zones)
	if len(zones) == 0 && w.Zone != "" {
		zones = []string{w.Zone}
	}
	sort.Strings(zones)
	return zones
}

// Zones is the cluster's current set of zones, shared by the reconciler and
// the ZoneWatcher.
type Zones struct {
	mu   sync.RWMutex
	list []string
}

// NewZones returns a Zones holding zones.
func NewZones(zones []string) *Zones {
	z := &Zones{}
	z.Set(zones)
	return z
}

// Get returns the current zones, sorted. The caller must not modify them.
func (z *Zones) Get() []string {
	z.mu.RLock()
	defer z.mu.RUnlock()
	return z.list
}

// Set replaces the zones and reports whether they changed.
func (z *Zones) Set(zones []string) bool {
	zones = slices.Clone(zones)
	sort.Strings(zones)
	z.mu.Lock()
	defer z.mu.Unlock()
	if slices.Equal(z.list, zones) {
		return false
	}
	z.list = zones
	return true
}

// ZoneWatcher keeps Zones in sync with the server, so `horus cluster zone
// add|rm` takes effect without restarting the operator. When the zones
// change, every opted-in Ingress is queued for reconciliation: hosts in a new
// zone get registered, hosts in a removed zone are dropped from the desired
// set.
type ZoneWatcher struct {
	Horus    WhoAmIer
	Zones    *Zones
	Reader   client.Reader // lists Ingresses (the manager's cached client)
	Events   chan<- event.GenericEvent
	Interval time.Duration
}

// Start implements manager.Runnable.
func (w *ZoneWatcher) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("zones")
	if w.Interval <= 0 {
		w.Interval = time.Minute
	}
	t := time.NewTicker(w.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		if _, err := w.Refresh(ctx); err != nil {
			logger.Error(err, "refreshing zones failed")
		}
	}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable: the
// reconciler it feeds only runs on the leader.
func (w *ZoneWatcher) NeedLeaderElection() bool { return true }

// Refresh fetches the zones from horus. When they changed, it requeues every
// opted-in Ingress and reports true.
func (w *ZoneWatcher) Refresh(ctx context.Context) (bool, error) {
	who, err := w.Horus.WhoAmI(ctx)
	if err != nil {
		return false, err
	}
	zones := ZonesOf(who)
	if !w.Zones.Set(zones) {
		return false, nil
	}
	log.FromContext(ctx).Info("cluster zones changed, re-registering all Ingresses", "zones", zones)
	var list networkingv1.IngressList
	if err := w.Reader.List(ctx, &list); err != nil {
		return true, err
	}
	for i := range list.Items {
		ing := &list.Items[i]
		if !ingressdns.Enabled(ing.Annotations) {
			continue
		}
		select {
		case w.Events <- event.GenericEvent{Object: ing}:
		case <-ctx.Done():
			return true, ctx.Err()
		}
	}
	return true, nil
}
