"""
The HTTPRoute, when a gateway is named.

One route and no HTTP-to-HTTPS redirect: unlike a login form there is nothing
here that a plaintext request could leak, and the gateway in front of this is
the place to decide that policy once for everything behind it rather than per
application.

The hostname comes from labels.host, which is config.host if set and
"<name>.<clusterDomain>" otherwise — one decision, two ways to express it.
"""

import labels

config = labels.config

httproute = {
    apiVersion: "gateway.networking.k8s.io/v1"
    kind: "HTTPRoute"
    metadata: {
        name: config.name
        namespace: config.namespace
        labels: labels.commonLabels
    }
    spec: {
        parentRefs: [
            {
                name: config.gatewayName
                if config.gatewayNamespace:
                    namespace: config.gatewayNamespace
                sectionName: config.gatewaySectionName
            }
        ]
        if labels.host:
            hostnames: [labels.host]
        rules: [
            {
                matches: [{
                    path: {type: "PathPrefix", value: "/"}
                }]
                backendRefs: [
                    {
                        name: config.name
                        port: config.servicePort
                    }
                ]
            }
        ]
    }
} if config.httpRouteEnabled else Undefined
