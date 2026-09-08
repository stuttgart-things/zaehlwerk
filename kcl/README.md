# Kubernetes manifests

KCL module for running zaehlwerk on Kubernetes. Renders a ServiceAccount, a
ConfigMap, an ExternalSecret, a Deployment, a Service and an HTTPRoute —
depending on what the profile switches on.

Everything is a variable. No hostname, no namespace, no gateway and no image
reference is fixed in the module, so the same manifests fit a laptop cluster,
the office cluster and a preview namespace.

## Rendering

```sh
task kcl:render                      # profile base
task kcl:render PROFILE=~/office.yaml
task kcl:check                       # do all profiles still render?
task kcl:apply                       # render and apply to the current context
task kcl:publish TAG=v1.2.3          # base to GHCR as an OCI artefact
```

Overriding single values — everything after `--` goes to `kcl`, behind the
profile's own flags, and a later `-D` wins:

```sh
task kcl:render -- -D config.gatewayName=cilium-gateway
task kcl:render -- -D config.image=ghcr.io/stuttgart-things/zaehlwerk:v0.1.0
```

**Calling `kcl run` directly is the way this goes wrong.** `-D` loads no
profile, and neither does `-Y kcl/profiles/base.yaml` — the profiles are flat
`key: value` files, not KCL settings files. Either way every field not passed
falls back to its default in `schema.k`, and those defaults are the bare
deployment: no panel, no route, no secret.

### Why the output looks the way it does

`kcl run` emits a list under the key `manifests:`. That is not an aesthetic
slip but a **contract**: `stuttgart-things/dagger/kcl` — the module that turns
this into a kustomize base and pushes it as an OCI artefact — normalises the
output with a fixed `sed` chain that assumes exactly this shape: drop the first
line, de-indent by two, turn `- ` at column zero into a document separator.

A YAML stream (`manifests.yaml_stream`) looks nicer and is directly usable with
`kubectl apply -f` — but put through the same chain, the first resource loses
its `apiVersion` and every `metadata` field is lifted to the top level. The
result still parses as YAML; it just deploys something else.

**Hence `task kcl:render` rather than `kcl run`.** The task applies the same
normalisation, so what appears locally is what ends up in the OCI artefact. Its
output is `---`-separated and usable with `kubectl apply -f`.

### A render with no profile still works here

Unlike the module next door in schmetterpause, a bare render is not an error.
Its defaults leave the panel off, the route off and the secret unrendered,
which is the `run:bare` deployment — a scoreboard serving its page with nothing
downstream. That is a real way to run this service, so it is what the defaults
describe.

## What the schema refuses

Four checks in `schema.k`, and the first is the one worth knowing:

| | |
| --- | --- |
| `replicas == 1` | The running match lives in this process's memory and this service is its only writer (`docs/adr/0001`). A second pod does not share load, it keeps a second score — a point routed to one pod is invisible to the other, and there is no session affinity that fixes it, because the panel is fed by whichever pod took the point. Scaling out needs shared state, which ADR-0004 declined to introduce. |
| `secretsMode in [none, external, existing]` | A typo would otherwise render no secret and no error. |
| `httpRouteEnabled` needs a `gatewayName` | A route to no gateway is accepted by the API server and reaches nobody. |
| `secretsMode: external` needs a `secretStoreName` | Same failure shape: an ExternalSecret pointing at no store stays unresolved and silent. |

## Secrets

Two values must not be readable from the namespace: `OMNI_PITCHER_TOKEN`, the
bearer token `/pitch` answers 401 without, and `REDIS_PASSWORD`. Everything
else is in the ConfigMap.

- `secretsMode: none` — the default. No panel, so no secret to have.
- `secretsMode: external` — renders an ExternalSecret against the cluster's
  store.
- `secretsMode: existing` — renders nothing and expects a Secret named
  `<name>-panel` to be in the namespace already. For a cluster without the
  External Secrets Operator.

Both keys are read with `optional: true`, so a Secret carrying only one of them
does not stop the pod from starting — which is the normal case, since the two
panel paths are alternatives rather than a pair.

## What an environment patches

The published base is neutral. An environment's Argo Application patches these:

```yaml
config.namespace: zaehlwerk
config.clusterDomain: <the cluster's domain>
config.gatewayName: <gateway>
config.gatewayNamespace: <its namespace>
config.omniPitcherURL: <the omni-pitcher>   # or config.redisAddr
config.allowedOrigins: <schmetterpause's origin, once its live tab exists>
config.secretStoreName: vault-<cluster>
config.vaultPath: zaehlwerk
```

`config.image` is not in that list: `task kcl:publish` and CI both override it
with the tag the artefact itself carries, so the profile's `:latest`
placeholder never reaches a published base.

## What is not rendered

**No Namespace.** Several Applications can share a workload namespace, and when
more than one ships a Namespace resource ArgoCD flags it as a SharedResource —
then one Application's prune cycle deletes the namespace out from under the
others. The consuming Application sets `CreateNamespace=true` instead.

**No Redis and no led-catcher.** Both are things this service talks to rather
than things it owns. They outlive any revision of it, they are shared with
everything else on the homerun bus, and a base that carried them would be a
base whose removal can prune somebody else's panel.

**No readiness probe.** `/healthz` is a liveness probe and touches neither
Redis nor the omni-pitcher. The reasoning is in the repository README under
"There is no `/readyz`".
