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
	"time"
	"unicode/utf8"

	"github.com/go-logr/logr"

	"github.com/mevijays/kube-pfsense-dns-controller/internal/metrics"
	"github.com/mevijays/kube-pfsense-dns-controller/internal/pfsense"
	"github.com/mevijays/kube-pfsense-dns-controller/internal/records"
)

// maxDescr is pfSense's limit for a host override description, in characters.
const maxDescr = 1024

// applyTimeout bounds the apply call that runs after writes, even during shutdown.
const applyTimeout = 30 * time.Second

// Marker returns the description prefix that tags overrides owned by ownerID.
// Only entries carrying it are ever updated or deleted.
func Marker(ownerID string) string { return "managed-by=" + ownerID }

// IsManaged reports whether descr carries exactly ownerID's tag. The tag must be
// followed by a space or end the string, so owner "dns" does not claim entries
// of owner "dns-b".
func IsManaged(descr, ownerID string) bool {
	m := Marker(ownerID)
	return descr == m || strings.HasPrefix(descr, m+" ")
}

// Options control how a Plan is built.
type Options struct {
	// Owner is the ownership tag value written to, and required on, managed overrides.
	Owner string
	// Prune deletes managed overrides that are no longer desired.
	Prune bool
	// Adopt takes over unmanaged overrides whose name matches a desired record
	// (tagging them and, if needed, correcting their IPs) instead of skipping them.
	Adopt bool
}

// Conflict is a desired name that already exists on pfSense without our tag.
type Conflict struct {
	Name     string
	Existing []string // IPs on pfSense
	Desired  []string // IPs the cluster wants
}

// Same reports whether the unmanaged entry already resolves to the desired IPs.
func (c Conflict) Same() bool {
	return slices.Equal(records.CanonicalIPs(c.Existing), records.CanonicalIPs(c.Desired))
}

// Update replaces Old (the entry as listed) with New. Old is used to check that
// the entry at Old.ID is still the same one before writing.
type Update struct {
	Old, New pfsense.HostOverride
}

// Plan is the set of changes needed to reach the desired state.
type Plan struct {
	Create    []pfsense.HostOverride
	Update    []Update
	Delete    []pfsense.HostOverride
	Conflicts []Conflict // desired names that already exist unmanaged on pfSense
}

// Empty reports whether nothing has to change.
func (p Plan) Empty() bool {
	return len(p.Create)+len(p.Update)+len(p.Delete) == 0
}

// truncateRunes cuts s to at most n characters without splitting a character.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

func description(owner string, r *records.Record, previous string) string {
	srcs := r.Sources
	if len(srcs) > 3 {
		srcs = srcs[:3]
	}
	d := Marker(owner) + " src=" + strings.Join(srcs, ",")
	if previous = strings.TrimSpace(previous); previous != "" {
		d += " | " + previous // keep the original note of an adopted entry
	}
	return truncateRunes(d, maxDescr)
}

// adoptedNote returns the original description kept after " | " on an adopted entry.
func adoptedNote(descr string) string {
	if i := strings.Index(descr, " | "); i >= 0 {
		return descr[i+3:]
	}
	return ""
}

