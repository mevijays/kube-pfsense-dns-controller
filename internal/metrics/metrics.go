// SPDX-License-Identifier: Apache-2.0

// Package metrics exposes Prometheus metrics on the controller-runtime metrics endpoint.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// DesiredRecords is the number of hostnames found in the cluster in the last sync.
	DesiredRecords = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pfsense_dns_desired_records",
		Help: "Hostnames discovered from HTTPRoutes and VirtualServices in the last sync.",
	})
	// Changes counts host override changes by action (create, update, delete).
	Changes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pfsense_dns_changes_total",
		Help: "Host override changes applied to pfSense, by action.",
	}, []string{"action"})
	// Conflicts is the number of desired names blocked by an unmanaged override in the last sync.
	Conflicts = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pfsense_dns_conflicts",
		Help: "Desired names that exist on pfSense without the ownership tag (skipped).",
	})
	// Syncs counts sync cycles by result (success, error).
	Syncs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pfsense_dns_syncs_total",
		Help: "Sync cycles by result.",
	}, []string{"result"})
	// LastSuccess is the Unix time of the last sync that finished without error.
	LastSuccess = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pfsense_dns_last_successful_sync_timestamp_seconds",
		Help: "Unix time of the last successful sync.",
	})
)

func init() {
	crmetrics.Registry.MustRegister(DesiredRecords, Changes, Conflicts, Syncs, LastSuccess)
}
