// SPDX-License-Identifier: Apache-2.0

// Package syncer diffs the desired DNS records against pfSense and applies
// the difference.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/go-logr/logr"

	"github.com/mevijays/kube-pfsense-dns-controller/internal/pfsense"
	"github.com/mevijays/kube-pfsense-dns-controller/internal/records"
)

// Marker returns the description prefix that tags overrides owned by ownerID.
// Only entries carrying it are ever updated or deleted.
func Marker(ownerID string) string { return "managed-by=" + ownerID }

// Plan is the set of changes needed to reach the desired state.
type Plan struct {
	Create    []pfsense.HostOverride
	Update    []pfsense.HostOverride
	Delete    []pfsense.HostOverride
	Conflicts []string // desired names that already exist unmanaged on pfSense
}

// Empty reports whether nothing has to change.
func (p Plan) Empty() bool {
	return len(p.Create)+len(p.Update)+len(p.Delete) == 0
}

func description(owner string, r *records.Record) string {
	srcs := r.Sources
	if len(srcs) > 3 {
		srcs = srcs[:3]
	}
	return Marker(owner) + " src=" + strings.Join(srcs, ",")
}

// BuildPlan compares desired with current. prune controls whether managed
// entries that are no longer desired are deleted.
func BuildPlan(desired records.Set, current []pfsense.HostOverride, owner string, prune bool) Plan {
	var p Plan
	marker := Marker(owner)

	byName := map[string][]pfsense.HostOverride{}
	for _, c := range current {
		byName[c.FQDN()] = append(byName[c.FQDN()], c)
	}

	names := make([]string, 0, len(desired))
	for n := range desired {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		rec := desired[name]
		if len(rec.IPs) == 0 {
			continue // nothing to publish (gateway has no address yet)
		}
		existing := byName[name]
		var ours []pfsense.HostOverride
		for _, e := range existing {
			if strings.HasPrefix(e.Descr, marker) {
				ours = append(ours, e)
			}
		}
		switch {
		case len(existing) == 0:
			p.Create = append(p.Create, pfsense.HostOverride{
				Host: rec.Host, Domain: rec.Domain, IP: rec.IPs, Descr: description(owner, rec),
			})
		case len(ours) == 0:
			// Somebody created this by hand: never overwrite it.
			p.Conflicts = append(p.Conflicts, name)
		default:
			first := ours[0]
			if !slices.Equal(records.SortedUnique(first.IP), rec.IPs) {
				first.IP = rec.IPs
				first.Descr = description(owner, rec)
				p.Update = append(p.Update, first)
			}
			// Any further managed duplicates of the same name are stale.
			if prune {
				p.Delete = append(p.Delete, ours[1:]...)
			}
		}
	}

	if prune {
		for _, c := range current {
			if !strings.HasPrefix(c.Descr, marker) {
				continue
			}
			if rec, ok := desired[c.FQDN()]; ok && len(rec.IPs) > 0 {
				continue
			}
			p.Delete = append(p.Delete, c)
		}
	}
	// Dedupe deletes (a stale duplicate can be added by both loops).
	seen := map[int]bool{}
	uniq := p.Delete[:0]
	for _, d := range p.Delete {
		if !seen[d.ID] {
			seen[d.ID] = true
			uniq = append(uniq, d)
		}
	}
	p.Delete = uniq
	return p
}

// Execute applies the plan. Updates and creates run before deletes, and deletes
// run from the highest ID down, because pfSense IDs are list positions that
// shift when an earlier entry is removed. Apply is called once, only if
// something changed.
func Execute(ctx context.Context, b pfsense.Backend, p Plan, log logr.Logger) error {
	var errs []error
	changed := false

	for _, h := range p.Update {
		log.Info("updating host override", "fqdn", h.FQDN(), "ip", h.IP, "id", h.ID)
		if err := b.UpdateHostOverride(ctx, h); err != nil {
			errs = append(errs, fmt.Errorf("update %s: %w", h.FQDN(), err))
			continue
		}
		changed = true
	}
	for _, h := range p.Create {
		log.Info("creating host override", "fqdn", h.FQDN(), "ip", h.IP)
		if err := b.CreateHostOverride(ctx, h); err != nil {
			errs = append(errs, fmt.Errorf("create %s: %w", h.FQDN(), err))
			continue
		}
		changed = true
	}
	del := append([]pfsense.HostOverride(nil), p.Delete...)
	sort.Slice(del, func(i, j int) bool { return del[i].ID > del[j].ID })
	for _, h := range del {
		log.Info("deleting host override", "fqdn", h.FQDN(), "id", h.ID)
		if err := b.DeleteHostOverride(ctx, h.ID); err != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", h.FQDN(), err))
			continue
		}
		changed = true
	}

	if changed {
		if err := b.Apply(ctx); err != nil {
			errs = append(errs, fmt.Errorf("apply: %w", err))
		}
	}
	return errors.Join(errs...)
}
