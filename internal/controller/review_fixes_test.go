// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr/funcr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/mevijays/kube-pfsense-dns-controller/internal/pfsense"
)

type flakyApply struct {
	memPfSense
	failApplies int
}

func (f *flakyApply) Apply(ctx context.Context) error {
	if f.failApplies > 0 {
		f.failApplies--
		return errors.New("apply timed out")
	}
	return f.memPfSense.Apply(ctx)
}

// A failed apply must be retried on the next sync even though the config already matches.
func TestFailedApplyIsRetried(t *testing.T) {
	gw, rt := fixture()
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(gw, rt).Build()
	pf := &flakyApply{failApplies: 1}
	r := newRec(c, pf, false)

	if err := r.Sync(context.Background()); err == nil {
		t.Fatal("expected the apply error")
	}
	if len(pf.items) != 1 || pf.applies != 0 || !r.applyPending {
		t.Fatalf("setup: items=%d applies=%d pending=%v", len(pf.items), pf.applies, r.applyPending)
	}
	if err := r.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pf.applies != 1 || r.applyPending {
		t.Fatalf("apply not retried: applies=%d pending=%v", pf.applies, r.applyPending)
	}
	if err := r.Sync(context.Background()); err != nil || pf.applies != 1 {
		t.Fatalf("idle sync applied again: %d", pf.applies)
	}
}

// When the gateway temporarily loses its address the DNS record stays.
func TestRecordKeptWhileGatewayAddressIsMissing(t *testing.T) {
	gw, rt := fixture()
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(gw, rt).Build()
	pf := &memPfSense{}
	r := newRec(c, pf, false)
	if err := r.Sync(context.Background()); err != nil || len(pf.items) != 1 {
		t.Fatalf("setup: %v %v", err, pf.items)
	}
	gw.Status.Addresses = nil // gateway being re-programmed / LB IP released
	if err := c.Update(context.Background(), gw); err != nil {
		t.Fatal(err)
	}
	if err := r.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(pf.items) != 1 || pf.items[0].IP[0] != "172.16.1.63" {
		t.Fatalf("record was removed or changed during the outage: %+v", pf.items)
	}
	// Deleting the route itself still removes the record.
	if err := c.Delete(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
	if err := r.Sync(context.Background()); err != nil || len(pf.items) != 0 {
		t.Fatalf("route deleted but record kept: %v %+v", err, pf.items)
	}
}

// Conflicts are logged once when they appear and once when resolved, not every sync.
func TestConflictsAreNotLoggedEverySync(t *testing.T) {
	gw, rt := fixture()
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(gw, rt).Build()
	hand := pfsense.HostOverride{Host: "app2", Domain: "mylab.lan", IP: []string{"172.16.1.61"}, Descr: "hand made"}
	pf := &memPfSense{items: []pfsense.HostOverride{hand}}

	var mu sync.Mutex
	var lines []string
	log := funcr.New(func(prefix, args string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, args)
	}, funcr.Options{})
	r := newRec(c, pf, false)
	r.Log = log
	for i := 0; i < 5; i++ {
		if err := r.Sync(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	count := func(s string) (n int) {
		for _, l := range lines {
			if strings.Contains(l, s) {
				n++
			}
		}
		return n
	}
	if n := count("CONFLICT"); n != 1 {
		t.Fatalf("conflict logged %d times over 5 syncs, want 1", n)
	}
	// resolve it by removing the hand-made entry -> "conflict resolved" once, then a create
	pf.items = nil
	if err := r.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := count("conflict resolved"); n != 1 {
		t.Fatalf("resolution logged %d times", n)
	}
}
