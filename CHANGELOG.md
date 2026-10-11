# Changelog

All notable changes are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project aims to follow
[Semantic Versioning](https://semver.org/) once it reaches 1.0.

## [Unreleased]

### Added
- Initial implementation: watches Gateway API `HTTPRoute` and Istio `VirtualService`
  objects and syncs their hostnames to pfSense DNS Resolver host overrides, pointing at the
  address of the gateway that serves them.
- Ownership tagging (`managed-by=<owner-id>`), pruning of stale entries, dry-run mode,
  leader election, health probes and metrics.
- Example manifests (`deploy/`, `examples/`), container image build, CI.
