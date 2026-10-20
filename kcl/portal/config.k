"""
The one configuration instance, from the command-line overrides, and the
names and labels derived from it. The same shape as kcl/labels.k next door,
for the same reasons: option() resolves per key, -D passes strings while a
profile passes real types, and a boolean is compared rather than assigned.
"""

import schema

_bool = lambda v: any -> bool {
    v in [True, "True", "true", "yes", "1"]
}

_name = option("config.name")
_namespace = option("config.namespace")
_image = option("config.image")
_imagePullPolicy = option("config.imagePullPolicy")
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
_zaehlwerkURL = option("config.zaehlwerkURL")
_zaehlwerkPublicURL = option("config.zaehlwerkPublicURL")
_schmetterpauseURL = option("config.schmetterpauseURL")
_schmetterpausePublicURL = option("config.schmetterpausePublicURL")
_pongURL = option("config.pongURL")
_pongPublicURL = option("config.pongPublicURL")
_buttonsURL = option("config.buttonsURL")
_buttonsPublicURL = option("config.buttonsPublicURL")
_piezoControlURL = option("config.piezoControlURL")
_simulatorURL = option("config.simulatorURL")
_simulatorPublicURL = option("config.simulatorPublicURL")
_caBundleEnabled = option("config.caBundleEnabled")
_caConfigMap = option("config.caConfigMap")
_caKey = option("config.caKey")

config: schema.Portal = schema.Portal {
    if _name:
        name = _name
    if _namespace:
        namespace = _namespace
    if _image:
        image = _image
    if _imagePullPolicy:
        imagePullPolicy = _imagePullPolicy
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
    if _zaehlwerkURL:
        zaehlwerkURL = _zaehlwerkURL
    if _zaehlwerkPublicURL:
        zaehlwerkPublicURL = _zaehlwerkPublicURL
    if _schmetterpauseURL:
        schmetterpauseURL = _schmetterpauseURL
    if _schmetterpausePublicURL:
        schmetterpausePublicURL = _schmetterpausePublicURL
    if _pongURL:
        pongURL = _pongURL
    if _pongPublicURL:
        pongPublicURL = _pongPublicURL
    if _buttonsURL:
        buttonsURL = _buttonsURL
    if _buttonsPublicURL:
        buttonsPublicURL = _buttonsPublicURL
    if _piezoControlURL:
        piezoControlURL = _piezoControlURL
    if _simulatorURL:
        simulatorURL = _simulatorURL
    if _simulatorPublicURL:
        simulatorPublicURL = _simulatorPublicURL
    if _caBundleEnabled != Undefined and _caBundleEnabled != None:
        caBundleEnabled = _bool(_caBundleEnabled)
    if _caConfigMap:
        caConfigMap = _caConfigMap
    if _caKey:
        caKey = _caKey
}

configMapName = "{}-config".format(config.name)

host = config.host if config.host else ("{}.{}".format(config.name, config.clusterDomain) if config.clusterDomain else "")

labels = {
    "app.kubernetes.io/name": config.name
    "app.kubernetes.io/instance": config.name
    "app.kubernetes.io/component": "dev-portal"
    "app.kubernetes.io/part-of": "zaehlwerk"
    **config.labels
}

selectorLabels = {
    "app.kubernetes.io/name": config.name
    "app.kubernetes.io/instance": config.name
}
