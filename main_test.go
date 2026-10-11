// SPDX-License-Identifier: Apache-2.0

package main

import (
	"regexp"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/mevijays/kube-pfsense-dns-controller/internal/discover"
)

func TestParseEnvBoolRejectsUnknownValues(t *testing.T) {
	for _, v := range []string{"yes", "no", "on", "enabled", "tru"} {
		t.Setenv("X_BOOL", v)
		if _, err := parseEnvBool("X_BOOL", false); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
	for v, want := range map[string]bool{"true": true, "TRUE": true, "1": true, "false": false, "0": false, " true ": true} {
		t.Setenv("X_BOOL", v)
		if got, err := parseEnvBool("X_BOOL", !want); err != nil || got != want {
			t.Errorf("%q -> %v, %v", v, got, err)
		}
	}
	t.Setenv("X_BOOL", "")
	if got, err := parseEnvBool("X_BOOL", true); err != nil || !got {
		t.Error("empty value must use the default")
	}
}

func TestParseEnvDuration(t *testing.T) {
	for _, v := range []string{"5", "two minutes", "-1m", "0s"} {
		t.Setenv("X_DUR", v)
		if _, err := parseEnvDuration("X_DUR", time.Minute); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
	t.Setenv("X_DUR", "90s")
	if d, err := parseEnvDuration("X_DUR", time.Minute); err != nil || d != 90*time.Second {
		t.Errorf("90s -> %v, %v", d, err)
	}
}

func TestLeaseIDIsPerOwnerAndValid(t *testing.T) {
	valid := regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
	a, b := leaseID("", "k8s-dns-sync"), leaseID("", "k8s-dns-sync-b")
	if a == b {
		t.Fatal("different owners must not share a lease")
	}
	for _, owner := range []string{"k8s-dns-sync", "Cluster_A/prod", "x", "---", "a" + string(make([]byte, 100))} {
		id := leaseID("", owner)
		if len(id) > 63 || !valid.MatchString(id) {
			t.Errorf("owner %q -> invalid lease name %q", owner, id)
		}
	}
	if got := leaseID("explicit-name", "x"); got != "explicit-name" {
		t.Errorf("explicit id ignored: %q", got)
	}
}

func TestServedGVKFollowsTheCluster(t *testing.T) {
	old := schema.GroupVersion{Group: "networking.istio.io", Version: "v1beta1"}
	m := meta.NewDefaultRESTMapper([]schema.GroupVersion{old})
	m.Add(old.WithKind("VirtualService"), meta.RESTScopeNamespace)
	got, err := servedGVK(m, discover.VirtualServiceGVK)
	if err != nil || got.Version != "v1beta1" {
		t.Fatalf("got %v, %v; want the served v1beta1", got, err)
	}
	if _, err := servedGVK(meta.NewDefaultRESTMapper(nil), discover.VirtualServiceGVK); err == nil {
		t.Error("missing CRD not reported")
	}
}
