# Security policy

This controller holds a **pfSense API key** and can create and delete DNS records,
so security reports are taken seriously.

## Reporting a vulnerability

Please **do not open a public issue**. Report it privately through GitHub:
**Security tab -> "Report a vulnerability"** on this repository
(https://github.com/mevijays/kube-pfsense-dns-controller/security/advisories/new).

Include what you found, how to reproduce it, and the versions involved. You can expect an
acknowledgement within a few days. Fixes are released as soon as practical and credited
in the advisory unless you prefer otherwise.

## Supported versions

The project is pre-1.0. Only the latest release receives fixes.

## Deployment hardening notes

- Keep the API key in a Kubernetes Secret (as in `deploy/deployment.yaml`); never pass it as a flag.
- Give the pfSense API user only the privileges it needs for DNS Resolver host overrides.
- Verify pfSense's certificate (`PFSENSE_CA_FILE`) instead of `PFSENSE_INSECURE_SKIP_VERIFY` where you can.
- The controller trusts the cluster: anyone who can create a `VirtualService` or `HTTPRoute`
  with a hostname under a managed domain can cause a DNS record for it. Restrict who can create
  routes (RBAC, Gateway `allowedRoutes`, admission policy) accordingly.
- Start with `DRY_RUN=true` and review the logs before enabling writes.
