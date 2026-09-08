"""
The one configuration instance, the command-line overrides that shape it, and
the labels every resource carries.

Every field of the schema is overridable, which is what "individually
renderable" means here: one module, one profile file per environment, no
per-environment copy of the manifests.

The enumeration below is unavoidable rather than accidental — KCL resolves
option("config.x") per key, so a spread of option("config") picks up nothing.
The string coercions exist because -D passes every value as a string while a
settings file passes real types; both have to work.
"""

import schema

_name = option("config.name")
_namespace = option("config.namespace")
_image = option("config.image")
_imagePullPolicy = option("config.imagePullPolicy")
_replicas = option("config.replicas")
_cpuRequest = option("config.cpuRequest")
_cpuLimit = option("config.cpuLimit")
_memoryRequest = option("config.memoryRequest")
_memoryLimit = option("config.memoryLimit")
_servicePort = option("config.servicePort")
_containerPort = option("config.containerPort")
_httpRouteEnabled = option("config.httpRouteEnabled")
_gatewayName = option("config.gatewayName")
_gatewayNamespace = option("config.gatewayNamespace")
_gatewaySectionName = option("config.gatewaySectionName")
_clusterDomain = option("config.clusterDomain")
_host = option("config.host")
_logLevel = option("config.logLevel")
_maxRetainedMatches = option("config.maxRetainedMatches")
_streamHeartbeat = option("config.streamHeartbeat")
_allowedOrigins = option("config.allowedOrigins")
_panelStream = option("config.panelStream")
_omniPitcherURL = option("config.omniPitcherURL")
_omniPitcherPath = option("config.omniPitcherPath")
_redisAddr = option("config.redisAddr")
_redisPort = option("config.redisPort")
_catcherURL = option("config.catcherURL")
_catcherMatchStreams = option("config.catcherMatchStreams")
_catcherIdleStreams = option("config.catcherIdleStreams")
_catcherIdleTimeout = option("config.catcherIdleTimeout")
_secretsMode = option("config.secretsMode")
_secretStoreKind = option("config.secretStoreKind")
_secretStoreName = option("config.secretStoreName")
_externalSecretApiVersion = option("config.externalSecretApiVersion")
_refreshInterval = option("config.refreshInterval")
_vaultPath = option("config.vaultPath")
_vaultKeyPitcherToken = option("config.vaultKeyPitcherToken")
_vaultKeyRedisPassword = option("config.vaultKeyRedisPassword")

# A boolean that arrives as the string "false" is still truthy, so every
# boolean is compared rather than assigned. Getting this wrong turns an
# explicit "off" into "on".
_bool = lambda v: any -> bool {
    v in [True, "True", "true", "yes", "1"]
}

config: schema.Zaehlwerk = schema.Zaehlwerk {
    if _name:
        name = _name
    if _namespace:
        namespace = _namespace
    if _image:
        image = _image
    if _imagePullPolicy:
        imagePullPolicy = _imagePullPolicy
    if _replicas:
        replicas = int(_replicas)
    if _cpuRequest:
        cpuRequest = _cpuRequest
    if _cpuLimit:
        cpuLimit = _cpuLimit
    if _memoryRequest:
        memoryRequest = _memoryRequest
    if _memoryLimit:
        memoryLimit = _memoryLimit
    if _servicePort:
        servicePort = int(_servicePort)
    if _containerPort:
        containerPort = int(_containerPort)
    if _httpRouteEnabled != Undefined and _httpRouteEnabled != None:
        httpRouteEnabled = _bool(_httpRouteEnabled)
    if _gatewayName:
        gatewayName = _gatewayName
    if _gatewayNamespace:
        gatewayNamespace = _gatewayNamespace
    if _gatewaySectionName:
        gatewaySectionName = _gatewaySectionName
    if _clusterDomain:
        clusterDomain = _clusterDomain
    if _host:
        host = _host
    if _logLevel:
        logLevel = _logLevel
    if _maxRetainedMatches:
        maxRetainedMatches = int(_maxRetainedMatches)
    if _streamHeartbeat:
        streamHeartbeat = _streamHeartbeat
    if _allowedOrigins:
        allowedOrigins = _allowedOrigins
    if _panelStream:
        panelStream = _panelStream
    if _omniPitcherURL:
        omniPitcherURL = _omniPitcherURL
    if _omniPitcherPath:
        omniPitcherPath = _omniPitcherPath
    if _redisAddr:
        redisAddr = _redisAddr
    if _redisPort:
        redisPort = int(_redisPort)
    if _catcherURL:
        catcherURL = _catcherURL
    if _catcherMatchStreams:
        catcherMatchStreams = _catcherMatchStreams
    if _catcherIdleStreams:
        catcherIdleStreams = _catcherIdleStreams
    if _catcherIdleTimeout:
        catcherIdleTimeout = _catcherIdleTimeout
    if _secretsMode:
        secretsMode = _secretsMode
    if _secretStoreKind:
        secretStoreKind = _secretStoreKind
    if _secretStoreName:
        secretStoreName = _secretStoreName
    if _externalSecretApiVersion:
        externalSecretApiVersion = _externalSecretApiVersion
    if _refreshInterval:
        refreshInterval = _refreshInterval
    if _vaultPath:
        vaultPath = _vaultPath
    if _vaultKeyPitcherToken:
        vaultKeyPitcherToken = _vaultKeyPitcherToken
    if _vaultKeyRedisPassword:
        vaultKeyRedisPassword = _vaultKeyRedisPassword
}

# ── Derived names ───────────────────────────────────────────────────────────
configMapName = "{}-config".format(config.name)
secretName = "{}-panel".format(config.name)

# Both panel paths are optional, and so is the secret they need.
panelEnabled = config.omniPitcherURL != "" or config.redisAddr != ""
renderSecret = config.secretsMode != "none"
renderExternalSecret = config.secretsMode == "external"

host = config.host if config.host else ("{}.{}".format(config.name, config.clusterDomain) if config.clusterDomain else "")

# ── Labels ──────────────────────────────────────────────────────────────────
#
# Built through a lambda rather than overridden per resource: KCL's dict union
# rejects a conflicting value instead of letting the later one win, so
# "commonLabels with one key changed" is not an expression that exists.
labelsFor = lambda component: str -> {str:str} {
    {
        "app.kubernetes.io/name": config.name
        "app.kubernetes.io/instance": config.name
        "app.kubernetes.io/component": component
        "app.kubernetes.io/part-of": config.name
        **config.labels
    }
}

commonLabels = labelsFor("scorekeeper")

selectorLabels = {
    "app.kubernetes.io/name": config.name
    "app.kubernetes.io/instance": config.name
}
