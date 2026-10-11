// SPDX-License-Identifier: Apache-2.0

package syncer

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/go-logr/logr"

	"github.com/mevijays/kube-pfsense-dns-controller/internal/pfsense"
	"github.com/mevijays/kube-pfsense-dns-controller/internal/records"
)

func TestIsManagedRequiresExactOwner(t *testing.T) {
	cases := []struct {
		descr string
		want  bool
	}{
		{"managed-by=k8s-dns-sync", true},
		{"managed-by=k8s-dns-sync src=HTTPRoute/a/b", true},
		{"managed-by=k8s-dns-sync src=x | note", true},
		{"managed-by=k8s-dns-sync-b src=HTTPRoute/a/b", false}, // another owner sharing the prefix
		{"managed-by=k8s-dns-syncX", false},
		{"my note managed-by=k8s-dns-sync", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsManaged(c.descr, "k8s-dns-sync"); got != c.want {
			t.Errorf("IsManaged(%q) = %v, want %v", c.descr, got, c.want)
		}
	}
}

// Two clusters sharing one pfSense, with owner IDs where one is a prefix of the other,
// must not prune or update each other's records.
func TestOwnersWithSharedPrefixDoNotTouchEachOther(t *testing.T) {
	other := pfsense.HostOverride{ID: 0, Host: "b-app", Domain: "mylab.lan", IP: []string{"10.0.0.9"}, Descr: Marker(owner+"-b") + " src=HTTPRoute/x/y"}
	p := BuildPlan(records.Set{}, []pfsense.HostOverride{other}, Options{Owner: owner, Prune: true})
	if !p.Empty() {
		t.Fatalf("owner %q planned changes to owner %q's record: %+v", owner, owner+"-b", p)
	}
	// and a same-name desired record treats it as a conflict, not as ours
	p = BuildPlan(desired([3]string{"b-app", "mylab.lan", "10.0.0.1"}), []pfsense.HostOverride{other}, Options{Owner: owner, Prune: true})
	if !p.Empty() || len(p.Conflicts) != 1 {
		t.Fatalf("plan = %+v", p)
	}
}

func TestEquivalentIPFormsDoNotCauseUpdates(t *testing.T) {
	cur := []pfsense.HostOverride{{ID: 0, Host: "v6", Domain: "mylab.lan", IP: []string{"2001:DB8:0:0::0001"}, Descr: Marker(owner)}}
	want := desired([3]string{"v6", "mylab.lan", "2001:db8::1"})
	if p := BuildPlan(want, cur, Options{Owner: owner, Prune: true}); !p.Empty() {
		t.Fatalf("non-canonical but equal IPv6 caused a change: %+v", p)
	}
	c := Conflict{Existing: []string{"2001:DB8::1"}, Desired: []string{"2001:db8::1"}}
	if !c.Same() {
		t.Error("Conflict.Same must compare canonical addresses")
	}
}

func TestDescriptionTruncatesOnRuneBoundary(t *testing.T) {
	r := &records.Record{Host: "x", Domain: "mylab.lan", IPs: []string{"1.1.1.1"}, Sources: []string{"HTTPRoute/a/b"}}
	// Shift the multi-byte text by 0..3 ASCII characters so that, whatever the
	// prefix length, a byte-based cut would land inside a character at least once.
	for shift := 0; shift < 4; shift++ {
		note := strings.Repeat("a", shift) + strings.Repeat("नमस्ते🙂", 400) // well over the limit
		d := description(owner, r, note)
		if !utf8.ValidString(d) {
			t.Fatalf("shift %d: description is not valid UTF-8 after truncation", shift)
		}
		if n := utf8.RuneCountInString(d); n != maxDescr {
			t.Fatalf("shift %d: description has %d characters, want exactly the limit %d", shift, n, maxDescr)
		}
	}
	if got := truncateRunes("héllo", 2); got != "hé" {
		t.Fatalf("truncateRunes = %q", got)
	}
}

// ---- stale IDs / concurrent changes ----

// shifting simulates another writer: right after our List, it deletes the entry at
// removeAt, so every later ID shifts down by one.
type shifting struct {
	mem
	removeAt int
	removed  bool
}

func (s *shifting) ListHostOverrides(ctx context.Context) ([]pfsense.HostOverride, error) {
	return s.mem.List(ctx)
}

func (s *shifting) concurrentDelete() {
	if !s.removed {
		s.items = append(s.items[:s.removeAt], s.items[s.removeAt+1:]...)
		s.removed = true
	}
}

func TestConcurrentChangeNeverHitsAnotherRecord(t *testing.T) {
	items := []pfsense.HostOverride{
		{Host: "first", Domain: "mylab.lan", IP: []string{"10.0.0.1"}, Descr: "hand made"},
		{Host: "stale", Domain: "mylab.lan", IP: []string{"10.0.0.2"}, Descr: Marker(owner) + " src=x"}, // ours, to prune
		{Host: "nas", Domain: "mylab.lan", IP: []string{"10.0.0.5"}, Descr: "my nas"},                   // hand made
		{Host: "move", Domain: "mylab.lan", IP: []string{"10.0.0.3"}, Descr: Marker(owner) + " src=x"},  // ours, IP change
	}
	s := &shifting{mem: mem{items: append([]pfsense.HostOverride(nil), items...)}, removeAt: 0}
	before, _ := s.ListHostOverrides(context.Background())
	plan := BuildPlan(desired([3]string{"move", "mylab.lan", "10.0.0.9"}), before, Options{Owner: owner, Prune: true})
	if len(plan.Update) != 1 || len(plan.Delete) != 1 {
		t.Fatalf("setup: %+v", plan)
	}

	s.concurrentDelete() // someone removes "first" in the UI: every ID shifts down by one

	_, err := Execute(context.Background(), s, plan, logr.Discard())
	if !errors.Is(err, ErrConcurrentChange) {
		t.Fatalf("expected ErrConcurrentChange, got %v", err)
	}
	// Without the check, PATCH id 3 / DELETE id 1 would now hit "nas" and "move".
	after, _ := s.List(context.Background())
	got := map[string]pfsense.HostOverride{}
	for _, h := range after {
		got[h.FQDN()] = h
	}
	if n := got["nas.mylab.lan"]; n.Descr != "my nas" || n.IP[0] != "10.0.0.5" {
		t.Fatalf("hand-made record was modified: %+v", n)
	}
	if len(after) != 3 {
		t.Fatalf("a record was deleted by a stale ID: %+v", after)
	}

	// The next sync re-plans from fresh data and converges.
	fresh, _ := s.ListHostOverrides(context.Background())
	plan = BuildPlan(desired([3]string{"move", "mylab.lan", "10.0.0.9"}), fresh, Options{Owner: owner, Prune: true})
	if _, err := Execute(context.Background(), s, plan, logr.Discard()); err != nil {
		t.Fatal(err)
	}
	final, _ := s.List(context.Background())
	names := []string{}
	for _, h := range final {
		names = append(names, h.FQDN()+"="+strings.Join(h.IP, ","))
	}
	if !reflect.DeepEqual(names, []string{"nas.mylab.lan=10.0.0.5", "move.mylab.lan=10.0.0.9"}) {
		t.Fatalf("did not converge: %v", names)
	}
}

func TestVerifyStopsDeletesOnMismatch(t *testing.T) {
	d1, d2 := managed(1, "a", "1.1.1.1"), managed(2, "b", "1.1.1.1")
	changed := d2
	changed.Host = "someone-else"
	f := &fakeBackend{byID: store(d1, changed)}
	_, err := Execute(context.Background(), f, Plan{Delete: []pfsense.HostOverride{d1, d2}}, logr.Discard())
	if !errors.Is(err, ErrConcurrentChange) {
		t.Fatalf("err = %v", err)
	}
	if len(f.ops) != 0 {
		t.Fatalf("deletes ran after a mismatch: %v", f.ops)
	}
}

// ---- apply survives cancellation ----

type ctxCheckingBackend struct{ fakeBackend }

func (c *ctxCheckingBackend) Apply(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.applied++
	return nil
}

func TestApplyRunsEvenIfContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the pod is shutting down
	b := &ctxCheckingBackend{}
	if err := Apply(ctx, b); err != nil || b.applied != 1 {
		t.Fatalf("apply did not run on a cancelled context: err=%v applied=%d", err, b.applied)
	}
}
