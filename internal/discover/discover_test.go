// SPDX-License-Identifier: Apache-2.0

package discover

import (
	"context"
	"reflect"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/mevijays/kube-pfsense-dns-controller/internal/records"
)

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := gatewayv1.Install(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func istioObj(kind, ns, name string, spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	u.SetGroupVersionKind(VirtualServiceGVK)
	u.SetKind(kind)
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}

func ipType() *gatewayv1.AddressType { t := gatewayv1.IPAddressType; return &t }

func httpRoute(ns, name, gw, gwNS string, hostnames []string, accepted bool) *gatewayv1.HTTPRoute {
	gns := gatewayv1.Namespace(gwNS)
	var hs []gatewayv1.Hostname
	for _, h := range hostnames {
		hs = append(hs, gatewayv1.Hostname(h))
	}
	status := metav1.ConditionFalse
	if accepted {
		status = metav1.ConditionTrue
	}
	ref := gatewayv1.ParentReference{Name: gatewayv1.ObjectName(gw), Namespace: &gns}
	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{ref}}, Hostnames: hs},
		Status: gatewayv1.HTTPRouteStatus{RouteStatus: gatewayv1.RouteStatus{Parents: []gatewayv1.RouteParentStatus{{
			ParentRef:  ref,
			Conditions: []metav1.Condition{{Type: string(gatewayv1.RouteConditionAccepted), Status: status}},
		}}}},
	}
}

func newDiscoverer(t *testing.T, objs ...client.Object) *Discoverer {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).Build()
	return &Discoverer{
		Client: c, APIReader: c,
		Domains:         records.ParseDomains("mylab.lan"),
		IstioNamespaces: []string{"istio-system"},
		EnableIstio:     true, EnableGatewayAPI: true,
		Log: logr.Discard(),
	}
}

func TestHTTPRouteUsesGatewayAddress(t *testing.T) {
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "istio-system", Name: "shared-gw"},
		Status:     gatewayv1.GatewayStatus{Addresses: []gatewayv1.GatewayStatusAddress{{Type: ipType(), Value: "172.16.1.63"}}},
	}
	d := newDiscoverer(t, gw,
		httpRoute("default", "ok", "shared-gw", "istio-system", []string{"app2.mylab.lan", "other.example.com", "*.mylab.lan"}, true),
		httpRoute("default", "rejected", "shared-gw", "istio-system", []string{"nope.mylab.lan"}, false),
	)
	got, err := d.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// the rejected route's hostname is declared without an address (kept, never created)
	if len(got) != 2 || len(got["nope.mylab.lan"].IPs) != 0 {
		t.Fatalf("want app2.mylab.lan and an address-less nope.mylab.lan, got %v", got)
	}
	r := got["app2.mylab.lan"]
	if r == nil || !reflect.DeepEqual(r.IPs, []string{"172.16.1.63"}) || r.Sources[0] != "HTTPRoute/default/ok" {
		t.Fatalf("record = %+v", r)
	}
}

func TestVirtualServiceUsesIngressGatewayServiceIP(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "istio-system", Name: "igw-1",
		Labels: map[string]string{"app": "istio-ingressgateway", "istio": "ingressgateway"}}}
	lb := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "istio-system", Name: "istio-ingressgateway"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, Selector: map[string]string{"app": "istio-ingressgateway", "istio": "ingressgateway"}},
		Status:     corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: "172.16.1.61"}}}},
	}
	// A second Service of a different workload must be ignored.
	other := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "istio-system", Name: "other-lb"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, Selector: map[string]string{"app": "other"}},
		Status:     corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: "9.9.9.9"}}}},
	}
	gw := istioObj("Gateway", "istio-system", "shared-istio-gw", map[string]any{"selector": map[string]any{"istio": "ingressgateway"}})
	vs := istioObj("VirtualService", "devops", "jenkins", map[string]any{
		"hosts":    []any{"jenkins.mylab.lan"},
		"gateways": []any{"istio-system/shared-istio-gw", "mesh"},
	})
	// Relative gateway reference + a host outside the managed domain.
	gw2 := istioObj("Gateway", "default", "local-gw", map[string]any{"selector": map[string]any{"istio": "ingressgateway"}})
	vs2 := istioObj("VirtualService", "default", "app1", map[string]any{
		"hosts":    []any{"app1.mylab.lan", "app1.example.com"},
		"gateways": []any{"local-gw"},
	})

	d := newDiscoverer(t, pod, lb, other, gw, vs, gw2, vs2)
	got, err := d.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"jenkins.mylab.lan", "app1.mylab.lan"} {
		r := got[name]
		if r == nil || !reflect.DeepEqual(r.IPs, []string{"172.16.1.61"}) {
			t.Errorf("%s = %+v", name, r)
		}
	}
	if len(got) != 2 {
		t.Errorf("unexpected records: %v", got)
	}
}

func TestSameHostOnTwoGatewaysGetsBothIPs(t *testing.T) {
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "istio-system", Name: "shared-gw"},
		Status:     gatewayv1.GatewayStatus{Addresses: []gatewayv1.GatewayStatusAddress{{Type: ipType(), Value: "172.16.1.63"}}},
	}
	cgw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "cilium-shared-gw"},
		Status:     gatewayv1.GatewayStatus{Addresses: []gatewayv1.GatewayStatusAddress{{Type: ipType(), Value: "172.16.1.64"}}},
	}
	r := httpRoute("default", "both", "shared-gw", "istio-system", []string{"both.mylab.lan"}, true)
	cns := gatewayv1.Namespace("kube-system")
	ref2 := gatewayv1.ParentReference{Name: "cilium-shared-gw", Namespace: &cns}
	r.Spec.ParentRefs = append(r.Spec.ParentRefs, ref2)
	r.Status.Parents = append(r.Status.Parents, gatewayv1.RouteParentStatus{ParentRef: ref2,
		Conditions: []metav1.Condition{{Type: string(gatewayv1.RouteConditionAccepted), Status: metav1.ConditionTrue}}})

	got, err := newDiscoverer(t, gw, cgw, r).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got["both.mylab.lan"].IPs, []string{"172.16.1.63", "172.16.1.64"}) {
		t.Fatalf("got %+v", got["both.mylab.lan"])
	}
}

func TestParseGatewayRef(t *testing.T) {
	cases := []struct {
		in, ns, name string
		ok           bool
	}{
		{"mesh", "", "", false},
		{"gw", "def", "gw", true},
		{"istio-system/gw", "istio-system", "gw", true},
		{"gw.istio-system.svc.cluster.local", "istio-system", "gw", true},
	}
	for _, c := range cases {
		ns, name, ok := parseGatewayRef(c.in, "def")
		if ns != c.ns || name != c.name || ok != c.ok {
			t.Errorf("parseGatewayRef(%q) = %q,%q,%v", c.in, ns, name, ok)
		}
	}
}
