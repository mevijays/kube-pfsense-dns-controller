// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/mevijays/kube-pfsense-dns-controller/internal/discover"
	"github.com/mevijays/kube-pfsense-dns-controller/internal/metrics"
	"github.com/mevijays/kube-pfsense-dns-controller/internal/pfsense"
	"github.com/mevijays/kube-pfsense-dns-controller/internal/records"
	"github.com/mevijays/kube-pfsense-dns-controller/internal/syncer"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = gatewayv1.Install(s)
	return s
}

func fixture() (*gatewayv1.Gateway, *gatewayv1.HTTPRoute) {
	t := gatewayv1.IPAddressType
	ns := gatewayv1.Namespace("istio-system")
	ref := gatewayv1.ParentReference{Name: "shared-gw", Namespace: &ns}
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "istio-system", Name: "shared-gw"},
		Status:     gatewayv1.GatewayStatus{Addresses: []gatewayv1.GatewayStatusAddress{{Type: &t, Value: "172.16.1.63"}}},
	}
	rt := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "app2"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{ref}},
			Hostnames:       []gatewayv1.Hostname{"app2.mylab.lan"},
		},
		Status: gatewayv1.HTTPRouteStatus{RouteStatus: gatewayv1.RouteStatus{Parents: []gatewayv1.RouteParentStatus{{
			ParentRef:  ref,
			Conditions: []metav1.Condition{{Type: string(gatewayv1.RouteConditionAccepted), Status: metav1.ConditionTrue}},
		}}}},
	}
	return gw, rt
}

func newRec(c client.Client, b pfsense.Backend, adopt bool) *Reconciler {
	return &Reconciler{
		Discoverer: &discover.Discoverer{Client: c, APIReader: c, Domains: records.ParseDomains("mylab.lan"), EnableGatewayAPI: true, Log: logr.Discard()},
		Backend:    b, Owner: "k8s-dns-sync", Prune: true, Adopt: adopt, Log: logr.Discard(),
	}
}

// A discovery error (apiserver hiccup) must never lead to deleting DNS records.
func TestDiscoveryErrorDisablesPruning(t *testing.T) {
	gw, rt := fixture()
	var failGets bool
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(gw, rt).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*gatewayv1.Gateway); ok && failGets {
				return errors.New("apiserver unavailable")
			}
			return cl.Get(ctx, key, obj, opts...)
		}}).Build()
	pf := &memPfSense{}
	r := newRec(c, pf, false)
	if err := r.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := pf.state(); !reflect.DeepEqual(got, map[string][]string{"app2.mylab.lan": {"172.16.1.63"}}) {
		t.Fatalf("setup: %v", got)
	}

	failGets = true
	err := r.Sync(context.Background())
	if err == nil {
		t.Fatal("expected the discovery error to be returned so the work item is retried")
	}
	if len(pf.state()) != 1 {
		t.Fatalf("records were deleted during a discovery failure: %v", pf.state())
	}
	failGets = false
	if err := r.Sync(context.Background()); err != nil || len(pf.state()) != 1 {
		t.Fatalf("recovery failed: err=%v state=%v", err, pf.state())
	}
}

type failingList struct{ memPfSense }

func (f *failingList) ListHostOverrides(context.Context) ([]pfsense.HostOverride, error) {
	return nil, errors.New("pfsense unreachable")
}

func TestBackendDownIsAnErrorAndChangesNothing(t *testing.T) {
	gw, rt := fixture()
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(gw, rt).Build()
	pf := &failingList{}
	before := testutil.ToFloat64(metrics.Syncs.WithLabelValues("error"))
	err := newRec(c, pf, false).Sync(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pfsense unreachable") {
		t.Fatalf("err = %v", err)
	}
	if len(pf.items) != 0 {
		t.Fatal("wrote to a backend that could not be read")
	}
	if testutil.ToFloat64(metrics.Syncs.WithLabelValues("error")) != before+1 {
		t.Error("error counter not incremented")
	}
}

func TestHandMadeEntriesAndConflictsAreLeftAlone(t *testing.T) {
	gw, rt := fixture()
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(gw, rt).Build()
	// pfSense already has app2 pointing elsewhere (like vault.mylab.lan on the real firewall)
	hand := pfsense.HostOverride{Host: "app2", Domain: "mylab.lan", IP: []string{"172.16.1.61"}, Descr: "vault k8s"}
	pf := &memPfSense{items: []pfsense.HostOverride{hand}}
	r := newRec(c, pf, false)
	if err := r.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pf.items, []pfsense.HostOverride{hand}) || pf.applies != 0 {
		t.Fatalf("unmanaged entry touched: %+v applies=%d", pf.items, pf.applies)
	}
	if testutil.ToFloat64(metrics.Conflicts) != 1 {
		t.Errorf("conflict gauge = %v", testutil.ToFloat64(metrics.Conflicts))
	}

	// with adoption on, the entry is taken over, IP corrected, original note kept
	r.Adopt = true
	if err := r.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := pf.items[0]
	if !reflect.DeepEqual(got.IP, []string{"172.16.1.63"}) || !strings.HasPrefix(got.Descr, syncer.Marker("k8s-dns-sync")) || !strings.HasSuffix(got.Descr, "| vault k8s") {
		t.Fatalf("adopted entry = %+v", got)
	}
	if testutil.ToFloat64(metrics.Conflicts) != 0 {
		t.Errorf("conflict gauge after adoption = %v", testutil.ToFloat64(metrics.Conflicts))
	}
}

func TestMetricsReflectWork(t *testing.T) {
	gw, rt := fixture()
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(gw, rt).Build()
	pf := &memPfSense{}
	created := testutil.ToFloat64(metrics.Changes.WithLabelValues("create"))
	ok := testutil.ToFloat64(metrics.Syncs.WithLabelValues("success"))
	if err := newRec(c, pf, false).Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if testutil.ToFloat64(metrics.Changes.WithLabelValues("create")) != created+1 ||
		testutil.ToFloat64(metrics.Syncs.WithLabelValues("success")) != ok+1 ||
		testutil.ToFloat64(metrics.DesiredRecords) != 1 ||
		testutil.ToFloat64(metrics.LastSuccess) == 0 {
		t.Fatal("metrics did not reflect the sync")
	}
}

// Many concurrent triggers (events + the periodic loop) must serialise cleanly.
func TestConcurrentSyncsAreSerialised(t *testing.T) {
	gw, rt := fixture()
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(gw, rt).Build()
	pf := &memPfSense{}
	r := newRec(c, pf, false)
	done := make(chan error, 20)
	for i := 0; i < 20; i++ {
		go func() { done <- r.Sync(context.Background()) }()
	}
	for i := 0; i < 20; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if len(pf.items) != 1 || pf.applies != 1 {
		t.Fatalf("expected exactly one create and one apply, got %d items, %d applies", len(pf.items), pf.applies)
	}
}
