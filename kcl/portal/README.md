# The dev portal on Kubernetes

KCL module for the dev portal (`tools/chain-mock/portal`, image
`ghcr.io/stuttgart-things/zaehlwerk-portal`): a ConfigMap, a Deployment, a
Service and an HTTPRoute. It is published as its own kustomize base,
`ghcr.io/stuttgart-things/zaehlwerk-portal-kustomize`, under the same tags as
zaehlwerk's (`main` on every push, the version and `latest` on a release), so
a cluster gets a portal by pointing an OCIRepository at it, not by taking
zaehlwerk's next base.

```sh
task kcl:render MODULE=kcl/portal
task kcl:render MODULE=kcl/portal -- -D config.pongURL=http://zaehlwerk-pong
task kcl:check          # checks this module too
```

## Two addresses per part

The portal calls each part and hands the browser that part's page to frame.
In a cluster those are two different addresses: a Service the browser cannot
resolve, and a route the pod has no business going round through. So every
part is a pair:

| Option | ConfigMap key | |
| --- | --- | --- |
| `config.zaehlwerkURL` | `ZAEHLWERK_URL` | `http://zaehlwerk`, the Service next door. Cannot be `off` |
| `config.zaehlwerkPublicURL` | `ZAEHLWERK_PUBLIC_URL` | zaehlwerk's route |
| `config.schmetterpauseURL` / `…PublicURL` | `SCHMETTERPAUSE_URL` / `…_PUBLIC_URL` | the fake or the real one; `off` by default |
| `config.pongURL` / `…PublicURL` | `PONG_URL` / `…_PUBLIC_URL` | `off` by default |
| `config.buttonsURL` / `…PublicURL` | `BUTTONS_URL` / `…_PUBLIC_URL` | the table mock; `off` by default |
| `config.piezoControlURL` | `PIEZO_CONTROL_URL` | a piezo board's control endpoint, no page; `off` by default |
| `config.simulatorURL` / `…PublicURL` | `SIMULATOR_URL` / `…_PUBLIC_URL` | a led-catcher web simulator; `off` by default |

`off` is a part the cluster does not run: no status chip, no frame, no
buttons. That is different from a part that is there and down, which is red —
a cluster with real buttons has no table mock, and a red chip for it forever
would teach everybody to ignore the red ones. Every `<PART>_URL` is rendered,
`off` included, so an environment patches a key that is there.

The Deployment mounts trust-manager's `cluster-trust-bundle` as zaehlwerk's
does (`config.caBundleEnabled`, on), so a Schmetterpause behind the cluster's
own CA answers over HTTPS.

## The route is the access control

The portal has no login. Whoever reaches its hostname can end the running
match, start one, and where those parts are there pause a piezo board or start
a pong game — what zaehlwerk's and pong's own routes already allow. It belongs
on the same internal gateway as theirs.

A frame is the part's own page, so a page that refuses to be framed, or a
login whose cookie is not sent into a frame from another site, shows as a
blank or signed-out frame. *Open ↗* above every frame opens the page itself.

## What an environment patches

```yaml
config.clusterDomain: <the cluster's domain>
config.gatewayName: <gateway>
config.gatewayNamespace: <its namespace>
config.zaehlwerkPublicURL: https://zaehlwerk.<domain>
# and for each part the cluster runs, e.g.:
config.pongURL: http://zaehlwerk-pong
config.pongPublicURL: https://zaehlwerk-pong.<domain>
config.schmetterpauseURL: https://schmetterpause.<domain>
```

With Flux, the same as patches on the base: the HTTPRoute's `parentRefs` and
`hostnames`, and the ConfigMap `zaehlwerk-portal-config`'s keys.
