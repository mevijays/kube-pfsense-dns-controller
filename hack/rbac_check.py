#!/usr/bin/env python3
"""Reads rendered manifests on stdin and fails if the controller's RBAC is broader than intended.

Allowed: read verbs (get/list/watch) on the listed resources; write verbs ONLY on its own
leases, and create/patch on events (recorded by leader election), both in its own namespace.
"""
import sys
import yaml

READ = {"get", "list", "watch"}
errors = []
for doc in yaml.safe_load_all(sys.stdin):
    if not doc or doc.get("kind") not in ("Role", "ClusterRole"):
        continue
    name = doc["metadata"]["name"]
    for rule in doc.get("rules", []):
        verbs, res = set(rule.get("verbs", [])), set(rule.get("resources", []))
        if "*" in verbs or "*" in res or "*" in rule.get("apiGroups", []):
            errors.append(f"{name}: wildcard in {rule}")
        writes = verbs - READ
        if writes and res != {"leases"} and not (res == {"events"} and writes <= {"create", "patch"}):
            errors.append(f"{name}: write verbs {sorted(writes)} on {sorted(res)}")
        if writes and doc["kind"] == "ClusterRole":
            errors.append(f"{name}: write verbs must never be cluster-wide")
        if "secrets" in res:
            errors.append(f"{name}: must not read secrets")
        if doc["kind"] == "ClusterRole" and ("pods" in res or "services" in res):
            errors.append(f"{name}: pods/services must be namespaced Roles, not cluster-wide")
if errors:
    print("\n".join(errors))
    sys.exit(1)
