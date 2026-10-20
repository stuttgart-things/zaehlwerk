# Kubernetes manifests

KCL module for running zaehlwerk on Kubernetes. Renders a ServiceAccount, a
ConfigMap, an ExternalSecret, a Deployment, a Service and an HTTPRoute —
depending on what the profile switches on — and, with `config.pongEnabled`,
zaehlwerk-pong next to it.

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

Seven checks in `schema.k`, and the first is the one worth knowing:

| | |
| --- | --- |
| `replicas == 1` | The running match lives in this process's memory and this service is its only writer (`docs/adr/0001`). A second pod does not share load, it keeps a second score — a point routed to one pod is invisible to the other, and there is no session affinity that fixes it, because the panel is fed by whichever pod took the point. Scaling out needs shared state, which ADR-0004 declined to introduce. |
| `secretsMode in [none, external, existing]` | A typo would otherwise render no secret and no error. |
| `httpRouteEnabled` needs a `gatewayName` | A route to no gateway is accepted by the API server and reaches nobody. |
| `secretsMode: external` needs a `secretStoreName` | Same failure shape: an ExternalSecret pointing at no store stays unresolved and silent. |
| `pongHttpRouteEnabled` needs `pongEnabled` and a `gatewayName` | A route to a game that is not rendered, or to no gateway, reaches nobody. |
| `pongBestOf` is odd | zaehlwerk would refuse it too, but only when the first game starts — in a demo nobody is watching the log of. |
| `pongEnabled` with a digest-pinned `image` needs `pongImage` | The game's image is derived from zaehlwerk's by its tag. A digest names one image, and the game's is another. |

## The route is the access control

`httpRouteEnabled` puts one rule at `PathPrefix: /` in front of everything, and
**that route belongs on an internal gateway.** There is no authentication in
this service — no token, no session — so whoever reaches the hostname can start
a match, score it, undo, end it, and with `CATCHER_URL` set take the LED panel.

The single rule is a deliberate refusal to pretend otherwise. The endpoints
fall into three groups:

| | |
| --- | --- |
| `/ui/…` | the scoring page: form posts, undo, end, and its own SSE stream |
| `/matches/…` | the JSON API, including the live stream and the lifecycle POSTs |
| `/ingest/…` | button, piezo and phone |

The page is self-contained, so a `/ui`-only route would serve it completely.
It would also grant everything the API grants, because the page starts, scores
and ends matches through its own routes — so the narrower rule would shrink the
surface without removing a single capability. That is a boundary that reads as
protection and is not one.

What would genuinely be exposed further is `GET /matches/{id}/stream`,
read-only, for the Schmetterpause live tab that `docs/adr/0001` describes —
and that needs `config.allowedOrigins` set, which is empty by default and never
`*`. A second, public route for that endpoint alone is the shape to reach for;
this module does not render one yet, because nothing asks for it yet.

## zaehlwerk-pong

The game (`docs/adr/0006`) can run next to zaehlwerk, for a fair or the office,
where attract mode keeps the panel alive while nobody plays. It is a part of
this module rather than a module of its own, because it is released with
zaehlwerk and plays against exactly this Deployment's Service — and it is off
unless a profile switches it on. With it off, not one line of the render
changes; `task kcl:check` proves that, and proves the four resources it adds
when it is on.

```sh
task kcl:render -- -D config.pongEnabled=true -D config.pongAttractAfter=10m
```

| Option | Default | |
| --- | --- | --- |
| `config.pongEnabled` | `false` | a ConfigMap, a Deployment and a Service named `<name>-pong` |
| `config.pongImage` | zaehlwerk's image, `-pong` added, same tag | so `zaehlwerk:v0.5.0` pins `zaehlwerk-pong:v0.5.0` and CI needs no second parameter |
| `config.pongAttractAfter` | unset, off | `PONG_ATTRACT_AFTER`, e.g. `10m` |
| `config.pongAttractPace` | unset, `1s` in the code | `PONG_ATTRACT_PACE` |
| `config.pongBrowserIdle` | unset, `10m` in the code | `PONG_BROWSER_IDLE` |
| `config.pongBestOf` | `3` | `PONG_BEST_OF` |
| `config.pongBotSkill` | `mittel` | `PONG_BOT_SKILL` |
| `config.pongFaults`, `config.pongSeed` | unset | `PONG_FAULTS`, `PONG_SEED`. Faults never reach a demo |
| `config.pongHttpRouteEnabled` | `false` | a route at `<name>-pong.<clusterDomain>`, or `config.pongHost`, on zaehlwerk's gateway |

**Configuration is the environment and nothing else**, as for zaehlwerk: each
option is one variable in the game's ConfigMap, and one left unset is left out
— unset is how the code switches attract mode, faults and a fixed seed off.

**`ZAEHLWERK_URL` is the Service, `http://<name>.<namespace>.svc`.** The game
talks to zaehlwerk server-side only, so it needs no route, no certificate and no
`ALLOWED_ORIGINS` entry, and a demo needs no route of its own either: nobody has
to open its page for the panel to show it. The route is for starting a game in
a browser, and like zaehlwerk's it is the access control — the game has no
authentication — so it belongs on the same internal gateway.

**One replica and `Recreate`, not a field.** A game lives in the memory of the
pod playing it, a browser game's token is known to that pod only, and one game
at a time holds the table. Two pods would be two demos fighting over one panel.

The published base does not carry it: `base.yaml` leaves it off, and kustomize
cannot switch on a resource that was never rendered. An environment that wants
the game renders its own profile with `config.pongEnabled: true`.

## The dev portal

`kcl/portal` is a second module, for the dev portal, published as its own base
(`zaehlwerk-portal-kustomize`) so that no cluster gets it by updating
zaehlwerk. `task kcl:render MODULE=kcl/portal`; the rest is in
[`portal/README.md`](portal/README.md).

