// SPDX-License-Identifier: Apache-2.0

package syncer

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/go-logr/logr"

	"github.com/mevijays/kube-pfsense-dns-controller/internal/pfsense"
	"github.com/mevijays/kube-pfsense-dns-controller/internal/records"
)

func TestConflictSame(t *testing.T) {
	if !(Conflict{Existing: []string{"2.2.2.2", "1.1.1.1"}, Desired: []string{"1.1.1.1", "2.2.2.2"}}).Same() {
		t.Error("same IPs in a different order must be Same")
	}
	if (Conflict{Existing: []string{"1.1.1.1"}, Desired: []string{"2.2.2.2"}}).Same() {
		t.Error("different IPs reported Same")
	}
}

func TestAdoptTakesOverMatchingUnmanagedEntry(t *testing.T) {
	manual := pfsense.HostOverride{ID: 13, Host: "jenkins", Domain: "mylab.lan", IP: []string{"172.16.1.61"}, Descr: "istioingress gatway"}
	other := pfsense.HostOverride{ID: 14, Host: "nas", Domain: "mylab.lan", IP: []string{"10.0.0.5"}, Descr: "nas"}
	want := desired([3]string{"jenkins", "mylab.lan", "172.16.1.61"})

	p := BuildPlan(want, []pfsense.HostOverride{manual, other}, Options{Owner: owner, Prune: true, Adopt: true})
	if len(p.Update) != 1 || len(p.Create) != 0 || len(p.Delete) != 0 || len(p.Conflicts) != 0 {
		t.Fatalf("plan = %+v", p)
	}
	u := p.Update[0].New
	if u.ID != 13 || p.Update[0].Old.Descr != "istioingress gatway" || !strings.HasPrefix(u.Descr, Marker(owner)) || !strings.HasSuffix(u.Descr, "| istioingress gatway") {
		t.Fatalf("adopted entry = %+v (must be tagged and keep the original note)", u)
	}

	// After adoption the entry is managed: an IP change updates it and keeps the note.
	adopted := u
	p = BuildPlan(desired([3]string{"jenkins", "mylab.lan", "172.16.1.63"}), []pfsense.HostOverride{adopted}, Options{Owner: owner, Prune: true})
	if len(p.Update) != 1 || !strings.HasSuffix(p.Update[0].New.Descr, "| istioingress gatway") {
		t.Fatalf("update of adopted entry lost its note: %+v", p)
	}
	// ...and when nothing changes there is nothing to do.
	if p := BuildPlan(want, []pfsense.HostOverride{adopted}, Options{Owner: owner, Prune: true}); !p.Empty() {
		t.Fatalf("adopted entry should be stable: %+v", p)
	}
}

func TestAdoptOffReportsConflictWithBothIPs(t *testing.T) {
	manual := pfsense.HostOverride{ID: 1, Host: "vault", Domain: "mylab.lan", IP: []string{"172.16.1.61"}, Descr: "vault k8s"}
	p := BuildPlan(desired([3]string{"vault", "mylab.lan", "172.16.1.63"}), []pfsense.HostOverride{manual}, Options{Owner: owner, Prune: true})
	if !p.Empty() || len(p.Conflicts) != 1 {
		t.Fatalf("plan = %+v", p)
	}
	c := p.Conflicts[0]
	if c.Same() || !reflect.DeepEqual(c.Existing, []string{"172.16.1.61"}) || !reflect.DeepEqual(c.Desired, []string{"172.16.1.63"}) {
		t.Fatalf("conflict = %+v", c)
	}
}

func TestDuplicateManagedEntriesAreCollapsed(t *testing.T) {
	cur := []pfsense.HostOverride{managed(0, "dup", "1.1.1.1"), managed(1, "dup", "1.1.1.1"), managed(2, "dup", "1.1.1.1")}
	p := BuildPlan(desired([3]string{"dup", "mylab.lan", "1.1.1.1"}), cur, Options{Owner: owner, Prune: true})
	ids := []int{}
	for _, d := range p.Delete {
		ids = append(ids, d.ID)
	}
	sort.Ints(ids)
	if !reflect.DeepEqual(ids, []int{1, 2}) || len(p.Update)+len(p.Create) != 0 {
		t.Fatalf("plan = %+v", p)
	}
}

