// SPDX-License-Identifier: Apache-2.0

package pfsense

import (
	"context"

	"github.com/go-logr/logr"
)

// DryRun logs what would change and never writes. Reads pass through to Inner;
// with no Inner it behaves like an empty pfSense, which is handy for checking
// what the controller would publish without a firewall.
type DryRun struct {
	Inner Backend
	Log   logr.Logger
}

func (d *DryRun) ListHostOverrides(ctx context.Context) ([]HostOverride, error) {
	if d.Inner == nil {
		return nil, nil
	}
	return d.Inner.ListHostOverrides(ctx)
}

func (d *DryRun) CreateHostOverride(_ context.Context, h HostOverride) error {
	d.Log.Info("dry-run: would create", "fqdn", h.FQDN(), "ip", h.IP, "descr", h.Descr)
	return nil
}

func (d *DryRun) UpdateHostOverride(_ context.Context, h HostOverride) error {
	d.Log.Info("dry-run: would update", "id", h.ID, "fqdn", h.FQDN(), "ip", h.IP)
	return nil
}

func (d *DryRun) DeleteHostOverride(_ context.Context, id int) error {
	d.Log.Info("dry-run: would delete", "id", id)
	return nil
}

func (d *DryRun) Apply(context.Context) error {
	d.Log.Info("dry-run: would apply DNS resolver")
	return nil
}

var _ Backend = (*DryRun)(nil)
