package controller

import (
	"context"
	"strings"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/warewave/horus/pkg/ingressdns"
)

// GarbageCollector periodically removes server-side owners whose Ingress no
// longer exists or is no longer opted in. It covers deletes the reconciler
// never saw (operator down, missed watch events).
//
// It reads Ingresses through the API reader (not the cache) so it is correct
// even before informers have synced.
type GarbageCollector struct {
	Reader   client.Reader
	Horus    Horus
	Interval time.Duration
}

// Start implements manager.Runnable. It runs once immediately, then on Interval.
func (g *GarbageCollector) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("gc")
	if g.Interval <= 0 {
		g.Interval = 5 * time.Minute
	}
	t := time.NewTicker(g.Interval)
	defer t.Stop()
	for {
		removed, err := g.RunOnce(ctx)
		if err != nil {
			logger.Error(err, "garbage collection failed")
		} else if removed > 0 {
			logger.Info("garbage collected stale owners", "removed", removed)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable: only the
// leader collects garbage.
func (g *GarbageCollector) NeedLeaderElection() bool { return true }

// RunOnce performs one pass and returns how many owners were removed.
func (g *GarbageCollector) RunOnce(ctx context.Context) (int, error) {
	owners, err := g.Horus.ListOwners(ctx)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, o := range owners {
		ns, name, ok := strings.Cut(o, "/")
		if !ok {
			continue
		}
		var ing networkingv1.Ingress
		err := g.Reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &ing)
		switch {
		case apierrors.IsNotFound(err):
		case err != nil:
			return removed, err
		case ingressdns.Enabled(ing.Annotations) && ing.DeletionTimestamp.IsZero():
			continue // still wanted
		}
		if err := g.Horus.DeleteOwner(ctx, ns, name); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}
