"""
Entry point: the dev portal (tools/chain-mock/portal) as a ConfigMap, a
Deployment, a Service and, with config.httpRouteEnabled, an HTTPRoute.

The output is a list under `manifests:` for the reason given in kcl/main.k:
stuttgart-things/dagger/kcl normalises exactly that shape into the kustomize
base, and anything else comes out of it as something different that still
parses.

**The route is the access control, and it belongs on an internal gateway.**
The portal has no login. Through it anybody who reaches the hostname can end
the running match, start one, and, where the parts are there, pause a piezo
board or start a pong game — no more than zaehlwerk's and pong's own routes
already allow, which is why it sits on the same gateway as theirs.

**One replica and Recreate.** The portal keeps nothing, so a second pod would
do no harm — and no good either: a page that polls every two seconds is not
load. One is simply the honest number for a tool.

**No Namespace**, for the reason given in kcl/main.k: the consuming
Kustomization or Application creates it.
"""

import config as c

_cfg = c.config

_caDir = "/etc/ssl/cluster-trust"

_configMap = {
    apiVersion: "v1"
    kind: "ConfigMap"
    metadata: {
        name: c.configMapName
        namespace: _cfg.namespace
        labels: c.labels
    }
    data: {
        PORTAL_ADDR: ":{}".format(_cfg.containerPort)
        # Every part is rendered, "off" included, so an environment can patch
        # a value that is there rather than add one that is not.
        ZAEHLWERK_URL: _cfg.zaehlwerkURL
        SCHMETTERPAUSE_URL: _cfg.schmetterpauseURL
        PONG_URL: _cfg.pongURL
        BUTTONS_URL: _cfg.buttonsURL
        PIEZO_CONTROL_URL: _cfg.piezoControlURL
        SIMULATOR_URL: _cfg.simulatorURL
        # Empty is the same address as the one called, which is never right
        # in a cluster but is what the binary does with an unset one; so only
        # the ones given are rendered.
        if _cfg.zaehlwerkPublicURL:
            ZAEHLWERK_PUBLIC_URL: _cfg.zaehlwerkPublicURL
        if _cfg.schmetterpausePublicURL:
            SCHMETTERPAUSE_PUBLIC_URL: _cfg.schmetterpausePublicURL
        if _cfg.pongPublicURL:
            PONG_PUBLIC_URL: _cfg.pongPublicURL
        if _cfg.buttonsPublicURL:
            BUTTONS_PUBLIC_URL: _cfg.buttonsPublicURL
        if _cfg.simulatorPublicURL:
            SIMULATOR_PUBLIC_URL: _cfg.simulatorPublicURL
        **_cfg.extraEnvVars
    }
}

_probe = {
    httpGet: {path: "/healthz", port: "http"}
    periodSeconds: 10
    timeoutSeconds: 2
}

_deployment = {
    apiVersion: "apps/v1"
    kind: "Deployment"
    metadata: {
        name: _cfg.name
        namespace: _cfg.namespace
        labels: c.labels
    }
    spec: {
        replicas: 1
        strategy: {type: "Recreate"}
        selector: {matchLabels: c.selectorLabels}
        template: {
            metadata: {labels: c.labels}
            spec: {
                # It calls other Services and nothing in the API server.
                automountServiceAccountToken: False
                securityContext: {
                    runAsNonRoot: True
                    runAsUser: 65532
                    runAsGroup: 65532
                    fsGroup: 65532
                    seccompProfile: {type: "RuntimeDefault"}
                }
                containers: [
                    {
                        name: "portal"
                        image: _cfg.image
                        imagePullPolicy: _cfg.imagePullPolicy
                        ports: [{name: "http", containerPort: _cfg.containerPort, protocol: "TCP"}]
                        envFrom: [{configMapRef: {name: c.configMapName}}]
                        if _cfg.caBundleEnabled:
                            # SSL_CERT_DIR adds to the image's roots, where
                            # SSL_CERT_FILE would replace them; see
                            # kcl/README.md, "Trusting the cluster's CA".
                            env: [{name: "SSL_CERT_DIR", value: _caDir}]
                            volumeMounts: [{name: "cluster-trust", mountPath: _caDir, readOnly: True}]
                        # /healthz asks no part: a zaehlwerk that is down is
                        # what the page is there to show, not a reason to
                        # restart it.
                        readinessProbe: _probe
                        livenessProbe: _probe | {initialDelaySeconds: 3, failureThreshold: 3}
                        resources: {
                            requests: {cpu: _cfg.cpuRequest, memory: _cfg.memoryRequest}
                            limits: {cpu: _cfg.cpuLimit, memory: _cfg.memoryLimit}
                        }
                        securityContext: {
                            allowPrivilegeEscalation: False
                            readOnlyRootFilesystem: True
                            capabilities: {drop: ["ALL"]}
                        }
                    }
                ]
                if _cfg.caBundleEnabled:
                    volumes: [{
                        name: "cluster-trust"
                        configMap: {
                            name: _cfg.caConfigMap
                            optional: True
                            items: [{key: _cfg.caKey, path: "ca.pem"}]
                        }
                    }]
            }
        }
    }
}

_service = {
    apiVersion: "v1"
    kind: "Service"
    metadata: {
        name: _cfg.name
        namespace: _cfg.namespace
        labels: c.labels
    }
    spec: {
        type: "ClusterIP"
        ports: [{name: "http", port: _cfg.servicePort, targetPort: "http", protocol: "TCP"}]
        selector: c.selectorLabels
    }
}

_httpRoute = {
    apiVersion: "gateway.networking.k8s.io/v1"
    kind: "HTTPRoute"
    metadata: {
        name: _cfg.name
        namespace: _cfg.namespace
        labels: c.labels
    }
    spec: {
        parentRefs: [
            {
                name: _cfg.gatewayName
                if _cfg.gatewayNamespace:
                    namespace: _cfg.gatewayNamespace
                sectionName: _cfg.gatewaySectionName
            }
        ]
        if c.host:
            hostnames: [c.host]
        rules: [
            {
                matches: [{path: {type: "PathPrefix", value: "/"}}]
                backendRefs: [{name: _cfg.name, port: _cfg.servicePort}]
            }
        ]
    }
} if _cfg.httpRouteEnabled else Undefined

manifests = [r for r in [_configMap, _deployment, _service, _httpRoute] if r]
