#!/usr/bin/env python3
"""Every key the ExternalSecret fetches must reach the container.

Reads a rendered manifest set on stdin and fails if the Secret carries a key
that the Deployment never reads. With --reads, it also fails the other way
round: a secret the binary reads that the Deployment never passes in.

The two ways a key reaches a container differ, and that difference is the bug
this exists for. The ConfigMap comes in whole through envFrom, so a key added to
configmap.k is live by construction. The Secret does NOT: deploy.k names its
keys one at a time in _secretEnv. A key added to externalsecret.k and forgotten
there is fetched from Vault, lands in the Secret, and sits in nobody's
environment -- nothing errors, the pod is healthy, and the feature quietly does
not work. v0.4.0 shipped SCHMETTERPAUSE_TOKEN exactly like that, and every
handover to schmetterpause would have come back 401.

A render cannot see it and a compile cannot see it. This can.

The first half only knows what the ExternalSecret fetches, so a key missing
from BOTH externalsecret.k and deploy.k passes it. That is how CHECKIN_TOKEN
went out (#121): the binary read it, no container got it, and with secretsMode
"existing" -- a Secret written by hand, no ExternalSecret at all -- there was
nothing to compare against. --reads takes the other end as the truth: every
name in the binary's source that looks like a secret has to arrive in the
scorekeeper's environment from the Secret.
"""
import argparse
import pathlib
import re
import sys

import yaml

# A string literal in the binary that names a secret. The binary reads its
# environment by literal name (os.Getenv, or env() in main.go), and secrets are
# named for what they are, so a suffix is enough to find them without listing
# them here -- a list here would be one more place to forget the next token.
_SECRET_NAME = re.compile(r'"([A-Z][A-Z0-9_]*_(?:TOKEN|PASSWORD|SECRET|KEY))"')

# The Deployment that runs the binary under --reads. zaehlwerk-pong's carries
# "game" and reads no secret.
_COMPONENT = "scorekeeper"


def secrets_read_by(source_dir: str) -> set:
    names = set()
    for f in pathlib.Path(source_dir).rglob("*.go"):
        if f.name.endswith("_test.go"):
            continue
        names.update(_SECRET_NAME.findall(f.read_text()))
    if not names:
        # A rename of the directory or of os.Getenv's spelling would otherwise
        # turn this check into a silent pass.
        raise SystemExit(f"FAIL  found no secret names in {source_dir} -- is it the binary's source?")
    return names


def passed_from_secret(docs: list) -> tuple:
    """The env names the scorekeeper gets from a Secret, and whether it gets a
    whole Secret through envFrom (which passes every key)."""
    names, wholesale, found = set(), False, False
    for d in docs:
        if d.get("kind") != "Deployment":
            continue
        if d["metadata"].get("labels", {}).get("app.kubernetes.io/component") != _COMPONENT:
            continue
        found = True
        for c in d["spec"]["template"]["spec"]["containers"]:
            for e in c.get("env", []) or []:
                ref = (e.get("valueFrom") or {}).get("secretKeyRef")
                # The name the process sees is e["name"]; the key it comes
                # from may differ, and only the first is what os.Getenv asks.
                if ref:
                    names.add(e["name"])
            wholesale = wholesale or any("secretRef" in s for s in c.get("envFrom", []) or [])
    if not found:
        raise SystemExit(f"FAIL  no Deployment with component {_COMPONENT} in the render")
    return names, wholesale


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--reads", metavar="DIR",
                        help="the binary's source; fail if a secret it reads is not passed in")
    args = parser.parse_args()

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

    if args.reads:
        passed, wholesale = passed_from_secret(docs)
        missing = [] if wholesale else sorted(secrets_read_by(args.reads) - passed)
        if missing:
            for name in missing:
                print(f"FAIL  {args.reads} reads {name}, but the {_COMPONENT} Deployment never gets it "
                      f"from a Secret -- name it in _secretEnv in kcl/deploy.k")
            return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