// BuildPlan compares desired with current.
func BuildPlan(desired records.Set, current []pfsense.HostOverride, opt Options) Plan {
	var p Plan
	owner, prune := opt.Owner, opt.Prune

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
			// Still declared, address unknown right now: leave whatever is on
			// pfSense alone (it is also exempt from pruning below).
			continue
		}
		want := records.CanonicalIPs(rec.IPs)
		existing := byName[name]
		var ours []pfsense.HostOverride
		for _, e := range existing {
			if IsManaged(e.Descr, owner) {
				ours = append(ours, e)
			}
		}
		switch {
		case len(existing) == 0:
			p.Create = append(p.Create, pfsense.HostOverride{
				Host: rec.Host, Domain: rec.Domain, IP: want, Descr: description(owner, rec, ""),
			})
		case len(ours) == 0 && opt.Adopt:
			// Take over the hand-made entry: tag it so it is managed from now on.
			old := existing[0]
			n := old
			n.IP = want
			n.Descr = description(owner, rec, old.Descr)
			p.Update = append(p.Update, Update{Old: old, New: n})
		case len(ours) == 0:
			// Somebody created this by hand: never overwrite it.
			c := Conflict{Name: name, Desired: want}
			for _, e := range existing {
				c.Existing = append(c.Existing, e.IP...)
			}
			c.Existing = records.CanonicalIPs(c.Existing)
			p.Conflicts = append(p.Conflicts, c)
		default:
			old := ours[0]
			if !slices.Equal(records.CanonicalIPs(old.IP), want) {
				n := old
				n.IP = want
				n.Descr = description(owner, rec, adoptedNote(old.Descr))
				p.Update = append(p.Update, Update{Old: old, New: n})
			}
			// Any further managed duplicates of the same name are stale.
			if prune {
				p.Delete = append(p.Delete, ours[1:]...)
			}
		}
	}

	if prune {
		for _, c := range current {
			if !IsManaged(c.Descr, owner) {
				continue
			}
			if _, declared := desired[c.FQDN()]; declared {
				continue // still wanted (possibly with its address temporarily unknown)
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

// ErrConcurrentChange means pfSense changed between listing and writing, so the
// remaining ID-based writes were skipped; the next sync re-plans from fresh data.
var ErrConcurrentChange = errors.New("pfSense host overrides changed since they were listed")

// sameEntry reports whether two overrides are the same record (ignoring ID).
func sameEntry(a, b pfsense.HostOverride) bool {
	return a.FQDN() == b.FQDN() && a.Descr == b.Descr &&
		slices.Equal(records.CanonicalIPs(a.IP), records.CanonicalIPs(b.IP))
}

// verify checks that the entry at want.ID is still want.
func verify(ctx context.Context, b pfsense.Backend, want pfsense.HostOverride) error {
	got, err := b.GetHostOverride(ctx, want.ID)
	if err != nil {
		return fmt.Errorf("re-read id %d: %w", want.ID, err)
	}
	if !sameEntry(got, want) {
		return fmt.Errorf("%w: id %d is now %q, expected %q", ErrConcurrentChange, want.ID, got.FQDN(), want.FQDN())
	}
	return nil
}

// Result reports what Execute did.
type Result struct {
	// Changed is true if at least one write succeeded.
	Changed bool
	// Applied is true if pfSense confirmed the apply after those writes.
	Applied bool
}

// Execute applies the plan.
//
// pfSense IDs are list positions that shift when an entry is removed, so:
// updates run first, creates next (they append), and deletes last from the
// highest ID down. Before every update or delete the entry is re-read by ID and
// must still be exactly the one that was planned against; on any mismatch (a
// change made in the UI or by another controller) the remaining ID-based writes
// are skipped and ErrConcurrentChange is returned.
//
// Apply runs once if anything changed. It uses a context detached from ctx so a
// shutdown in the middle of a sync does not leave written changes unapplied.
func Execute(ctx context.Context, b pfsense.Backend, p Plan, log logr.Logger) (Result, error) {
	var res Result
	var errs []error
	stale := false

	for _, u := range p.Update {
		if err := verify(ctx, b, u.Old); err != nil {
			errs = append(errs, fmt.Errorf("update %s: %w", u.New.FQDN(), err))
			stale = stale || errors.Is(err, ErrConcurrentChange)
			if stale {
				break
			}
			continue
		}
		log.Info("updating host override", "fqdn", u.New.FQDN(), "ip", u.New.IP, "id", u.New.ID)
		if err := b.UpdateHostOverride(ctx, u.New); err != nil {
			errs = append(errs, fmt.Errorf("update %s: %w", u.New.FQDN(), err))
			continue
		}
		res.Changed = true
		metrics.Changes.WithLabelValues("update").Inc()
	}
	for _, h := range p.Create {
		log.Info("creating host override", "fqdn", h.FQDN(), "ip", h.IP)
		if err := b.CreateHostOverride(ctx, h); err != nil {
			errs = append(errs, fmt.Errorf("create %s: %w", h.FQDN(), err))
			continue
		}
		res.Changed = true
		metrics.Changes.WithLabelValues("create").Inc()
	}
	if !stale {
		del := append([]pfsense.HostOverride(nil), p.Delete...)
		sort.Slice(del, func(i, j int) bool { return del[i].ID > del[j].ID })
		for _, h := range del {
			if err := verify(ctx, b, h); err != nil {
				errs = append(errs, fmt.Errorf("delete %s: %w", h.FQDN(), err))
				if errors.Is(err, ErrConcurrentChange) {
					break
				}
				continue
			}
			log.Info("deleting host override", "fqdn", h.FQDN(), "id", h.ID)
			if err := b.DeleteHostOverride(ctx, h.ID); err != nil {
				errs = append(errs, fmt.Errorf("delete %s: %w", h.FQDN(), err))
				continue
			}
			res.Changed = true
			metrics.Changes.WithLabelValues("delete").Inc()
		}
	}

	if res.Changed {
		if err := Apply(ctx, b); err != nil {
			errs = append(errs, err)
		} else {
			res.Applied = true
		}
	}
	return res, errors.Join(errs...)
}

// Apply asks pfSense to apply pending DNS Resolver changes, with a context that
// survives cancellation of ctx (bounded by applyTimeout).
func Apply(ctx context.Context, b pfsense.Backend) error {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), applyTimeout)
	defer cancel()
	if err := b.Apply(actx); err != nil {
		return fmt.Errorf("apply: %w", err)
	}
	return nil
}
