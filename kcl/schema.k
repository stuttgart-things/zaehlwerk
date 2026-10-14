"""
Typed configuration for the zaehlwerk manifests.

Everything is a variable. No hostname, namespace, gateway, cluster domain or
image reference is baked in, so the same module renders for a laptop cluster,
the office cluster and a preview namespace.

Nothing here can hold a secret value, and nothing ever should — the two secrets
this service can have are the omni-pitcher bearer token and the Redis password,
and both come from a Secret. See externalsecret.k.
"""

schema Zaehlwerk:
    """Configuration for the zaehlwerk Kubernetes manifests."""
    # ── Identity ────────────────────────────────────────────────────────────
    name: str = "zaehlwerk"
    namespace: str = "zaehlwerk"

    # ── Image ───────────────────────────────────────────────────────────────
    
    # A placeholder for local renders. task kcl:publish always overrides it
    # with the tag the artefact itself carries, so it cannot reach a published
    # base.
    image: str = "ghcr.io/stuttgart-things/zaehlwerk:latest"
    imagePullPolicy: str = "IfNotPresent"

    # ── Scale ───────────────────────────────────────────────────────────────
    
    # One replica, and the check below is a hard error rather than advice.
    #
    # This service holds the running match in memory and is the only writer of
    # that state (docs/adr/0001). A second pod does not share the load, it
    # keeps a second score: a point routed to one pod is invisible to the
    # other, and an SSE client reconnecting through the Service can land on the
    # pod that never saw the match. There is no session affinity that fixes
    # this, because the LED panel is fed by whichever pod took the point.
    #
    # Scaling out needs shared state first, which is a decision ADR-0004
    # explicitly declined to make. Until then, more than one replica is not a
    # tuning mistake but a wrong answer.
    replicas: int = 1

    cpuRequest: str = "20m"
    cpuLimit: str = "200m"
    memoryRequest: str = "32Mi"
    memoryLimit: str = "128Mi"

    # ── Ports ───────────────────────────────────────────────────────────────
    servicePort: int = 80
    containerPort: int = 8080

    # ── Exposure ────────────────────────────────────────────────────────────
    httpRouteEnabled: bool = False
    gatewayName: str = ""
    gatewayNamespace: str = ""
    gatewaySectionName: str = "https"
    clusterDomain: str = ""
    # Defaults to "<name>.<clusterDomain>" when both are set.
    host: str = ""

    # ── The service's own configuration ─────────────────────────────────────
    logLevel: str = "INFO"
    maxRetainedMatches: int = 200
    streamHeartbeat: str = "25s"

    # Browser origins that may read the live stream. Empty means none, which is
    # what the code defaults to — never "*", see internal/api/stream.go.
    allowedOrigins: str = ""

    # ── The panel ───────────────────────────────────────────────────────────
    #
    # Two ways onto the homerun bus and both are optional: unset, the service
    # scores and serves and pitches nothing. omniPitcherURL wins over redisAddr
    # where both are set, which is the precedence main.go applies.
    panelStream: str = "tabletennis"

    # Schmetterpause's base URL. Empty, and a finished match is reported
    # nowhere: it is still scored, shown and pitched to the panel, which
    # docs/adr/0004 keeps as a first-class way to run rather than a degraded
    # one. Set it and the scoring page offers the players that application
    # knows and hands the result over at the last point.
    #
    # The token is not here: it is a secret, and it comes through the
    # ExternalSecret like OMNI_PITCHER_TOKEN does.
    schmetterpauseURL: str = ""

    # Trust the cluster's own CA. trust-manager publishes the bundle into every
    # namespace as a ConfigMap, and the certificate on the cluster gateway is
    # signed by an internal CA that the image's public roots do not contain --
    # so without this, reaching schmetterpause over its HTTPRoute fails with
    # "x509: certificate signed by unknown authority" while the pod looks
    # healthy (measured against v0.4.1, 2026-09-16).
    #
    # Mounted as a directory and named in SSL_CERT_DIR, which ADDS to the
    # image's roots rather than replacing them. SSL_CERT_FILE would replace
    # them, and only keeps public TLS working where the bundle happens to carry
    # public roots too. The volume is optional: on a cluster without
    # trust-manager the mount is empty, public TLS is unaffected, and only the
    # internal CA stays untrusted -- exactly as without this setting. So it is
    # on by default.
    caBundleEnabled: bool = True
    caConfigMap: str = "cluster-trust-bundle"
    caKey: str = "trust-bundle.pem"
    omniPitcherURL: str = ""
    omniPitcherPath: str = ""
    redisAddr: str = ""
    redisPort: int = 6379

    # ── The catcher ─────────────────────────────────────────────────────────
    catcherURL: str = ""
    catcherMatchStreams: str = ""
    catcherIdleStreams: str = ""
    catcherIdleTimeout: str = ""

    # ── Secrets ─────────────────────────────────────────────────────────────
    #
    # "external" renders an ExternalSecret and expects a store; "existing"
    # renders nothing and expects a Secret of that name to be there already.
    # "none" is for a deployment with no panel at all, where there is no secret
    # to have.
    secretsMode: str = "none"
    secretStoreKind: str = "ClusterSecretStore"
    secretStoreName: str = ""
    externalSecretApiVersion: str = "external-secrets.io/v1"
    refreshInterval: str = "1h"
    vaultPath: str = "zaehlwerk"
    vaultKeyPitcherToken: str = "omni-pitcher-token"
    vaultKeySchmetterpauseToken: str = "schmetterpause-token"
    vaultKeyRedisPassword: str = "redis-password"

    # checkinEnabled adds CHECKIN_TOKEN to the ExternalSecret and nothing else
    # (#113). Off, the ExternalSecret renders as it did before, so a Vault
    # entry without the key never leaves the Secret unresolved. deploy.k reads
    # the key either way, optionally: on a cluster with secretsMode "existing"
    # it is one more key in the Secret, and without it check-in stays off.
    checkinEnabled: bool = False
    vaultKeyCheckinToken: str = "checkin-token"

    # ── zaehlwerk-pong (ADR-0006) ───────────────────────────────────────────
    #
    # The game, as a second Deployment next to zaehlwerk, playing against it
    # through its Service. Off by default, and with it off not one rendered
    # line changes. It is its own pod rather than a sidecar so that it can be
    # switched off, scaled to nothing or restarted without touching the
    # scorekeeper, which owns the running match.
    pongEnabled: bool = False

    # Empty is zaehlwerk's image with -pong added to the repository and the
    # same tag, so a base published as zaehlwerk:v0.5.0 pins
    # zaehlwerk-pong:v0.5.0 without a second parameter — the two are built
    # from one commit and released together.
    pongImage: str = ""
    pongContainerPort: int = 8081
    pongCpuRequest: str = "10m"
    pongCpuLimit: str = "200m"
    pongMemoryRequest: str = "16Mi"
    pongMemoryLimit: str = "64Mi"
    pongBestOf: int = 3
    pongBotSkill: str = "mittel"

    # Empty leaves the variable out, which is what switches each of these
    # off or to the code's default: no faults, a clock seed, no attract mode.
    pongFaults: str = ""
    pongSeed: str = ""
    # e.g. "10m". Unset, the game only plays when somebody starts it.
    pongAttractAfter: str = ""
    pongAttractPace: str = ""
    pongBrowserIdle: str = ""

    # A route of its own, on the same gateway, at "<name>-pong.<clusterDomain>"
    # unless pongHost says otherwise. Without it the game is reachable inside
    # the cluster only, which is enough for attract mode — a demo needs no
    # browser, the panel shows it.
    pongHttpRouteEnabled: bool = False
    pongHost: str = ""

    # ── Escape hatches ──────────────────────────────────────────────────────
    labels: {str:str} = {}
    extraEnvVars: {str:str} = {}

    check:
        replicas == 1, "zaehlwerk keeps the running match in memory and is its only writer (ADR-0001); a second replica keeps a second score. Shared state has to come first."
        secretsMode in ["none", "external", "existing"], "secretsMode is none, external or existing"
        not httpRouteEnabled or gatewayName != "", "httpRouteEnabled needs a gatewayName"
        not (secretsMode == "external") or secretStoreName != "", "secretsMode external needs a secretStoreName"
        not pongHttpRouteEnabled or pongEnabled, "pongHttpRouteEnabled routes to a game that pongEnabled does not render"
        not pongHttpRouteEnabled or gatewayName != "", "pongHttpRouteEnabled needs a gatewayName"
        pongBestOf >= 1 and pongBestOf % 2 == 1, "pongBestOf is an odd number, 1 or more"
        not pongEnabled or pongImage != "" or "@" not in image, "image is pinned by digest, and the game's image has a digest of its own: set pongImage"
