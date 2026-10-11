# Changelog

All notable changes are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project aims to follow
[Semantic Versioning](https://semver.org/) once it reaches 1.0.

## [Unreleased]

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
