// SPDX-License-Identifier: Apache-2.0

package syncer

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/go-logr/logr"

	"github.com/mevijays/kube-pfsense-dns-controller/internal/pfsense"
	"github.com/mevijays/kube-pfsense-dns-controller/internal/records"
)

const owner = "k8s-dns-sync"

func desired(entries ...[3]string) records.Set { // host, domain, ip
	s := records.Set{}
	for _, e := range entries {
		s.Add(e[0], e[1], e[2], "HTTPRoute/default/x")
	}
	return s
}

func managed(id int, host, ip string) pfsense.HostOverride {
	return pfsense.HostOverride{ID: id, Host: host, Domain: "mylab.lan", IP: []string{ip}, Descr: Marker(owner) + " src=x"}
}

func TestCreateWhenMissing(t *testing.T) {
	p := BuildPlan(desired([3]string{"jenkins", "mylab.lan", "172.16.1.61"}), nil, Options{Owner: owner, Prune: true})
	if len(p.Create) != 1 || p.Create[0].FQDN() != "jenkins.mylab.lan" || p.Create[0].IP[0] != "172.16.1.61" {
		t.Fatalf("plan = %+v", p)
	}
	if len(p.Update)+len(p.Delete) != 0 {
		t.Fatalf("unexpected changes: %+v", p)
	}
}

func TestNoChangeWhenInSync(t *testing.T) {
	p := BuildPlan(desired([3]string{"jenkins", "mylab.lan", "172.16.1.61"}), []pfsense.HostOverride{managed(0, "jenkins", "172.16.1.61")}, Options{Owner: owner, Prune: true})
	if !p.Empty() {
		t.Fatalf("expected empty plan, got %+v", p)
	}
}

func TestUpdateWhenIPChanges(t *testing.T) {
	p := BuildPlan(desired([3]string{"jenkins", "mylab.lan", "172.16.1.63"}), []pfsense.HostOverride{managed(4, "jenkins", "172.16.1.61")}, Options{Owner: owner, Prune: true})
	if len(p.Update) != 1 || p.Update[0].New.ID != 4 || !reflect.DeepEqual(p.Update[0].New.IP, []string{"172.16.1.63"}) {
		t.Fatalf("plan = %+v", p)
	}
}

func TestNeverTouchesUnmanagedEntries(t *testing.T) {
	manual := pfsense.HostOverride{ID: 1, Host: "jenkins", Domain: "mylab.lan", IP: []string{"10.9.9.9"}, Descr: "made by hand"}
	other := pfsense.HostOverride{ID: 2, Host: "nas", Domain: "mylab.lan", IP: []string{"10.0.0.5"}, Descr: "my nas"}
	p := BuildPlan(desired([3]string{"jenkins", "mylab.lan", "172.16.1.61"}), []pfsense.HostOverride{manual, other}, Options{Owner: owner, Prune: true})
	if !p.Empty() {
		t.Fatalf("manual entries must not be changed or deleted: %+v", p)
	}
	if len(p.Conflicts) != 1 || p.Conflicts[0].Name != "jenkins.mylab.lan" || p.Conflicts[0].Same() {
		t.Fatalf("conflicts = %+v", p.Conflicts)
	}
}

func TestPrune(t *testing.T) {
	cur := []pfsense.HostOverride{managed(0, "gone", "172.16.1.61"), managed(1, "keep", "172.16.1.61"),
		{ID: 2, Host: "nas", Domain: "mylab.lan", IP: []string{"10.0.0.5"}, Descr: "my nas"}}
	want := desired([3]string{"keep", "mylab.lan", "172.16.1.61"})

	p := BuildPlan(want, cur, Options{Owner: owner, Prune: true})
	if len(p.Delete) != 1 || p.Delete[0].Host != "gone" {
		t.Fatalf("prune plan = %+v", p)
	}
	p = BuildPlan(want, cur, Options{Owner: owner, Prune: false})
	if len(p.Delete) != 0 {
		t.Fatalf("prune disabled but deletes planned: %+v", p)
	}
}

