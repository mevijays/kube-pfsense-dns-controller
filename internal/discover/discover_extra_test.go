// SPDX-License-Identifier: Apache-2.0

package discover

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/mevijays/kube-pfsense-dns-controller/internal/records"
)

func gwWithAddr(ns, name, addr string, typ gatewayv1.AddressType) *gatewayv1.Gateway {
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status:     gatewayv1.GatewayStatus{Addresses: []gatewayv1.GatewayStatusAddress{{Type: &typ, Value: addr}}},
	}
}

func lbPodAndSvc(ns string, labels map[string]string, ip string) (*corev1.Pod, *corev1.Service) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "p-" + ip, Labels: labels}}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "lb-" + ip},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, Selector: labels},
	}
	if ip != "" {
		svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: ip}}
	}
	return pod, svc
}

func TestHTTPRouteEdgeCases(t *testing.T) {
	k := gatewayv1.Kind("Service")
	g := gatewayv1.Group("example.com")
	ipv6 := gwWithAddr("istio-system", "v6gw", "2001:db8::1", gatewayv1.IPAddressType)
	hostGW := gwWithAddr("istio-system", "hostgw", "gw.example.net", gatewayv1.HostnameAddressType)
	okGW := gwWithAddr("istio-system", "ok", "10.1.1.1", gatewayv1.IPAddressType)
	noAddr := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "istio-system", Name: "pending"}}

	routes := []client.Object{
		httpRoute("d", "v6", "v6gw", "istio-system", []string{"v6.mylab.lan"}, true),
		httpRoute("d", "hostaddr", "hostgw", "istio-system", []string{"hostaddr.mylab.lan"}, true),
		httpRoute("d", "pending", "pending", "istio-system", []string{"pending.mylab.lan"}, true),
		httpRoute("d", "missing-gw", "does-not-exist", "istio-system", []string{"missing.mylab.lan"}, true),
		httpRoute("d", "nohosts", "ok", "istio-system", nil, true),
		httpRoute("d", "good", "ok", "istio-system", []string{"GOOD.MyLab.LAN"}, true),
	}
	// route whose parentRef is not a Gateway
	odd := httpRoute("d", "odd-kind", "ok", "istio-system", []string{"odd.mylab.lan"}, true)
	odd.Spec.ParentRefs[0].Kind = &k
	odd.Spec.ParentRefs[0].Group = &g
	// deleted objects are ignored (fake client needs a finalizer to keep them around)
	dying := httpRoute("d", "dying", "ok", "istio-system", []string{"dying.mylab.lan"}, true)
	now := metav1.Now()
	dying.DeletionTimestamp = &now
	dying.Finalizers = []string{"x"}

	d := newDiscoverer(t, append(routes, ipv6, hostGW, okGW, noAddr, odd, dying)...)
	got, err := d.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	names := map[string][]string{}
	for n, r := range got {
		names[n] = r.IPs
	}
	want := map[string][]string{
		"v6.mylab.lan":   {"2001:db8::1"}, // IPv6 passes through
		"good.mylab.lan": {"10.1.1.1"},    // hostname is lower-cased
		// pending (no address), missing gateway, no hostnames, hostname-type address,
		// non-Gateway parent and deleting routes must all produce nothing
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("got %v, want %v", names, want)
	}
}