## Secrets

Two values must not be readable from the namespace: `OMNI_PITCHER_TOKEN`, the
bearer token `/pitch` answers 401 without, and `REDIS_PASSWORD`. Everything
else is in the ConfigMap, apart from the tokens of two optional couplings:
`SCHMETTERPAUSE_TOKEN`, and `CHECKIN_TOKEN` below.

- `secretsMode: none` — the default. No panel, so no secret to have.
- `secretsMode: external` — renders an ExternalSecret against the cluster's
  store.
- `secretsMode: existing` — renders nothing and expects a Secret named
  `<name>-panel` to be in the namespace already. For a cluster without the
  External Secrets Operator.

Both keys are read with `optional: true`, so a Secret carrying only one of them
does not stop the pod from starting — which is the normal case, since the two
panel paths are alternatives rather than a pair.

### `CHECKIN_TOKEN`

The bearer token the hub sends with a fob check-in (#113). zaehlwerk turns
check-in by fob on only with it set, so without it fob check-in simply stays
off: `/ingest/checkin` answers 503, `GET /checkin` leaves out `"fob"`, the
start log says `fob check-in disabled, CHECKIN_TOKEN not configured`, and
nothing else changes. Check-in by phone does not need it (ADR-0011).

The Deployment reads it from `<name>-panel` with `optional: true`, like
`SCHMETTERPAUSE_TOKEN`, in both `external` and `existing` mode.

- **`secretsMode: external`** — `config.checkinEnabled: true` adds the entry
  to the ExternalSecret, from the Vault key `checkin-token` under
  `config.vaultPath` (`config.vaultKeyCheckinToken` to name it otherwise).
  Off, the ExternalSecret renders exactly as before: a property missing in
  Vault fails the whole ExternalSecret, so it is asked for only once it is
  there. `task kcl:check` proves the switch adds that one entry and nothing
  else.
- **`secretsMode: existing`**, a cluster without ESO such as one run by Flux —
  add `CHECKIN_TOKEN` as one more key to the Secret `<name>-panel` that is
  already there. There is no switch to set; the Deployment already reads it.

The hub has to send the same value (zaehlwerk-firmware#65).

`task kcl:check` also fails when the binary reads a secret that the Deployment
never passes in (#121): every `…_TOKEN`, `…_PASSWORD`, `…_SECRET` or `…_KEY`
named in `cmd/zaehlwerk-api` has to arrive from `<name>-panel`, in `external`
and in `existing` mode. A new token is then one entry in `deploy.k`, and the
check says so before a cluster does.

## Trusting the cluster's CA

The certificate on a stuttgart-things cluster gateway is signed by an internal CA,
and the image carries only public roots. So without help, reaching schmetterpause
over its HTTPRoute fails with `x509: certificate signed by unknown authority` —
while the pod reports healthy and the coupling logs that it is enabled.

trust-manager already publishes that CA into every namespace as the ConfigMap
`cluster-trust-bundle`. The Deployment mounts it at `/etc/ssl/cluster-trust` and
sets `SSL_CERT_DIR` to that directory. On by default:

| Option | Default | |
| --- | --- | --- |
| `config.caBundleEnabled` | `true` | mount the bundle and point Go at it |
| `config.caConfigMap` | `cluster-trust-bundle` | the ConfigMap trust-manager publishes |
| `config.caKey` | `trust-bundle.pem` | its key |

**`SSL_CERT_DIR`, not `SSL_CERT_FILE`.** The directory form adds the bundle to
the image's roots; the file form replaces them and only keeps public TLS working
where the bundle happens to include public roots as well.

**The volume is optional, which is why this can default to on.** On a cluster
without trust-manager the mount is empty: public TLS is unaffected and only the
internal CA stays untrusted, exactly as with the option off.

Measured on 2026-09-16 in the image's own runtime (Alpine, a Go client) against
schmetterpause's gateway, including the symlink layout a ConfigMap volume really
has (`ca.pem -> ..data/ca.pem`):

| | schmetterpause via its HTTPRoute | public HTTPS |
| --- | --- | --- |
| no bundle | `x509: certificate signed by unknown authority` | ok |
| bundle via `SSL_CERT_DIR` | ok (401, the right answer without a token) | ok |
| empty mount, as without trust-manager | fails as before | ok |

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

**An unpatched placeholder is loud rather than broken.** Applied as it stands,
the base pitches to `omni-pitcher.example.com`, which does not resolve — the
match still scores, `/ui` still serves, and the log carries one line per point:

```
WARN panel pitch failed, point not shown
     error="… lookup omni-pitcher.example.com … no such host" title="1:0"
```

Measured on a cluster, not assumed. It is the optional-coupling invariant doing
its job, and it is also the signal that `config.omniPitcherURL` was never
patched. The placeholder cannot simply be empty: `configmap.k` leaves the key
out when the value is, and kustomize can patch a key it can see but not one
that was never rendered.

`config.image` is not in that list: `task kcl:publish` and CI both override it
with the tag the artefact itself carries, so the profile's `:latest`
placeholder never reaches a published base. CI does it through the
`kcl-parameters` input of `call-push-kustomize.yaml`, which wins over the
profile file — the base and the image it pins are published under the same
name, so `zaehlwerk-kustomize:v0.1.0` carries `zaehlwerk:v0.1.0` and not
whatever moved last.

`latest` moves on a release tag and nowhere else, under the same rule the image
follows, and it is applied with `oras tag` on the digest just pushed rather
than by publishing a second time. A second publish re-renders the base and
lands the two tags on two different digests while both report success — which
is what schmetterpause hit, and what the workflow now proves cannot happen by
comparing the digests and failing when they disagree.

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