func TestDescriptionIsBounded(t *testing.T) {
	r := &records.Record{Host: "x", Domain: "mylab.lan", IPs: []string{"1.1.1.1"}, Sources: []string{"a", "b", "c", "d", "e"}}
	d := description(owner, r, strings.Repeat("n", 5000))
	if len(d) > 1024 || !strings.HasPrefix(d, Marker(owner)) {
		t.Fatalf("len=%d prefix=%q", len(d), d[:20])
	}
	if strings.Contains(d, ",d") {
		t.Error("only the first 3 sources should be listed")
	}
}

// ---- Execute: failure handling ----

type flaky struct {
	fakeBackend
	failCreate, failDelete, failApply bool
}

func (f *flaky) CreateHostOverride(ctx context.Context, h pfsense.HostOverride) error {
	if f.failCreate {
		return errors.New("boom create")
	}
	return f.fakeBackend.CreateHostOverride(ctx, h)
}
func (f *flaky) DeleteHostOverride(ctx context.Context, id int) error {
	if f.failDelete {
		return errors.New("boom delete")
	}
	return f.fakeBackend.DeleteHostOverride(ctx, id)
}
func (f *flaky) Apply(ctx context.Context) error {
	if f.failApply {
		return errors.New("boom apply")
	}
	return f.fakeBackend.Apply(ctx)
}

func TestExecuteContinuesAfterFailures(t *testing.T) {
	u := managed(1, "u", "1.1.1.1")
	u2 := u
	u2.IP = []string{"2.2.2.2"}
	d := managed(2, "d", "1.1.1.1")
	p := Plan{
		Create: []pfsense.HostOverride{{Host: "a", Domain: "mylab.lan"}},
		Update: []Update{{Old: u, New: u2}},
		Delete: []pfsense.HostOverride{d},
	}
	f := &flaky{fakeBackend: fakeBackend{byID: store(u, d)}, failCreate: true}
	res, err := Execute(context.Background(), f, p, logr.Discard())
	if err == nil || !strings.Contains(err.Error(), "boom create") {
		t.Fatalf("err = %v", err)
	}
	if !reflect.DeepEqual(f.ops, []string{"update u.mylab.lan", "delete 2"}) {
		t.Errorf("remaining operations did not run: %v", f.ops)
	}
	if f.applied != 1 || !res.Applied {
		t.Errorf("apply must still run for the changes that succeeded, got %d", f.applied)
	}

	// everything fails -> nothing changed -> no apply
	f = &flaky{fakeBackend: fakeBackend{byID: store(d)}, failCreate: true, failDelete: true}
	if res, _ := Execute(context.Background(), f, Plan{Create: p.Create, Delete: p.Delete}, logr.Discard()); f.applied != 0 || res.Changed {
		t.Errorf("apply called although nothing changed")
	}

	// apply failure is reported, and the result says the writes are not applied
	f = &flaky{fakeBackend: fakeBackend{byID: store(u, d)}, failApply: true}
	res, err = Execute(context.Background(), f, p, logr.Discard())
	if err == nil || !strings.Contains(err.Error(), "apply") || !res.Changed || res.Applied {
		t.Errorf("apply error not reported: err=%v res=%+v", err, res)
	}
}

// ---- randomized invariants ----

// mem is a stateful pfSense stand-in with position-based IDs.
type mem struct{ items []pfsense.HostOverride }

func (m *mem) List(context.Context) ([]pfsense.HostOverride, error) {
	out := make([]pfsense.HostOverride, len(m.items))
	for i, h := range m.items {
		h.ID = i
		out[i] = h
	}
	return out, nil
}
func (m *mem) ListHostOverrides(ctx context.Context) ([]pfsense.HostOverride, error) {
	return m.List(ctx)
}
func (m *mem) GetHostOverride(_ context.Context, id int) (pfsense.HostOverride, error) {
	if id < 0 || id >= len(m.items) {
		return pfsense.HostOverride{}, fmt.Errorf("no override %d", id)
	}
	h := m.items[id]
	h.ID = id
	return h, nil
}
func (m *mem) CreateHostOverride(_ context.Context, h pfsense.HostOverride) error {
	m.items = append(m.items, h)
	return nil
}
func (m *mem) UpdateHostOverride(_ context.Context, h pfsense.HostOverride) error {
	m.items[h.ID] = h
	return nil
}
func (m *mem) DeleteHostOverride(_ context.Context, id int) error {
	m.items = append(m.items[:id], m.items[id+1:]...)
	return nil
}
func (m *mem) Apply(context.Context) error { return nil }