func TestVirtualServiceEdgeCases(t *testing.T) {
	labels := map[string]string{"istio": "ingressgateway"}
	pod, svc := lbPodAndSvc("istio-system", labels, "172.16.1.61")
	// LB Service that has not been given an IP yet
	pod2, svcPending := lbPodAndSvc("istio-system", map[string]string{"istio": "other"}, "")
	pod2.Name, svcPending.Name = "p-pending", "lb-pending"
	// a same-labelled pod in a namespace we are not configured to search
	podElsewhere, svcElsewhere := lbPodAndSvc("other-ns", labels, "9.9.9.9")
	// a ClusterIP service selecting the same pods must not be used
	cip := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "istio-system", Name: "cip"},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Selector: labels}}

	gw := istioObj("Gateway", "istio-system", "gw", map[string]any{"selector": map[string]any{"istio": "ingressgateway"}})
	gwPending := istioObj("Gateway", "istio-system", "gw-pending", map[string]any{"selector": map[string]any{"istio": "other"}})
	gwNoSel := istioObj("Gateway", "istio-system", "gw-nosel", map[string]any{})
	gwNoPods := istioObj("Gateway", "istio-system", "gw-nopods", map[string]any{"selector": map[string]any{"istio": "nothing"}})

	vs := func(name string, hosts, gws []any) *unstructured.Unstructured {
		return istioObj("VirtualService", "default", name, map[string]any{"hosts": hosts, "gateways": gws})
	}
	objs := []client.Object{pod, svc, pod2, svcPending, podElsewhere, svcElsewhere, cip, gw, gwPending, gwNoSel, gwNoPods,
		vs("ok", []any{"ok.mylab.lan", "short"}, []any{"istio-system/gw"}),
		vs("fqdn-ref", []any{"fqdn.mylab.lan"}, []any{"gw.istio-system.svc.cluster.local"}),
		vs("mesh-only", []any{"mesh.mylab.lan"}, []any{"mesh"}),
		vs("lb-pending", []any{"pending.mylab.lan"}, []any{"istio-system/gw-pending"}),
		vs("no-selector", []any{"nosel.mylab.lan"}, []any{"istio-system/gw-nosel"}),
		vs("no-pods", []any{"nopods.mylab.lan"}, []any{"istio-system/gw-nopods"}),
		vs("missing-gw", []any{"missing.mylab.lan"}, []any{"istio-system/nope"}),
		vs("no-gateways", []any{"nogw.mylab.lan"}, nil),
		vs("wildcard", []any{"*.mylab.lan"}, []any{"istio-system/gw"}),
	}
	d := newDiscoverer(t, objs...)
	got, err := d.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	names := map[string][]string{}
	for n, r := range got {
		names[n] = r.IPs
	}
	want := map[string][]string{
		"ok.mylab.lan":   {"172.16.1.61"},
		"fqdn.mylab.lan": {"172.16.1.61"},
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("got %v, want %v", names, want)
	}
}

func TestSourcesCanBeDisabled(t *testing.T) {
	gw := gwWithAddr("istio-system", "ok", "10.1.1.1", gatewayv1.IPAddressType)
	r := httpRoute("d", "r", "ok", "istio-system", []string{"r.mylab.lan"}, true)
	d := newDiscoverer(t, gw, r)
	d.EnableGatewayAPI = false
	if got, err := d.Discover(context.Background()); err != nil || len(got) != 0 {
		t.Fatalf("gateway api disabled: got=%v err=%v", got, err)
	}
}

func TestAPIErrorsAreReportedNotSwallowed(t *testing.T) {
	boom := errors.New("apiserver unavailable")
	gw := gwWithAddr("istio-system", "ok", "10.1.1.1", gatewayv1.IPAddressType)
	r := httpRoute("d", "r", "ok", "istio-system", []string{"r.mylab.lan"}, true)

	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(gw, r).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*gatewayv1.Gateway); ok {
				return boom
			}
			return cl.Get(ctx, key, obj, opts...)
		}}).Build()
	d := &Discoverer{Client: c, APIReader: c, Domains: records.ParseDomains("mylab.lan"), EnableGatewayAPI: true, Log: logr.Discard()}
	_, err := d.Discover(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("a transient Get error must surface (so pruning is disabled), got %v", err)
	}

	c = fake.NewClientBuilder().WithScheme(scheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			return boom
		}}).Build()
	d = &Discoverer{Client: c, APIReader: c, Domains: records.ParseDomains("mylab.lan"), EnableGatewayAPI: true, EnableIstio: true, Log: logr.Discard()}
	_, err = d.Discover(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("list errors must surface, got %v", err)
	}
}

func TestSelectorKeyIsStable(t *testing.T) {
	a := selectorKey(map[string]string{"a": "1", "b": "2"})
	b := selectorKey(map[string]string{"b": "2", "a": "1"})
	if a != b {
		t.Fatalf("%q != %q", a, b)
	}
}
