#!/usr/bin/env bash
# Renders the Helm chart with different values and asserts on the output.
#   hack/chart-test.sh
set -uo pipefail
CHART="$(cd "$(dirname "$0")/.." && pwd)/charts/kube-pfsense-dns-controller"
BASE=(--set pfsense.url=https://pfsense.example.lan --set pfsense.existingSecret=pfsense-api --set 'config.domains={example.lan}')
fail=0
ok()   { printf '  ok    %s\n' "$1"; }
bad()  { printf '  FAIL  %s\n' "$1"; fail=1; }
render() { helm template rel "$CHART" "$@" 2>&1; }
# flat: the rendered YAML on one line, so patterns can span "name: X" and "value: Y"
flat() { render "$@" | tr '\n' ' ' | tr -s ' '; }

# expect_contains NAME PATTERN -- args...
expect_contains() { local n=$1 p=$2; shift 3; flat "$@" | grep -Eq -- "$p" && ok "$n" || bad "$n (missing: $p)"; }
expect_absent()   { local n=$1 p=$2; shift 3; flat "$@" | grep -Eq -- "$p" && bad "$n (unexpected: $p)" || ok "$n"; }
expect_fail()     { local n=$1 p=$2; shift 3; local out; out=$(render "$@"); if [ $? -ne 0 ] && grep -Eq -- "$p" <<<"$out"; then ok "$n"; else bad "$n (expected failure matching: $p)"; fi; }

echo "required values"
expect_fail "url is required"            'pfsense.url is required'   -- --set pfsense.existingSecret=x --set 'config.domains={a.lan}'
expect_fail "domains are required"       'config.domains is required' -- --set pfsense.url=https://x --set pfsense.existingSecret=x
expect_fail "an API key source is required" 'existingSecret'         -- --set pfsense.url=https://x --set 'config.domains={a.lan}'
expect_fail "bad url is rejected by the schema" 'pfsense.url'        -- --set pfsense.url=ftp-nope --set pfsense.existingSecret=x --set 'config.domains={a.lan}'

echo "environment"
expect_contains "domains list is joined"   'name: DNS_DOMAINS value: "a.lan,b.lan"' -- "${BASE[@]}" --set 'config.domains={a.lan,b.lan}'
expect_contains "domains string accepted"  'name: DNS_DOMAINS\s+value: "c.lan"'       -- --set pfsense.url=https://x --set pfsense.existingSecret=x --set config.domains=c.lan
expect_contains "owner id"                 'name: OWNER_ID\s+value: "my-cluster"'     -- "${BASE[@]}" --set config.ownerId=my-cluster
expect_contains "dry run on"               'name: DRY_RUN\s+value: "true"'            -- "${BASE[@]}" --set config.dryRun=true
expect_contains "adopt on"                 'name: ADOPT_EXISTING\s+value: "true"'     -- "${BASE[@]}" --set config.adoptExisting=true
expect_contains "image tag defaults to appVersion" 'image: "ghcr.io/mevijays/kube-pfsense-dns-controller:0.1.0"' -- "${BASE[@]}"
expect_contains "image tag override"       'controller:9.9.9"'                        -- "${BASE[@]}" --set image.tag=9.9.9
expect_contains "log level"                '--zap-log-level=debug'                    -- "${BASE[@]}" --set config.logLevel=debug

echo "secrets"
expect_contains "existing secret is mounted" 'secretName: pfsense-api'                -- "${BASE[@]}"
expect_contains "key is read from the mounted file" 'name: PFSENSE_API_KEY_FILE value: /etc/pfsense-api/api-key' -- "${BASE[@]}"
expect_absent   "key is not injected as an env var" 'name: PFSENSE_API_KEY value' -- "${BASE[@]}"
expect_absent   "key is not read via secretKeyRef" 'secretKeyRef'                       -- "${BASE[@]}"
expect_contains "inline key Secret is mounted" 'secretName: rel-kube-pfsense-dns-controller' -- --set pfsense.url=https://x --set pfsense.apiKey=abc --set 'config.domains={a.lan}'
expect_contains "lease name is per release" 'name: LEADER_ELECTION_ID value: "rel-kube-pfsense-dns-controller"' -- "${BASE[@]}"
expect_contains "lease name follows fullnameOverride" 'name: LEADER_ELECTION_ID value: "dns-a"' -- "${BASE[@]}" --set fullnameOverride=dns-a
expect_absent   "no Secret object when using an existing one" 'kind: Secret'          -- "${BASE[@]}"
expect_contains "inline key creates a Secret" 'kind: Secret'                          -- --set pfsense.url=https://x --set pfsense.apiKey=abc --set 'config.domains={a.lan}'
expect_contains "custom secret key"        'key: token path: api-key'                 -- "${BASE[@]}" --set pfsense.existingSecretKey=token
expect_contains "CA secret is mounted"     'PFSENSE_CA_FILE'                          -- "${BASE[@]}" --set pfsense.caSecret=pf-ca
expect_absent   "no CA mount by default"   'pfsense-ca'                               -- "${BASE[@]}"

echo "rbac"
expect_contains "cluster read access"      'resources: \["httproutes", "gateways"\]'  -- "${BASE[@]}"
expect_contains "istio read access"        'virtualservices'                          -- "${BASE[@]}"
expect_absent   "no istio rules when disabled" 'virtualservices'                      -- "${BASE[@]}" --set config.enableIstio=false
expect_absent   "no gateway-api rules when disabled" 'httproutes'                     -- "${BASE[@]}" --set config.enableGatewayAPI=false
expect_contains "role per istio namespace" 'namespace: ingress-a'                     -- "${BASE[@]}" --set 'config.istioGatewayNamespaces={ingress-a,ingress-b}'
expect_contains "second istio namespace"   'namespace: ingress-b'                     -- "${BASE[@]}" --set 'config.istioGatewayNamespaces={ingress-a,ingress-b}'
expect_absent   "no rbac when disabled"    'kind: ClusterRole'                        -- "${BASE[@]}" --set rbac.create=false
expect_contains "leader election lease role" 'leases'                                 -- "${BASE[@]}"
expect_absent   "no lease role when disabled" 'leases'                                -- "${BASE[@]}" --set leaderElection.enabled=false
# the controller must never get more than read access, except for its own lease
for variant in "" "--set config.enableIstio=false" "--set config.enableGatewayAPI=false" "--set leaderElection.enabled=false" "--set config.istioGatewayNamespaces={a,b}"; do
  # shellcheck disable=SC2086
  out=$(render "${BASE[@]}" $variant | python3 "$(dirname "$0")/rbac_check.py" 2>&1) && ok "rbac is least-privilege ${variant:+($variant)}" || bad "rbac too broad ${variant}: $out"
done

echo "workload"
expect_contains "non-root"                 'runAsNonRoot: true'                       -- "${BASE[@]}"
expect_contains "read-only root fs"        'readOnlyRootFilesystem: true'             -- "${BASE[@]}"
expect_contains "replicas"                 'replicas: 2'                              -- "${BASE[@]}" --set replicaCount=2
expect_contains "service account name override" 'serviceAccountName: custom-sa'       -- "${BASE[@]}" --set serviceAccount.name=custom-sa
expect_contains "node selector"            'disktype: ssd'                            -- "${BASE[@]}" --set nodeSelector.disktype=ssd

echo "metrics"
expect_absent   "no metrics service by default" 'kind: Service '                       -- "${BASE[@]}"
expect_contains "metrics service"          'kind: Service '                            -- "${BASE[@]}" --set metrics.service.enabled=true
expect_contains "service monitor"          'kind: ServiceMonitor'                     -- "${BASE[@]}" --set metrics.service.enabled=true --set metrics.serviceMonitor.enabled=true
expect_absent   "service monitor needs the service" 'kind: ServiceMonitor'            -- "${BASE[@]}" --set metrics.serviceMonitor.enabled=true

echo "naming"
expect_contains "release name is used"     'name: rel-kube-pfsense-dns-controller'    -- "${BASE[@]}"
expect_contains "fullnameOverride"         'name: dns-sync( |$)'                          -- "${BASE[@]}" --set fullnameOverride=dns-sync

echo "plain manifests (deploy/)"
DEPLOY="$(dirname "$0")/../deploy/deployment.yaml"
grep -Eq 'image: [^ ]+:v[0-9]' "$DEPLOY" && bad "deploy image tag must not start with v (release tags v1.2.3 publish image tag 1.2.3)" || ok "deploy image tag has no v prefix"
grep -q 'PFSENSE_API_KEY_FILE' "$DEPLOY" && ok "deploy reads the key from a mounted file" || bad "deploy does not use PFSENSE_API_KEY_FILE"

echo "kubectl client-side validation of the default render"
if command -v kubectl >/dev/null && kubectl version --request-timeout=3s >/dev/null 2>&1; then
  render "${BASE[@]}" --namespace t | kubectl apply --dry-run=client --validate=false -f - >/dev/null 2>&1 && ok "manifests are valid Kubernetes objects" || bad "kubectl rejected the rendered manifests"
else
  echo "  skip  no reachable cluster for kubectl validation"
fi

[ $fail -eq 0 ] && echo "ALL CHART TESTS PASSED" || { echo "SOME CHART TESTS FAILED"; exit 1; }
