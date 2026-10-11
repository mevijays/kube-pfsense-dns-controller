// SPDX-License-Identifier: Apache-2.0

package syncer

import (
	"context"
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
	p := BuildPlan(desired([3]string{"jenkins", "mylab.lan", "172.16.1.61"}), nil, owner, true)
	if len(p.Create) != 1 || p.Create[0].FQDN() != "jenkins.mylab.lan" || p.Create[0].IP[0] != "172.16.1.61" {
		t.Fatalf("plan = %+v", p)
	}
	if len(p.Update)+len(p.Delete) != 0 {
		t.Fatalf("unexpected changes: %+v", p)
	}
}

func TestNoChangeWhenInSync(t *testing.T) {
	p := BuildPlan(desired([3]string{"jenkins", "mylab.lan", "172.16.1.61"}), []pfsense.HostOverride{managed(0, "jenkins", "172.16.1.61")}, owner, true)
	if !p.Empty() {
		t.Fatalf("expected empty plan, got %+v", p)
	}
}

func TestUpdateWhenIPChanges(t *testing.T) {
	p := BuildPlan(desired([3]string{"jenkins", "mylab.lan", "172.16.1.63"}), []pfsense.HostOverride{managed(4, "jenkins", "172.16.1.61")}, owner, true)
	if len(p.Update) != 1 || p.Update[0].ID != 4 || !reflect.DeepEqual(p.Update[0].IP, []string{"172.16.1.63"}) {
		t.Fatalf("plan = %+v", p)
	}
}

func TestNeverTouchesUnmanagedEntries(t *testing.T) {
	manual := pfsense.HostOverride{ID: 1, Host: "jenkins", Domain: "mylab.lan", IP: []string{"10.9.9.9"}, Descr: "made by hand"}
	other := pfsense.HostOverride{ID: 2, Host: "nas", Domain: "mylab.lan", IP: []string{"10.0.0.5"}, Descr: "my nas"}
	p := BuildPlan(desired([3]string{"jenkins", "mylab.lan", "172.16.1.61"}), []pfsense.HostOverride{manual, other}, owner, true)
	if !p.Empty() {
		t.Fatalf("manual entries must not be changed or deleted: %+v", p)
	}
	if !reflect.DeepEqual(p.Conflicts, []string{"jenkins.mylab.lan"}) {
		t.Fatalf("conflicts = %v", p.Conflicts)
	}
}

func TestPrune(t *testing.T) {
	cur := []pfsense.HostOverride{managed(0, "gone", "172.16.1.61"), managed(1, "keep", "172.16.1.61"),
		{ID: 2, Host: "nas", Domain: "mylab.lan", IP: []string{"10.0.0.5"}, Descr: "my nas"}}
	want := desired([3]string{"keep", "mylab.lan", "172.16.1.61"})

	p := BuildPlan(want, cur, owner, true)
	if len(p.Delete) != 1 || p.Delete[0].Host != "gone" {
		t.Fatalf("prune plan = %+v", p)
	}
	p = BuildPlan(want, cur, owner, false)
	if len(p.Delete) != 0 {
		t.Fatalf("prune disabled but deletes planned: %+v", p)
	}
}

func TestSkipsRecordsWithoutIP(t *testing.T) {
	s := records.Set{}
	s.Add("pending", "mylab.lan", "", "HTTPRoute/default/x") // gateway has no address yet
	p := BuildPlan(s, nil, owner, true)
	if !p.Empty() {
		t.Fatalf("plan = %+v", p)
	}
	// ...and an existing managed entry for it is pruned, not kept.
	p = BuildPlan(s, []pfsense.HostOverride{managed(0, "pending", "1.1.1.1")}, owner, true)
	if len(p.Delete) != 1 {
		t.Fatalf("plan = %+v", p)
	}
}

type fakeBackend struct {
	ops     []string
	applied int
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
	p := Plan{
		Create: []pfsense.HostOverride{{Host: "new", Domain: "mylab.lan"}},
		Update: []pfsense.HostOverride{{ID: 1, Host: "upd", Domain: "mylab.lan"}},
		Delete: []pfsense.HostOverride{{ID: 2}, {ID: 5}, {ID: 3}},
	}
	f := &fakeBackend{}
	if err := Execute(context.Background(), f, p, logr.Discard()); err != nil {
		t.Fatal(err)
	}
	want := []string{"update upd.mylab.lan", "create new.mylab.lan", "delete 5", "delete 3", "delete 2"}
	if !reflect.DeepEqual(f.ops, want) {
		t.Errorf("ops = %v, want %v (deletes must run highest ID first)", f.ops, want)
	}
	if f.applied != 1 {
		t.Errorf("apply called %d times", f.applied)
	}

	f = &fakeBackend{}
	_ = Execute(context.Background(), f, Plan{}, logr.Discard())
	if f.applied != 0 {
		t.Error("apply called with nothing to do")
	}
}
