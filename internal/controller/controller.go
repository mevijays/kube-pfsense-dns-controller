// SPDX-License-Identifier: Apache-2.0

// Package controller wires the watches, the periodic resync and the sync loop
// into controller-runtime.
package controller

import (
	"context"
	"errors"
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
	Log        logr.Logger

	mu sync.Mutex // serialises the periodic loop and event-driven reconciles
}

// Reconcile implements reconcile.Reconciler.
func (r *Reconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	return reconcile.Result{}, r.Sync(ctx)
}

// Sync performs one full cycle.
func (r *Reconciler) Sync(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	desired, derr := r.Discoverer.Discover(ctx)
	if derr != nil {
		r.Log.Error(derr, "discovery incomplete; pruning disabled for this cycle")
	}
	current, err := r.Backend.ListHostOverrides(ctx)
	if err != nil {
		return errors.Join(derr, err)
	}
	plan := syncer.BuildPlan(desired, current, r.Owner, r.Prune && derr == nil)
	for _, name := range plan.Conflicts {
		r.Log.Info("skipping: an unmanaged host override with this name already exists", "fqdn", name)
	}
	if plan.Empty() {
		r.Log.V(1).Info("in sync", "records", len(desired))
		return derr
	}
	r.Log.Info("changes needed", "create", len(plan.Create), "update", len(plan.Update), "delete", len(plan.Delete))
	return errors.Join(derr, syncer.Execute(ctx, r.Backend, plan, r.Log))
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
		vs.SetGroupVersionKind(discover.VirtualServiceGVK)
		gw := &unstructured.Unstructured{}
		gw.SetGroupVersionKind(discover.IstioGatewayGVK)
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
