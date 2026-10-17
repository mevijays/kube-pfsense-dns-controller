# Changelog

All notable changes are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project aims to follow
[Semantic Versioning](https://semver.org/) once it reaches 1.0.

## [Unreleased]

### Fixed (found in live testing against a real pfSense)
- pfSense keeps host overrides sorted by name, so a create is inserted mid-list and shifts IDs. Creates now run after
  updates and deletes, so a create and a delete in the same sync no longer cost a refused write and an extra cycle.
- Leader election could not record its Event (`events is forbidden`); the Role now allows creating Events in its namespace.
- The leader lease is released on shutdown, so a restart or rollout hands over immediately instead of after ~15s.

### Fixed (in-depth review)
- Updates and deletes re-read each entry by ID and only write if it is unchanged, so a concurrent edit in the pfSense UI
  or by another controller can no longer make the controller modify or delete the wrong (possibly hand-made) record.
- Ownership tags are matched exactly: owner `k8s-dns-sync` no longer claims (and prunes) records of `k8s-dns-sync-b`.
- A hostname that is still declared but whose gateway address is temporarily unknown keeps its record instead of being deleted.
- A failed or interrupted `apply` is retried on the next sync, and `apply` completes even if the pod is shutting down.
- A 2xx response that is not the pfSense REST API envelope (HTML page, proxy, wrong URL) is an error, not an empty list.
- Invalid boolean/duration environment values (e.g. `DRY_RUN=yes`) now stop the controller instead of being ignored.
- `deploy/deployment.yaml` referenced image tag `v0.1.0`; release images are tagged without the `v`.
- The leader election Lease is named per owner (or per Helm release), so separate instances no longer block each other.
- The Istio API version is taken from the cluster, so Istio releases that serve only `v1beta1` work.
- IP addresses are compared in canonical form, avoiding endless update/apply cycles for equivalent IPv6 spellings.
- The API key is read from a mounted file and re-read on every request, so rotating the Secret needs no restart.
- The HTTP client keeps the default transport (proxy from environment, dial and TLS handshake timeouts).
- Conflicts are logged once when they appear or change, not on every sync.
- Descriptions are truncated on a character boundary (no broken UTF-8).

### Added
- Helm chart (`charts/kube-pfsense-dns-controller`), published to ghcr.io as an OCI artifact and attached to GitHub Releases.
- Release workflow: multi-arch image (amd64/arm64) with SBOM and provenance, chart, GitHub Release.
- `--adopt-existing`: take over unmanaged host overrides whose name matches a route.
- Prometheus metrics (`pfsense_dns_*`) and clearer conflict logging (same IP vs. different IP).
- Hint in the error message when pfSense rejects the API key; the key is trimmed of whitespace.
- Much wider tests: client failure modes, TLS modes, discovery edge cases, randomized planner invariants,
  hostname fuzzing, chart rendering tests including an RBAC least-privilege check; staticcheck and govulncheck in CI.

### Changed
- Built with Go 1.26.9 and golang.org/x/net 0.61 (fixes known standard library / module vulnerabilities).
- The image is cross-compiled natively instead of under emulation.

### Added (initial)
- Initial implementation: watches Gateway API `HTTPRoute` and Istio `VirtualService`
  objects and syncs their hostnames to pfSense DNS Resolver host overrides, pointing at the
  address of the gateway that serves them.
- Ownership tagging (`managed-by=<owner-id>`), pruning of stale entries, dry-run mode,
  leader election, health probes and metrics.
- Example manifests (`deploy/`, `examples/`), container image build, CI.
