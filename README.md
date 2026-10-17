# kube-pfsense-dns-controller

[![CI](https://github.com/mevijays/kube-pfsense-dns-controller/actions/workflows/ci.yml/badge.svg)](https://github.com/mevijays/kube-pfsense-dns-controller/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

A Kubernetes controller that keeps **pfSense DNS** in step with your cluster. It watches
**Gateway API `HTTPRoute`s** and **Istio `VirtualService`s** and publishes each hostname as a
**DNS Resolver (Unbound) host override** on pfSense, pointing at the IP of the gateway that serves it.

Create a route for `jenkins.mylab.lan`, and a few seconds later your whole network resolves it.
Delete the route, and the DNS record goes away.

> **Status: early / alpha.** Covered by unit tests and checked in dry-run against a live cluster,
> but not yet widely tested against real pfSense installations. Start with `DRY_RUN=true`.
> Feedback and bug reports are very welcome (see [Contributing](#contributing)).

## Why

Home labs and small on-prem clusters usually have a firewall running the LAN's DNS and a
LoadBalancer implementation (MetalLB, Cilium, ...) that hands the gateways their IPs. Something
still has to tell DNS that `app.mylab.lan` lives on the gateway's IP. [ExternalDNS](https://github.com/kubernetes-sigs/external-dns)
does this for many providers, but pfSense is not one of its in-tree providers. This project is a small, focused
alternative for that case.

## How it works

```
 HTTPRoute  hostnames ──┐                                ┌─ Gateway.status.addresses ──┐
                        ├─► desired: hostname -> IP(s) ──┤                             ├─► diff ─► pfSense
 VirtualService hosts ──┘                                └─ LoadBalancer Service IP ───┘   host overrides
                                                            (of the Istio ingress gateway)
```

| Source | Hostnames from | IP from |
|---|---|---|
| Gateway API `HTTPRoute` | `spec.hostnames` | `status.addresses` of every Gateway that has **accepted** the route |
| Istio `VirtualService` (whichever API version the cluster serves) | `spec.hosts` | external IP of the LoadBalancer Service in front of the ingress gateway pods selected by the referenced Istio `Gateway` (`spec.selector`) |

- **Level-based.** Every change (plus a resync every 2 minutes) recomputes the whole desired state
  and diffs it with pfSense. No finalizers are added to your objects; deletes, IP changes and
  manual drift all heal the same way.
- **Only touches its own records.** Each override it creates is tagged `managed-by=<owner-id>` in the
  description, and only an exact tag match counts (owner `dns` never claims `dns-b`'s records). Entries without the tag
  (hand-made ones) are never updated or deleted; a name clash is logged once and skipped.
- **Prunes safely.** A managed record is deleted only when no route or VirtualService declares its hostname any more.
  While a hostname is still declared but its gateway address is temporarily unknown (gateway pods restarting, LB IP
  being reassigned, route status being rewritten), its record is kept. If discovery had an error, nothing is deleted.
- **Safe against concurrent edits.** pfSense identifies host overrides by list position, which shifts when an entry is
  removed. Before every update or delete the controller re-reads the entry and only writes if it is still exactly the one
  it planned against; otherwise it skips the remaining writes and re-plans on the next sync. So edits in the pfSense UI or
  by another controller sharing the firewall can never make it change or delete the wrong record.
- **Changes always get applied.** If the `apply` call fails, or the pod is stopped mid-sync, it is retried on the next
  sync; once writes were made, `apply` is completed even during shutdown.
- **Scoped by domain.** Only hostnames under `DNS_DOMAINS` are handled; others and wildcards
  (`*.mylab.lan`, which host overrides cannot express) are ignored.
- **Multiple gateways.** A hostname served by two gateways gets both IPs on one override.
- **Minimal pfSense traffic.** One `apply` call per cycle, and only when something changed.
- **Dry-run mode** shows exactly what would change, even with no pfSense at all.

## Quick start

### 1. Prepare pfSense

1. Install the [pfSense REST API package](https://pfrest.org) (v2).
2. Create an API key (System > REST API > Keys) for a user allowed to manage `services/dns_resolver`.
3. Make sure the DNS Resolver service is enabled.

### 2. Try it without writing anything

```bash
git clone https://github.com/mevijays/kube-pfsense-dns-controller && cd kube-pfsense-dns-controller
make run-dry          # uses your current kubeconfig context; set DNS_DOMAINS=yourdomain.lan
```

You will see lines like `dry-run: would create fqdn=jenkins.mylab.lan ip=["172.16.1.61"]`.

### 3. Deploy with Helm

Create the API key secret first. It must hold the **bare key**, with no prefix or whitespace:

```bash
kubectl create namespace dns-sync
kubectl -n dns-sync create secret generic pfsense-api --from-literal=api-key='<your pfSense REST API key>'
```

Then install the chart (published to GitHub Container Registry on every release):

```bash
helm install kube-pfsense-dns-controller \
  oci://ghcr.io/mevijays/charts/kube-pfsense-dns-controller --version <version> \
  --namespace dns-sync \
  --set pfsense.url=https://pfsense.example.lan \
  --set pfsense.existingSecret=pfsense-api \
  --set 'config.domains={example.lan}' \
  --set config.dryRun=true            # log only, until the output looks right

kubectl -n dns-sync logs deploy/kube-pfsense-dns-controller -f
```

When the log shows what you expect, switch to real writes:
`helm upgrade ... --reuse-values --set config.dryRun=false`.

All chart values are documented in [`charts/kube-pfsense-dns-controller/values.yaml`](charts/kube-pfsense-dns-controller/values.yaml)
(`pfsense.caSecret` to trust your CA, `config.istioGatewayNamespaces`, `replicaCount`, `metrics.serviceMonitor`, ...).

Prefer plain manifests? [`deploy/`](deploy/) has an equivalent RBAC + Deployment.

#### Existing hand-made records

Records you created by hand are never touched. If a route's hostname already exists on pfSense without the ownership
tag, the controller logs it and skips it:

- same IP as the cluster wants: `unmanaged host override already resolves to the desired IP; leaving it alone`
- different IP: `CONFLICT: an unmanaged host override has a different IP; skipping`

To let the controller take over such records, set `config.adoptExisting=true` (`--adopt-existing`). It then tags them,
corrects their IP, and keeps your original description after a `|`. From then on they are managed like any other record.

Sample resources are in [`examples/`](examples/).

## Configuration

Flags, with environment variables as defaults. Boolean and duration variables are parsed strictly: a value such as
`DRY_RUN=yes` stops the controller with a configuration error instead of being silently ignored (use `true`/`false`).

| Env | Flag | Default | Description |
|---|---|---|---|
| `PFSENSE_URL` | `--pfsense-url` | | pfSense base URL, e.g. `https://pfsense.mylab.lan` |
| `PFSENSE_API_KEY_FILE` | `--pfsense-api-key-file` | | File holding the API key, re-read on every request (the chart mounts the Secret here, so rotating it needs no restart; the kubelet refreshes the file within about 1-2 minutes) |
| `PFSENSE_API_KEY` | | | API key as an environment variable (used when no key file is set; never a flag) |
| `PFSENSE_CA_FILE` | `--pfsense-ca-file` | | CA bundle to trust for pfSense's certificate |
| `PFSENSE_INSECURE_SKIP_VERIFY` | `--pfsense-insecure-skip-verify` | `false` | Skip TLS verification (lab use) |
| `DNS_DOMAINS` | `--domains` | **required** | Comma-separated domains to manage, e.g. `mylab.lan` |
| `OWNER_ID` | `--owner-id` | `k8s-dns-sync` | Ownership tag; use a different value per cluster sharing one pfSense |
| `PRUNE` | `--prune` | `true` | Delete managed records that no longer have a route |
| `ADOPT_EXISTING` | `--adopt-existing` | `false` | Take over unmanaged records whose name matches a route (see above) |
| `DRY_RUN` | `--dry-run` | `false` | Log changes only. With no `PFSENSE_URL`, pfSense is treated as empty |
| `ISTIO_GATEWAY_NAMESPACES` | `--istio-gateway-namespaces` | `istio-system` | Namespaces holding Istio ingress gateway pods and Services |
| `ENABLE_ISTIO` | `--enable-istio` | `true` | Watch VirtualServices (disable if the Istio CRDs are absent) |
| `ENABLE_GATEWAY_API` | `--enable-gateway-api` | `true` | Watch HTTPRoutes (disable if the Gateway API CRDs are absent) |
| `RESYNC_PERIOD` | `--resync-period` | `2m` | Full resync interval |
| `LEADER_ELECT` | `--leader-elect` | `true` | Leader election (needed when running more than one replica) |
| `LEADER_ELECTION_ID` | `--leader-election-id` | derived from `OWNER_ID` | Lease name; separate instances in one namespace get separate leases |
| `METRICS_ADDR` / `PROBE_ADDR` | `--metrics-bind-address` / `--health-probe-bind-address` | `:8080` / `:8081` | Metrics and `/healthz`, `/readyz` |

pfSense endpoints used: `GET /api/v2/services/dns_resolver/host_overrides`,
`POST|PATCH|DELETE /api/v2/services/dns_resolver/host_override`, `POST /api/v2/services/dns_resolver/apply`,
authenticated with the `x-api-key` header.

## Permissions

[`deploy/rbac.yaml`](deploy/rbac.yaml) grants read-only (`get/list/watch`) access to HTTPRoutes, Gateways,
VirtualServices and Istio Gateways, read access to Pods and Services **only in the Istio gateway namespace(s)**,
and Lease access for leader election. The controller never writes to the cluster, except its leader lease.

## Observability

Metrics are served on `:8080/metrics` (set `metrics.service.enabled` and `metrics.serviceMonitor.enabled` in the chart):

| Metric | |
|---|---|
| `pfsense_dns_desired_records` | hostnames found in the cluster in the last sync |
| `pfsense_dns_changes_total{action}` | `create` / `update` / `delete` operations applied |
| `pfsense_dns_conflicts` | desired names blocked by an unmanaged record |
| `pfsense_dns_syncs_total{result}` | sync cycles, `success` / `error` |
| `pfsense_dns_last_successful_sync_timestamp_seconds` | alert on this going stale |

Liveness and readiness are `/healthz` and `/readyz` on `:8081`.

## Quality

`make check` runs formatting, `go vet` and the unit tests with the race detector. On top of that:

- `make lint` (staticcheck), `make vuln` (govulncheck) and `make fuzz` (hostname parser fuzzing);
- `hack/chart-test.sh` renders the Helm chart with many value combinations and asserts on the result, including
  that the RBAC stays least-privilege (read-only, plus the controller's own lease);
- the planner has a randomized test that checks, for thousands of random pfSense contents, that unmanaged records are never
  modified or deleted, that a sync converges to the desired state, and that running it again changes nothing.

## Limitations

- IPv4/IPv6 addresses only; no CNAME/alias records.
- Only the pfSense DNS Resolver (Unbound) is supported, not the Forwarder (dnsmasq) or other providers.
- A hostname's records come from the gateway addresses, not from per-listener details.
- A VirtualService/HTTPRoute that lists no hostname produces no record.
- If pfSense holds several unmanaged duplicates of one name, `--adopt-existing` adopts only the first.

## Roadmap ideas

Contributions on any of these are welcome: other Gateway API route kinds (`GRPCRoute`, `TLSRoute`) and plain
`Ingress`; support for more pfSense/OPNsense DNS backends; an annotation to override the target IP per route;
CNAME/alias records.

## Contributing

Contributions are welcome! Please read [CONTRIBUTING.md](CONTRIBUTING.md) and the
[Code of Conduct](CODE_OF_CONDUCT.md). Quick start for developers:

```bash
make check      # gofmt, vet, tests with -race
make build
```

Security issues: see [SECURITY.md](SECURITY.md).

## License

Licensed under the [Apache License 2.0](LICENSE). pfSense is a registered trademark of Rubicon Communications, LLC (Netgate);
this project is independent and not affiliated with or endorsed by them.
