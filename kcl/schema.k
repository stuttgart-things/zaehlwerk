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
    vaultKeyRedisPassword: str = "redis-password"

    # ── Escape hatches ──────────────────────────────────────────────────────
    labels: {str:str} = {}
    extraEnvVars: {str:str} = {}

    check:
        replicas == 1, "zaehlwerk keeps the running match in memory and is its only writer (ADR-0001); a second replica keeps a second score. Shared state has to come first."
        secretsMode in ["none", "external", "existing"], "secretsMode is none, external or existing"
        not httpRouteEnabled or gatewayName != "", "httpRouteEnabled needs a gatewayName"
        not (secretsMode == "external") or secretStoreName != "", "secretsMode external needs a secretStoreName"

