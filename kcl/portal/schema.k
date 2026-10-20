"""
Typed configuration for the dev portal's manifests.

A module of its own rather than a switch on zaehlwerk's, because it is
released as an artefact of its own: a cluster that runs zaehlwerk does not get
a portal by taking zaehlwerk's next base, and switching it on is pointing an
OCIRepository at zaehlwerk-portal-kustomize. Nothing in it reaches into
zaehlwerk's resources; it only names zaehlwerk's Service.

Every part is a pair. <part>URL is where the portal calls it — a Service, in
the cluster — and <part>PublicURL is what the browser is given to frame the
part's page, the route. "off" is a part the cluster does not run: no status
chip, no frame, no buttons. The variables and what they mean are the binary's,
in the README's dev portal section.
"""

schema Portal:
    """Configuration for the zaehlwerk dev portal's Kubernetes manifests."""
    # ── Identity ────────────────────────────────────────────────────────────
    name: str = "zaehlwerk-portal"
    namespace: str = "zaehlwerk"

    # ── Image ───────────────────────────────────────────────────────────────

    # A placeholder for local renders. CI overrides it with the tag the
    # artefact itself carries, as for zaehlwerk's base.
    image: str = "ghcr.io/stuttgart-things/zaehlwerk-portal:latest"
    imagePullPolicy: str = "IfNotPresent"

    cpuRequest: str = "10m"
    cpuLimit: str = "200m"
    memoryRequest: str = "16Mi"
    memoryLimit: str = "64Mi"

    # ── Ports ───────────────────────────────────────────────────────────────
    servicePort: int = 80
    containerPort: int = 8086

    # ── Exposure ────────────────────────────────────────────────────────────
    httpRouteEnabled: bool = False
    gatewayName: str = ""
    gatewayNamespace: str = ""
    gatewaySectionName: str = "https"
    clusterDomain: str = ""
    # Defaults to "<name>.<clusterDomain>" when both are set.
    host: str = ""


    # ── The parts ───────────────────────────────────────────────────────────
    #
    # zaehlwerk is the one that cannot be off: the page is about it.
    zaehlwerkURL: str = "http://zaehlwerk"
    zaehlwerkPublicURL: str = ""
    schmetterpauseURL: str = "off"
    schmetterpausePublicURL: str = ""
    pongURL: str = "off"
    pongPublicURL: str = ""
    buttonsURL: str = "off"
    buttonsPublicURL: str = ""
    # The board's control endpoint has no page, so no public address.
    piezoControlURL: str = "off"
    simulatorURL: str = "off"
    simulatorPublicURL: str = ""

    # Trust the cluster's own CA, exactly as zaehlwerk does (kcl/schema.k):
    # a Schmetterpause behind the cluster gateway has a certificate the
    # image's public roots do not know. Optional volume, so on by default.
    caBundleEnabled: bool = True
    caConfigMap: str = "cluster-trust-bundle"
    caKey: str = "trust-bundle.pem"

    # ── Escape hatches ──────────────────────────────────────────────────────
    labels: {str:str} = {}
    extraEnvVars: {str:str} = {}

    check:
        zaehlwerkURL not in ["", "off"], "zaehlwerkURL cannot be off: the portal is zaehlwerk's"
        not httpRouteEnabled or gatewayName != "", "httpRouteEnabled needs a gatewayName"
