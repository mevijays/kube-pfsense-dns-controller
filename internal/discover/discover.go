// SPDX-License-Identifier: Apache-2.0

// Package discover works out which hostnames the cluster exposes and which
// IP each one is reachable on, from Gateway API HTTPRoutes and Istio
// VirtualServices.
package discover

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/mevijays/kube-pfsense-dns-controller/internal/records"
)

var (
	// VirtualServiceGVK and IstioGatewayGVK are the default versions of the Istio
	// kinds, which are read as unstructured objects so the controller does not
	// need to vendor the whole Istio API. main replaces the version with the one
	// the cluster serves (older Istio releases only serve v1beta1/v1alpha3).
	VirtualServiceGVK = schema.GroupVersionKind{Group: "networking.istio.io", Version: "v1", Kind: "VirtualService"}
	IstioGatewayGVK   = schema.GroupVersionKind{Group: "networking.istio.io", Version: "v1", Kind: "Gateway"}
)

// Discoverer computes the desired DNS records.
type Discoverer struct {
	// Client reads from the informer cache (HTTPRoutes, Gateways, Istio objects).
	Client client.Reader
	// APIReader reads straight from the API server (Pods, Services), so no
	// cluster-wide Pod informer is needed.
	APIReader client.Reader

	Domains records.Domains
	// IstioNamespaces are the namespaces searched for Istio ingress gateway
	// workloads (pods selected by a Gateway's spec.selector) and their Services.
	IstioNamespaces []string

	EnableIstio      bool
	EnableGatewayAPI bool

	// VirtualServiceGVK and IstioGatewayGVK override the package defaults with
	// the versions served by the cluster. Zero values mean the defaults.
	VirtualServiceGVK schema.GroupVersionKind
	IstioGatewayGVK   schema.GroupVersionKind

	Log logr.Logger
}

// VirtualServiceKind returns the VirtualService GVK in use.
func (d *Discoverer) VirtualServiceKind() schema.GroupVersionKind {
	if d.VirtualServiceGVK.Empty() {
		return VirtualServiceGVK
	}
	return d.VirtualServiceGVK
}

// IstioGatewayKind returns the Istio Gateway GVK in use.
func (d *Discoverer) IstioGatewayKind() schema.GroupVersionKind {
	if d.IstioGatewayGVK.Empty() {
		return IstioGatewayGVK
	}
	return d.IstioGatewayGVK
}

// Discover returns the desired records. A non-nil error means the result may
// be incomplete; callers must not prune on it.
func (d *Discoverer) Discover(ctx context.Context) (records.Set, error) {
	set := records.Set{}
	var errs []error
	if d.EnableGatewayAPI {
		if err := d.fromHTTPRoutes(ctx, set); err != nil {
			errs = append(errs, err)
		}
	}
	if d.EnableIstio {
		if err := d.fromVirtualServices(ctx, set); err != nil {
			errs = append(errs, err)
		}
	}
	return set, errors.Join(errs...)
}

// managedHosts keeps the hostnames that fall inside the managed domains.
func (d *Discoverer) managedHosts(hosts []string) (out [][2]string) {
	for _, h := range hosts {
		if host, dom, ok := d.Domains.Split(h); ok {
			out = append(out, [2]string{host, dom})
		} else {
			d.Log.V(1).Info("ignoring hostname outside managed domains or wildcard", "hostname", h)
		}
	}
	return out
}

// ---- Gateway API ----