// TestRandomizedSafetyAndConvergence throws random pfSense contents and random desired
// states at the planner and checks the properties that must always hold:
//  1. entries without our tag are never modified or deleted (Adopt off),
//  2. executing a plan reaches the desired state for every name we are allowed to manage,
//  3. planning again afterwards yields nothing (idempotent).
func TestRandomizedSafetyAndConvergence(t *testing.T) {
	rng := rand.New(rand.NewSource(20261011))
	hosts := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	ips := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4"}
	pick := func() []string {
		n := 1 + rng.Intn(2)
		var out []string
		for i := 0; i < n; i++ {
			out = append(out, ips[rng.Intn(len(ips))])
		}
		return records.SortedUnique(out)
	}
	for iter := 0; iter < 2000; iter++ {
		adopt := rng.Intn(4) == 0
		prune := rng.Intn(4) != 0

		var cur []pfsense.HostOverride
		for _, h := range hosts {
			switch rng.Intn(4) {
			case 0: // managed
				cur = append(cur, pfsense.HostOverride{Host: h, Domain: "mylab.lan", IP: pick(), Descr: Marker(owner) + " src=x | note"})
			case 1: // unmanaged
				cur = append(cur, pfsense.HostOverride{Host: h, Domain: "mylab.lan", IP: pick(), Descr: fmt.Sprintf("manual %s", h)})
			}
		}
		want := records.Set{}
		for _, h := range hosts {
			if rng.Intn(2) == 0 {
				for _, ip := range pick() {
					want.Add(h, "mylab.lan", ip, "HTTPRoute/default/"+h)
				}
			}
		}

		m := &mem{items: append([]pfsense.HostOverride(nil), cur...)}
		before, _ := m.List(context.Background())
		opt := Options{Owner: owner, Prune: prune, Adopt: adopt}
		plan := BuildPlan(want, before, opt)
		if _, err := Execute(context.Background(), m, plan, logr.Discard()); err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}
		after, _ := m.List(context.Background())

		byName := func(l []pfsense.HostOverride) map[string]pfsense.HostOverride {
			out := map[string]pfsense.HostOverride{}
			for _, h := range l {
				out[h.FQDN()] = h
			}
			return out
		}
		a := byName(after)

		// 1. unmanaged entries survive untouched unless adoption was requested for that name
		for _, b := range before {
			if strings.HasPrefix(b.Descr, Marker(owner)) {
				continue
			}
			got, ok := a[b.FQDN()]
			if !ok {
				t.Fatalf("iter %d: unmanaged %s was deleted (opt=%+v)", iter, b.FQDN(), opt)
			}
			_, wanted := want[b.FQDN()]
			if !(adopt && wanted) && (got.Descr != b.Descr || !reflect.DeepEqual(got.IP, b.IP)) {
				t.Fatalf("iter %d: unmanaged %s was modified: %+v -> %+v (opt=%+v)", iter, b.FQDN(), b, got, opt)
			}
		}
		// 2. every desired name that is manageable resolves to the desired IPs
		for name, rec := range want {
			e, ok := a[name]
			if !ok {
				t.Fatalf("iter %d: %s missing after sync", iter, name)
			}
			manageable := strings.HasPrefix(e.Descr, Marker(owner))
			if manageable && !reflect.DeepEqual(records.SortedUnique(e.IP), rec.IPs) {
				t.Fatalf("iter %d: %s has %v want %v", iter, name, e.IP, rec.IPs)
			}
		}
		// pruning removes managed entries that are no longer wanted
		if prune {
			for _, e := range after {
				if _, wanted := want[e.FQDN()]; !wanted && strings.HasPrefix(e.Descr, Marker(owner)) {
					t.Fatalf("iter %d: stale managed %s survived pruning", iter, e.FQDN())
				}
			}
		}
		// 3. idempotent
		if again := BuildPlan(want, after, opt); len(again.Create)+len(again.Update)+len(again.Delete) != 0 {
			t.Fatalf("iter %d: second plan not empty: %+v (opt=%+v)", iter, again, opt)
		}
	}
}
