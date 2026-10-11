// SPDX-License-Identifier: Apache-2.0

// Package controller wires the watches, the periodic resync and the sync loop
// into controller-runtime.
package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/mevijays/kube-pfsense-dns-controller/internal/discover"
	"github.com/mevijays/kube-pfsense-dns-controller/internal/metrics"
	"github.com/mevijays/kube-pfsense-dns-controller/internal/pfsense"
	"github.com/mevijays/kube-pfsense-dns-controller/internal/syncer"
)

// syncKey is the single work item: every event just means "recompute and diff
// the whole desired state". The state is level-based, so deletions, IP changes
// and drift are all handled the same way and no finalizers are needed.
var syncKey = reconcile.Request{NamespacedName: types.NamespacedName{Name: "dns-sync"}}

// Reconciler runs the discover -> diff -> apply cycle.
type Reconciler struct {
	Discoverer *discover.Discoverer
	Backend    pfsense.Backend
	Owner      string
	Prune      bool
	Adopt      bool
	Log        logr.Logger

	mu sync.Mutex // serialises the periodic loop and event-driven reconciles
	// applyPending is set when writes reached pfSense but the apply that makes
	// them live did not succeed; the next sync retries the apply.
	applyPending bool
	// lastConflicts holds the conflicts reported by the previous sync, so each is
	// logged once when it appears or changes rather than on every sync.
	lastConflicts map[string]string
}

// Reconcile implements reconcile.Reconciler.
func (r *Reconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	return reconcile.Result{}, r.Sync(ctx)
}

// Sync performs one full cycle.
func (r *Reconciler) Sync(ctx context.Context) (err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer func() {
		if err != nil {
			metrics.Syncs.WithLabelValues("error").Inc()
			return
		}
		metrics.Syncs.WithLabelValues("success").Inc()
		metrics.LastSuccess.SetToCurrentTime()
	}()

	desired, derr := r.Discoverer.Discover(ctx)
	if derr != nil {
		r.Log.Error(derr, "discovery incomplete; pruning disabled for this cycle")
	}
	metrics.DesiredRecords.Set(float64(len(desired)))
	current, lerr := r.Backend.ListHostOverrides(ctx)
	if lerr != nil {
		return errors.Join(derr, lerr)
	}
	plan := syncer.BuildPlan(desired, current, syncer.Options{Owner: r.Owner, Prune: r.Prune && derr == nil, Adopt: r.Adopt})
	metrics.Conflicts.Set(float64(len(plan.Conflicts)))
	r.logConflicts(plan.Conflicts)

	if plan.Empty() {
		if r.applyPending {
			// Earlier writes were not applied (apply failed or the process was
			// interrupted): retry the apply even though nothing else changed.
			r.Log.Info("retrying apply of earlier changes")
			if err := syncer.Apply(ctx, r.Backend); err != nil {
				return errors.Join(derr, err)
			}
			r.applyPending = false
		}
		r.Log.V(1).Info("in sync", "records", len(desired))
		return derr
	}
	r.Log.Info("changes needed", "create", len(plan.Create), "update", len(plan.Update), "delete", len(plan.Delete))
	res, xerr := syncer.Execute(ctx, r.Backend, plan, r.Log)
	switch {
	case res.Applied:
		r.applyPending = false
	case res.Changed:
		r.applyPending = true
	}
	return errors.Join(derr, xerr)
}

// logConflicts logs a conflict when it first appears or changes, and when it is
// resolved, instead of repeating every one on every sync.
func (r *Reconciler) logConflicts(conflicts []syncer.Conflict) {
	now := make(map[string]string, len(conflicts))
	for _, c := range conflicts {
		key := fmt.Sprint(c.Existing, c.Desired)
		now[c.Name] = key
		if r.lastConflicts[c.Name] == key {
			continue
		}
		if c.Same() {
			r.Log.Info("unmanaged host override already resolves to the desired IP; leaving it alone (use --adopt-existing to manage it)",
				"fqdn", c.Name, "ip", c.Desired)
		} else {
			r.Log.Info("CONFLICT: an unmanaged host override has a different IP; skipping (use --adopt-existing to take it over)",
				"fqdn", c.Name, "pfsenseIP", c.Existing, "clusterIP", c.Desired)
		}
	}
	for name := range r.lastConflicts {
		if _, still := now[name]; !still {
			r.Log.Info("conflict resolved", "fqdn", name)
		}
	}
	r.lastConflicts = now
}

// Setup registers the controller and the periodic resync with the manager.
func (r *Reconciler) Setup(mgr ctrl.Manager, resync time.Duration) error {
	toSync := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{syncKey}
	})

	b := ctrl.NewControllerManagedBy(mgr).Named("kube-pfsense-dns-controller")
	if r.Discoverer.EnableGatewayAPI {
		b = b.Watches(&gatewayv1.HTTPRoute{}, toSync).Watches(&gatewayv1.Gateway{}, toSync)
	}
	if r.Discoverer.EnableIstio {
		vs := &unstructured.Unstructured{}
		vs.SetGroupVersionKind(r.Discoverer.VirtualServiceKind())
		gw := &unstructured.Unstructured{}
		gw.SetGroupVersionKind(r.Discoverer.IstioGatewayKind())
		b = b.Watches(vs, toSync).Watches(gw, toSync)
	}
	if err := b.Complete(r); err != nil {
		return err
	}

	// Initial sync, then a periodic one. The periodic loop also catches things
	// no event announces: LoadBalancer IP changes and manual edits on pfSense.
	return mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		t := time.NewTicker(resync)
		defer t.Stop()
		for {
			if err := r.Sync(ctx); err != nil {
				r.Log.Error(err, "periodic sync failed")
			}
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
			}
		}
	}))
}
