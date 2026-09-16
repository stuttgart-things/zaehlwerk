#!/usr/bin/env python3
"""Every key the ExternalSecret fetches must reach the container.

Reads a rendered manifest set on stdin and fails if the Secret carries a key
that the Deployment never reads.

The two ways a key reaches a container differ, and that difference is the bug
this exists for. The ConfigMap comes in whole through envFrom, so a key added to
configmap.k is live by construction. The Secret does NOT: deploy.k names its
keys one at a time in _secretEnv. A key added to externalsecret.k and forgotten
there is fetched from Vault, lands in the Secret, and sits in nobody's
environment -- nothing errors, the pod is healthy, and the feature quietly does
not work. v0.4.0 shipped SCHMETTERPAUSE_TOKEN exactly like that, and every
handover to schmetterpause would have come back 401.

A render cannot see it and a compile cannot see it. This can.
"""
import sys

import yaml


def main() -> int:
    docs = [d for d in yaml.safe_load_all(sys.stdin) if d]
    # task kcl:render already flattens `manifests:`; accept the raw shape too.
    if len(docs) == 1 and isinstance(docs[0], dict) and "manifests" in docs[0]:
        docs = docs[0]["manifests"]

    fetched = set()
    consumed = set()
    wholesale_secrets = set()

    for d in docs:
        kind = d.get("kind")
        if kind == "ExternalSecret":
            target = d["spec"].get("target", {}).get("name") or d["metadata"]["name"]
            for entry in d["spec"].get("data", []):
                fetched.add((target, entry["secretKey"]))
        if kind == "Deployment":
            for c in d["spec"]["template"]["spec"]["containers"]:
                for e in c.get("env", []) or []:
                    ref = (e.get("valueFrom") or {}).get("secretKeyRef")
                    if ref:
                        consumed.add((ref["name"], ref["key"]))
                for src in c.get("envFrom", []) or []:
                    if "secretRef" in src:
                        wholesale_secrets.add(src["secretRef"]["name"])

    orphaned = sorted(
        key for key in fetched
        if key not in consumed and key[0] not in wholesale_secrets
    )
    if orphaned:
        for secret, key in orphaned:
            print(f"FAIL  {secret}/{key} is fetched into the Secret but no container reads it "
                  f"-- name it in _secretEnv in kcl/deploy.k")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