func (d *Discoverer) fromHTTPRoutes(ctx context.Context, set records.Set) error {
	var routes gatewayv1.HTTPRouteList
	if err := d.Client.List(ctx, &routes); err != nil {
		return fmt.Errorf("list httproutes: %w", err)
	}
	var errs []error
	for i := range routes.Items {
		r := &routes.Items[i]
		if !r.DeletionTimestamp.IsZero() {
			continue
		}
		hostnames := make([]string, 0, len(r.Spec.Hostnames))
		for _, h := range r.Spec.Hostnames {
			hostnames = append(hostnames, string(h))
		}
		hosts := d.managedHosts(hostnames)
		if len(hosts) == 0 {
			continue
		}
		if !hasGatewayParent(r) {
			continue
		}
		src := fmt.Sprintf("HTTPRoute/%s/%s", r.Namespace, r.Name)
		// Declare the hostnames first: if the address is unknown right now the
		// existing DNS entry is kept instead of being pruned.
		for _, hd := range hosts {
			set.Declare(hd[0], hd[1], src)
		}
		ips, err := d.httpRouteIPs(ctx, r)
		if err != nil {
			errs = append(errs, fmt.Errorf("httproute %s/%s: %w", r.Namespace, r.Name, err))
			continue
		}
		for _, hd := range hosts {
			for _, ip := range ips {
				set.Add(hd[0], hd[1], ip, src)
			}
		}
	}
	return errors.Join(errs...)
}

// isGatewayRef reports whether a parentRef points at a Gateway API Gateway.
func isGatewayRef(ref gatewayv1.ParentReference) bool {
	if ref.Kind != nil && string(*ref.Kind) != "Gateway" {
		return false
	}
	return ref.Group == nil || string(*ref.Group) == gatewayv1.GroupName
}

func hasGatewayParent(r *gatewayv1.HTTPRoute) bool {
	for _, ref := range r.Spec.ParentRefs {
		if isGatewayRef(ref) {
			return true
		}
	}
	return false
}

// httpRouteIPs returns the addresses of every Gateway the route is accepted by.
func (d *Discoverer) httpRouteIPs(ctx context.Context, r *gatewayv1.HTTPRoute) ([]string, error) {
	var ips []string
	for _, ref := range r.Spec.ParentRefs {
		if !isGatewayRef(ref) {
			continue
		}
		ns := r.Namespace
		if ref.Namespace != nil {
			ns = string(*ref.Namespace)
		}
		name := string(ref.Name)
		if !routeAccepted(r, ns, name) {
			d.Log.V(1).Info("route not accepted by gateway yet, skipping", "route", r.Name, "gateway", ns+"/"+name)
			continue
		}
		var gw gatewayv1.Gateway
		if err := d.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &gw); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		for _, a := range gw.Status.Addresses {
			if a.Type != nil && *a.Type != gatewayv1.IPAddressType {
				continue
			}
			if net.ParseIP(a.Value) != nil {
				ips = append(ips, a.Value)
			}
		}
	}
	return records.SortedUnique(ips), nil
}

// routeAccepted reports whether the Gateway ns/name has accepted the route.
func routeAccepted(r *gatewayv1.HTTPRoute, gwNS, gwName string) bool {
	for _, p := range r.Status.Parents {
		pns := r.Namespace
		if p.ParentRef.Namespace != nil {
			pns = string(*p.ParentRef.Namespace)
		}
		if string(p.ParentRef.Name) != gwName || pns != gwNS {
			continue
		}
		for _, c := range p.Conditions {
			if c.Type == string(gatewayv1.RouteConditionAccepted) && c.Status == "True" {
				return true
			}
		}
	}
	return false
}

// ---- Istio ----

func (d *Discoverer) fromVirtualServices(ctx context.Context, set records.Set) error {
	list := &unstructured.UnstructuredList{}
	gvk := d.VirtualServiceKind()
	gvk.Kind += "List"
	list.SetGroupVersionKind(gvk)
	if err := d.Client.List(ctx, list); err != nil {
		return fmt.Errorf("list virtualservices: %w", err)
	}

	lbCache := map[string][]string{} // gateway selector -> IPs, per run
	var errs []error
	for i := range list.Items {
		vs := &list.Items[i]
		if !vs.GetDeletionTimestamp().IsZero() {
			continue
		}
		vsHosts, _, _ := unstructured.NestedStringSlice(vs.Object, "spec", "hosts")
		hosts := d.managedHosts(vsHosts)
		if len(hosts) == 0 {
			continue
		}
		gwRefs, _, _ := unstructured.NestedStringSlice(vs.Object, "spec", "gateways")
		src := fmt.Sprintf("VirtualService/%s/%s", vs.GetNamespace(), vs.GetName())
		var ips []string
		for _, ref := range gwRefs {
			ns, name, ok := parseGatewayRef(ref, vs.GetNamespace())
			if !ok {
				continue
			}
			// Exposed through an ingress gateway: declare the hostnames so their
			// DNS entries are kept while the gateway's address is unknown.
			for _, hd := range hosts {
				set.Declare(hd[0], hd[1], src)
			}
			got, err := d.istioGatewayIPs(ctx, ns, name, lbCache)
			if err != nil {
				errs = append(errs, fmt.Errorf("virtualservice %s/%s gateway %s/%s: %w", vs.GetNamespace(), vs.GetName(), ns, name, err))
				continue
			}
			ips = append(ips, got...)
		}
		for _, hd := range hosts {
			for _, ip := range records.SortedUnique(ips) {
				set.Add(hd[0], hd[1], ip, src)
			}
		}
	}
	return errors.Join(errs...)
}

