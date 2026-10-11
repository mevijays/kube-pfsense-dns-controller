# kube-pfsense-dns-controller Helm chart

Installs [kube-pfsense-dns-controller](https://github.com/mevijays/kube-pfsense-dns-controller), which publishes the
hostnames of Gateway API `HTTPRoute`s and Istio `VirtualService`s as pfSense DNS Resolver host overrides.

## Install

```bash
kubectl create namespace dns-sync
kubectl -n dns-sync create secret generic pfsense-api --from-literal=api-key='<bare pfSense REST API key>'

helm install kube-pfsense-dns-controller \
  oci://ghcr.io/mevijays/charts/kube-pfsense-dns-controller --version <version> \
  --namespace dns-sync \
  --set pfsense.url=https://pfsense.example.lan \
  --set pfsense.existingSecret=pfsense-api \
  --set 'config.domains={example.lan}' \
  --set config.dryRun=true
```

Start with `config.dryRun=true`, read the logs, then set it to `false`.

The API key Secret is mounted as a file and re-read on every request, so rotating the key
(`kubectl create secret ... --dry-run=client -o yaml | kubectl apply -f -`) takes effect within about a minute,
without restarting the pod.

## Required values

| Value | Description |
|---|---|
| `pfsense.url` | pfSense base URL |
| `pfsense.existingSecret` (or `pfsense.apiKey`) | Secret holding the REST API key |
| `config.domains` | Domains to manage, e.g. `{example.lan}` |

See [`values.yaml`](values.yaml) for everything else. Highlights: `pfsense.caSecret` (trust your CA),
`config.adoptExisting`, `config.prune`, `config.ownerId`, `config.istioGatewayNamespaces`, `replicaCount`
(2 with `leaderElection.enabled` gives failover), `metrics.service` / `metrics.serviceMonitor`.

## Permissions created

- ClusterRole: `get/list/watch` on `httproutes` and `gateways` (Gateway API) and `virtualservices` and `gateways` (Istio),
  depending on `config.enableGatewayAPI` / `config.enableIstio`.
- A Role per namespace in `config.istioGatewayNamespaces`: `get/list` on `pods` and `services`.
- A Role for its leader-election Lease.

The controller never writes to the cluster, except its lease.
