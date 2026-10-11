# Contributing

Thanks for your interest in improving kube-pfsense-dns-controller! Bug reports,
questions, docs fixes and code are all welcome. By taking part you agree to follow the
[Code of Conduct](CODE_OF_CONDUCT.md).

## Ways to help

- **Report a bug** or **suggest a feature** with an [issue](https://github.com/mevijays/kube-pfsense-dns-controller/issues/new/choose).
  Please include versions (Kubernetes, Istio / Gateway API implementation, pfSense + REST API package)
  and, for bugs, controller logs. Run with `DRY_RUN=true` and redact anything private.
- **Test on your setup.** The controller is young; reports from different Gateway API
  implementations (Envoy Gateway, Cilium, Traefik, ...) and pfSense versions are very valuable.
- **Send a pull request.** For anything larger than a small fix, open an issue first so we
  can agree on the approach.
- **Security problems** go through [SECURITY.md](SECURITY.md), not public issues.

## Development setup

You need Go (the version in `go.mod`) and, to try it on a cluster, `kubectl` access.

```bash
git clone https://github.com/mevijays/kube-pfsense-dns-controller
cd kube-pfsense-dns-controller
make check        # gofmt check, go vet, tests with -race
make lint vuln    # staticcheck, govulncheck
make chart-lint && ./hack/chart-test.sh   # Helm chart (needs helm and python3 with pyyaml)
make build        # binary in ./bin
make run-dry      # run against your current kubeconfig context, writes nothing to pfSense
```

`make run-dry` needs `DNS_DOMAINS` (defaults to `mylab.lan`) and logs `dry-run: would create ...`
lines, so you can see what would be published without a firewall.

## Project layout

| Path | What lives there |
|---|---|
| `main.go` | flags/env, manager setup |
| `internal/discover` | turns HTTPRoutes and VirtualServices into hostname -> IP records |
| `internal/syncer` | diffs desired vs. current state, applies changes (ownership tag, prune) |
| `internal/pfsense` | pfSense REST API v2 client and the dry-run backend |
| `internal/records` | domain matching and the desired-state types |
| `internal/controller` | controller-runtime wiring and the periodic resync |
| `charts/` | Helm chart (published to ghcr.io on release) |
| `hack/` | chart rendering tests and the RBAC least-privilege check |
| `deploy/` | plain-manifest alternative to the chart |
| `examples/` | sample HTTPRoute / VirtualService |

## Pull requests

1. Fork, then branch from `main`.
2. Keep the change focused; add or update tests. New behaviour in `internal/syncer` or
   `internal/discover` should come with a table-style test, as the existing ones do.
3. Run `make check`.
4. Update `README.md` / `examples/` if behaviour or configuration changes, and add a line
   to the "Unreleased" section of `CHANGELOG.md`.
5. Open the PR using the template. A maintainer will review; small, focused PRs get reviewed fastest.

### Commit messages

Use short imperative subjects ("Skip routes that no gateway accepted"), with a body that explains
*why* when it is not obvious. Referencing an issue (`Fixes #12`) is appreciated.

## Design principles

These explain most review feedback:

- **Never touch what we do not own.** Only overrides tagged `managed-by=<owner-id>` may be
  updated or deleted. Anything that could change that needs a very good reason.
- **Fail safe.** If discovery is incomplete, do not delete anything.
- **Level-based, not edge-based.** Recompute the whole desired state; do not depend on seeing every event.
- **Small dependency footprint.** Istio objects are read as unstructured on purpose.

## Licensing

The project is licensed under [Apache-2.0](LICENSE). By submitting a contribution you agree
that it is licensed under the same terms. New Go files should start with
`// SPDX-License-Identifier: Apache-2.0`.