// parseGatewayRef understands "name", "namespace/name" and the legacy
// "name.namespace.svc.cluster.local" forms. "mesh" (sidecar-only) is skipped.
func parseGatewayRef(ref, defaultNS string) (ns, name string, ok bool) {
	ref = strings.TrimSpace(ref)
	switch {
	case ref == "" || ref == "mesh":
		return "", "", false
	case strings.Contains(ref, "/"):
		p := strings.SplitN(ref, "/", 2)
		return p[0], p[1], true
	case strings.Contains(ref, "."):
		p := strings.Split(ref, ".")
		if len(p) >= 2 {
			return p[1], p[0], true
		}
		return defaultNS, ref, true
	default:
		return defaultNS, ref, true
	}
}

// istioGatewayIPs returns the LoadBalancer IPs in front of the workload that
// an Istio Gateway selects.
func (d *Discoverer) istioGatewayIPs(ctx context.Context, ns, name string, cache map[string][]string) ([]string, error) {
	gw := &unstructured.Unstructured{}
	gw.SetGroupVersionKind(d.IstioGatewayKind())
	if err := d.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, gw); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	selector, _, _ := unstructured.NestedStringMap(gw.Object, "spec", "selector")
	if len(selector) == 0 {
		return nil, nil
	}
	key := selectorKey(selector)
	if ips, ok := cache[key]; ok {
		return ips, nil
	}
	ips, err := d.workloadLBIPs(ctx, selector)
	if err != nil {
		return nil, err
	}
	cache[key] = ips
	return ips, nil
}

func selectorKey(sel map[string]string) string {
	parts := make([]string, 0, len(sel))
	for k, v := range sel {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// workloadLBIPs finds pods matching selector, then the LoadBalancer Services
// that select those pods, and returns their external IPs.
func (d *Discoverer) workloadLBIPs(ctx context.Context, selector map[string]string) ([]string, error) {
	var ips []string
	for _, ns := range d.IstioNamespaces {
		var pods corev1.PodList
		if err := d.APIReader.List(ctx, &pods, client.InNamespace(ns), client.MatchingLabels(selector)); err != nil {
			return nil, fmt.Errorf("list pods in %s: %w", ns, err)
		}
		if len(pods.Items) == 0 {
			continue
		}
		var svcs corev1.ServiceList
		if err := d.APIReader.List(ctx, &svcs, client.InNamespace(ns)); err != nil {
			return nil, fmt.Errorf("list services in %s: %w", ns, err)
		}
		for i := range svcs.Items {
			svc := &svcs.Items[i]
			if svc.Spec.Type != corev1.ServiceTypeLoadBalancer || len(svc.Spec.Selector) == 0 {
				continue
			}
			if !selectsAny(labels.SelectorFromSet(svc.Spec.Selector), pods.Items) {
				continue
			}
			for _, in := range svc.Status.LoadBalancer.Ingress {
				if net.ParseIP(in.IP) != nil {
					ips = append(ips, in.IP)
				}
			}
		}
	}
	return records.SortedUnique(ips), nil
}

func selectsAny(sel labels.Selector, pods []corev1.Pod) bool {
	for i := range pods {
		if sel.Matches(labels.Set(pods[i].Labels)) {
			return true
		}
	}
	return false
}