func TestDeclaredRecordWithoutIPKeepsItsEntry(t *testing.T) {
	s := records.Set{}
	s.Declare("pending", "mylab.lan", "HTTPRoute/default/x") // gateway has no address right now
	if p := BuildPlan(s, nil, Options{Owner: owner, Prune: true}); !p.Empty() {
		t.Fatalf("nothing to create without an address: %+v", p)
	}
	// An existing managed entry must be kept (not pruned) while the address is unknown.
	if p := BuildPlan(s, []pfsense.HostOverride{managed(0, "pending", "1.1.1.1")}, Options{Owner: owner, Prune: true}); !p.Empty() {
		t.Fatalf("declared hostname was pruned: %+v", p)
	}
	// Once the hostname is no longer declared at all, it is pruned.
	if p := BuildPlan(records.Set{}, []pfsense.HostOverride{managed(0, "pending", "1.1.1.1")}, Options{Owner: owner, Prune: true}); len(p.Delete) != 1 {
		t.Fatalf("undeclared hostname not pruned: %+v", p)
	}
}

type fakeBackend struct {
	ops     []string
	applied int
	byID    map[int]pfsense.HostOverride // what GetHostOverride returns
}

func (f *fakeBackend) GetHostOverride(_ context.Context, id int) (pfsense.HostOverride, error) {
	h, ok := f.byID[id]
	if !ok {
		return pfsense.HostOverride{}, fmt.Errorf("no override %d", id)
	}
	return h, nil
}

// store returns a map of the given entries by ID, for fakeBackend.byID.
func store(hs ...pfsense.HostOverride) map[int]pfsense.HostOverride {
	m := map[int]pfsense.HostOverride{}
	for _, h := range hs {
		m[h.ID] = h
	}
	return m
}

func (f *fakeBackend) ListHostOverrides(context.Context) ([]pfsense.HostOverride, error) {
	return nil, nil
}
func (f *fakeBackend) CreateHostOverride(_ context.Context, h pfsense.HostOverride) error {
	f.ops = append(f.ops, "create "+h.FQDN())
	return nil
}
func (f *fakeBackend) UpdateHostOverride(_ context.Context, h pfsense.HostOverride) error {
	f.ops = append(f.ops, "update "+h.FQDN())
	return nil
}
func (f *fakeBackend) DeleteHostOverride(_ context.Context, id int) error {
	f.ops = append(f.ops, "delete "+string(rune('0'+id)))
	return nil
}
func (f *fakeBackend) Apply(context.Context) error { f.applied++; return nil }

func TestExecuteOrderAndSingleApply(t *testing.T) {
	upd := pfsense.HostOverride{ID: 1, Host: "upd", Domain: "mylab.lan", IP: []string{"1.1.1.1"}, Descr: Marker(owner)}
	newUpd := upd
	newUpd.IP = []string{"2.2.2.2"}
	d2, d3, d5 := managed(2, "d2", "1.1.1.1"), managed(3, "d3", "1.1.1.1"), managed(5, "d5", "1.1.1.1")
	p := Plan{
		Create: []pfsense.HostOverride{{Host: "new", Domain: "mylab.lan"}},
		Update: []Update{{Old: upd, New: newUpd}},
		Delete: []pfsense.HostOverride{d2, d5, d3},
	}
	f := &fakeBackend{byID: store(upd, d2, d3, d5)}
	res, err := Execute(context.Background(), f, p, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"update upd.mylab.lan", "create new.mylab.lan", "delete 5", "delete 3", "delete 2"}
	if !reflect.DeepEqual(f.ops, want) {
		t.Errorf("ops = %v, want %v (deletes must run highest ID first)", f.ops, want)
	}
	if f.applied != 1 || !res.Changed || !res.Applied {
		t.Errorf("apply called %d times, result %+v", f.applied, res)
	}

	f = &fakeBackend{}
	if res, _ := Execute(context.Background(), f, Plan{}, logr.Discard()); f.applied != 0 || res.Changed {
		t.Error("apply called with nothing to do")
	}
}
