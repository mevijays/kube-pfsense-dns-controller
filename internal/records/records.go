// SPDX-License-Identifier: Apache-2.0

// Package records holds the desired DNS state derived from the cluster.
package records

import (
	"net"
	"sort"
	"strings"
)

// Domains is the list of DNS domains this controller manages (e.g. mylab.lan).
// Hostnames outside these domains are ignored.
type Domains []string

// ParseDomains turns "mylab.lan, other.lan" into a normalised list, longest
// first so the most specific domain wins in Split.
func ParseDomains(csv string) Domains {
	var out Domains
	for _, d := range strings.Split(csv, ",") {
		d = strings.ToLower(strings.Trim(strings.TrimSpace(d), "."))
		if d != "" {
			out = append(out, d)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// Split separates fqdn into host and managed domain. ok is false for names
// outside the managed domains, for the bare domain itself, and for wildcards
// (host overrides cannot express them).
func (d Domains) Split(fqdn string) (host, domain string, ok bool) {
	fqdn = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(fqdn), "."))
	if fqdn == "" || strings.Contains(fqdn, "*") {
		return "", "", false
	}
	for _, dom := range d {
		suffix := "." + dom
		if strings.HasSuffix(fqdn, suffix) && len(fqdn) > len(suffix) {
			return strings.TrimSuffix(fqdn, suffix), dom, true
		}
	}
	return "", "", false
}

// Record is the desired state for one hostname. A Record with no IPs means the
// hostname is still declared by a route, but its gateway address is unknown right
// now (gateway not ready, pods restarting, route not yet accepted). Its existing
// DNS entry must be kept, not deleted.
type Record struct {
	Host    string
	Domain  string
	IPs     []string // canonical, sorted, unique
	Sources []string // sorted, unique, e.g. "HTTPRoute/default/nginx-gwapi"
}

// FQDN returns host.domain.
func (r *Record) FQDN() string { return r.Host + "." + r.Domain }

// Set maps FQDN to its Record.
type Set map[string]*Record

// Add records that fqdn should resolve to ip because of source. Invalid IPs are
// ignored; valid ones are stored in canonical form.
func (s Set) Add(host, domain, ip, source string) {
	r := s.Declare(host, domain, source)
	if c, ok := CanonicalIP(ip); ok {
		r.IPs = addSorted(r.IPs, c)
	}
}

// Declare records that source still exposes fqdn, without adding an address.
func (s Set) Declare(host, domain, source string) *Record {
	fqdn := host + "." + domain
	r, ok := s[fqdn]
	if !ok {
		r = &Record{Host: host, Domain: domain}
		s[fqdn] = r
	}
	r.Sources = addSorted(r.Sources, source)
	return r
}

// CanonicalIP returns the canonical text form of an IPv4/IPv6 address, so that
// "2001:DB8::0001" and "2001:db8::1" compare equal.
func CanonicalIP(ip string) (string, bool) {
	p := net.ParseIP(strings.TrimSpace(ip))
	if p == nil {
		return "", false
	}
	return p.String(), true
}

// CanonicalIPs canonicalises, sorts and de-duplicates a list of addresses.
// Entries that are not IP addresses are kept as they are.
func CanonicalIPs(in []string) []string {
	var out []string
	for _, v := range in {
		if c, ok := CanonicalIP(v); ok {
			v = c
		}
		out = addSorted(out, v)
	}
	return out
}

func addSorted(list []string, v string) []string {
	i := sort.SearchStrings(list, v)
	if i < len(list) && list[i] == v {
		return list
	}
	list = append(list, "")
	copy(list[i+1:], list[i:])
	list[i] = v
	return list
}

// SortedUnique returns a sorted copy without duplicates.
func SortedUnique(in []string) []string {
	var out []string
	for _, v := range in {
		out = addSorted(out, v)
	}
	return out
}
