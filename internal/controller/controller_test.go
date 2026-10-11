// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/mevijays/kube-pfsense-dns-controller/internal/discover"
	"github.com/mevijays/kube-pfsense-dns-controller/internal/pfsense"
	"github.com/mevijays/kube-pfsense-dns-controller/internal/records"
)

// memPfSense is a stateful stand-in for pfSense. Like the real API, IDs are
// list positions, so they shift when an earlier entry is deleted.
type memPfSense struct {
	items   []pfsense.HostOverride
	applies int
}

func (m *memPfSense) ListHostOverrides(context.Context) ([]pfsense.HostOverride, error) {
	out := make([]pfsense.HostOverride, len(m.items))
	for i, h := range m.items {
		h.ID = i
		out[i] = h
	}
	return out, nil
}
func (m *memPfSense) CreateHostOverride(_ context.Context, h pfsense.HostOverride) error {
	m.items = append(m.items, h)
	return nil
}
func (m *memPfSense) UpdateHostOverride(_ context.Context, h pfsense.HostOverride) error {
	m.items[h.ID] = h
	return nil
}
func (m *memPfSense) DeleteHostOverride(_ context.Context, id int) error {
	m.items = append(m.items[:id], m.items[id+1:]...)
	return nil
}
func (m *memPfSense) Apply(context.Context) error { m.applies++; return nil }

func (m *memPfSense) state() map[string][]string {
	out := map[string][]string{}
	for _, h := range m.items {
		out[h.FQDN()] = h.IP
	}
	return out
}

func TestLifecycle(t *testing.T) {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = gatewayv1.Install(s)

	ipType := gatewayv1.IPAddressType
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "istio-system", Name: "shared-gw"},
		Status:     gatewayv1.GatewayStatus{Addresses: []gatewayv1.GatewayStatusAddress{{Type: &ipType, Value: "172.16.1.63"}}},
	}
	ns := gatewayv1.Namespace("istio-system")
	ref := gatewayv1.ParentReference{Name: "shared-gw", Namespace: &ns}
	route := func(name, host string) *gatewayv1.HTTPRoute {
		return &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
			Spec: gatewayv1.HTTPRouteSpec{
				CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{ref}},
				Hostnames:       []gatewayv1.Hostname{gatewayv1.Hostname(host)},
			},
			Status: gatewayv1.HTTPRouteStatus{RouteStatus: gatewayv1.RouteStatus{Parents: []gatewayv1.RouteParentStatus{{
				ParentRef:  ref,
				Conditions: []metav1.Condition{{Type: string(gatewayv1.RouteConditionAccepted), Status: metav1.ConditionTrue}},
			}}}},
		}
	}
	r1, r2 := route("one", "one.mylab.lan"), route("two", "two.mylab.lan")

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(gw, r1, r2).Build()
	handBuilt := pfsense.HostOverride{Host: "nas", Domain: "mylab.lan", IP: []string{"10.0.0.5"}, Descr: "mine"}
	pf := &memPfSense{items: []pfsense.HostOverride{handBuilt}}
	rec := &Reconciler{
		Discoverer: &discover.Discoverer{Client: c, APIReader: c, Domains: records.ParseDomains("mylab.lan"),
			EnableGatewayAPI: true, Log: logr.Discard()},
		Backend: pf, Owner: "k8s-dns-sync", Prune: true, Log: logr.Discard(),
	}
	ctx := context.Background()
	sync := func() {
		t.Helper()
		if err := rec.Sync(ctx); err != nil {
			t.Fatal(err)
		}
	}
	names := func() []string {
		var n []string
		for k := range pf.state() {
			n = append(n, k)
		}
		sort.Strings(n)
		return n
	}

	// 1. creates both records, leaves the hand-built one alone, applies once
	sync()
	if !reflect.DeepEqual(names(), []string{"nas.mylab.lan", "one.mylab.lan", "two.mylab.lan"}) || pf.applies != 1 {
		t.Fatalf("after create: %v applies=%d", pf.state(), pf.applies)
	}

	// 2. second run changes nothing and does not call apply again
	sync()
	if pf.applies != 1 {
		t.Fatalf("idle sync applied again: %d", pf.applies)
	}

	// 3. gateway gets a new IP -> both records are updated
	gw.Status.Addresses[0].Value = "172.16.1.70"
	if err := c.Update(ctx, gw); err != nil { // fake client has no status subresource, Update writes status too
		t.Fatal(err)
	}
	sync()
	if got := pf.state()["one.mylab.lan"]; !reflect.DeepEqual(got, []string{"172.16.1.70"}) {
		t.Fatalf("IP not updated: %v", pf.state())
	}

	// 4. route deleted -> only its record is removed; the hand-built entry survives
	if err := c.Delete(ctx, client.Object(r1)); err != nil {
		t.Fatal(err)
	}
	sync()
	if !reflect.DeepEqual(names(), []string{"nas.mylab.lan", "two.mylab.lan"}) {
		t.Fatalf("after delete: %v", pf.state())
	}
	if pf.state()["nas.mylab.lan"][0] != "10.0.0.5" {
		t.Fatal("hand-built entry was modified")
	}
}
