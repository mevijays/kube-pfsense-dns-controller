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

// Record is the desired state for one hostname.
type Record struct {
	Host    string
	Domain  string
	IPs     []string // sorted, unique
	Sources []string // sorted, unique, e.g. "HTTPRoute/default/nginx-gwapi"
}

// FQDN returns host.domain.
func (r *Record) FQDN() string { return r.Host + "." + r.Domain }

// Set maps FQDN to its Record.
type Set map[string]*Record

// Add records that fqdn should resolve to ip because of source.
func (s Set) Add(host, domain, ip, source string) {
	fqdn := host + "." + domain
	r, ok := s[fqdn]
	if !ok {
		r = &Record{Host: host, Domain: domain}
		s[fqdn] = r
	}
	if net.ParseIP(ip) != nil {
		r.IPs = addSorted(r.IPs, ip)
	}
	r.Sources = addSorted(r.Sources, source)
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
