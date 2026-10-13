"""
zaehlwerk-pong (docs/adr/0006), when config.pongEnabled is set: a ConfigMap, a
Deployment, a Service, and with config.pongHttpRouteEnabled an HTTPRoute.
Nothing is rendered with it off, and nothing of zaehlwerk's own changes with
it on.

**It plays against zaehlwerk's Service, not its route.** ZAEHLWERK_URL is the
cluster-internal name, so the game needs no gateway, no certificate and no
ALLOWED_ORIGINS entry: the browser only ever talks to the game, and the game
talks to zaehlwerk server-side (ADR-0006).

**One replica and Recreate, for the same reason as zaehlwerk.** A game is in
the memory of the pod playing it, one game at a time holds the table, and a
browser game's token is known to that pod only. Two pods would be two players
each believing the table is theirs, and with attract mode on, two demos
fighting over one panel. The replica count is therefore not a field.

**The route, if any, is access control too.** The game has no authentication.
Whoever reaches it can start a game, which takes the table from a demo — never
from a match, which it refuses to join — so it belongs on the same internal
gateway as zaehlwerk.

**No terminationGracePeriodSeconds**: the default thirty seconds is already
more than the ten the binary takes to end the running game's match on
zaehlwerk at SIGTERM, which is what keeps a match from staying current with
nobody scoring it.

**Only the variables that are set are rendered**, as for zaehlwerk: unset is
how the code switches attract mode, faults and a fixed seed off, and an empty
value would be a value.
"""

import labels

config = labels.config

pongConfigMap = {
    apiVersion: "v1"
    kind: "ConfigMap"
    metadata: {
        name: labels.pongConfigMapName
        namespace: config.namespace
        labels: labels.pongLabels
    }
    data: {
        HTTP_ADDR: ":{}".format(config.pongContainerPort)
        LOG_LEVEL: config.logLevel
        ZAEHLWERK_URL: labels.zaehlwerkURL
        PONG_BEST_OF: "{}".format(config.pongBestOf)
        PONG_BOT_SKILL: config.pongBotSkill
        if config.pongFaults:
            PONG_FAULTS: config.pongFaults
        if config.pongSeed:
            PONG_SEED: config.pongSeed
        if config.pongAttractAfter:
            PONG_ATTRACT_AFTER: config.pongAttractAfter
        if config.pongAttractPace:
            PONG_ATTRACT_PACE: config.pongAttractPace
        if config.pongBrowserIdle:
            PONG_BROWSER_IDLE: config.pongBrowserIdle
    }
} if config.pongEnabled else Undefined

pongDeployment = {
    apiVersion: "apps/v1"
    kind: "Deployment"
    metadata: {
        name: labels.pongName
        namespace: config.namespace
        labels: labels.pongLabels
    }
    spec: {
        replicas: 1
        strategy: {type: "Recreate"}
        selector: {matchLabels: labels.pongSelectorLabels}
        template: {
            metadata: {labels: labels.pongLabels}
            spec: {
                # zaehlwerk's account: it has no token mounted and no role,
                # and the game needs nothing from the API server either.
                serviceAccountName: config.name
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
                        name: labels.pongName
                        image: labels.pongImage
                        imagePullPolicy: config.imagePullPolicy
                        ports: [{name: "http", containerPort: config.pongContainerPort, protocol: "TCP"}]
                        envFrom: [{
                            configMapRef: {name: labels.pongConfigMapName}
                        }]
                        # /healthz says the process is up and does not ask
                        # zaehlwerk: a zaehlwerk that is down is a game that
                        # cannot start, not a pod to restart.
                        livenessProbe: {
                            httpGet: {path: "/healthz", port: "http"}
                            initialDelaySeconds: 3
                            periodSeconds: 10
                            timeoutSeconds: 2
                            failureThreshold: 3
                        }
                        resources: {
                            requests: {cpu: config.pongCpuRequest, memory: config.pongMemoryRequest}
                            limits: {cpu: config.pongCpuLimit, memory: config.pongMemoryLimit}
                        }
                        securityContext: {
                            allowPrivilegeEscalation: False
                            readOnlyRootFilesystem: True
                            capabilities: {drop: ["ALL"]}
                        }
                    }
                ]
            }
        }
    }
} if config.pongEnabled else Undefined

pongService = {
    apiVersion: "v1"
    kind: "Service"
    metadata: {
        name: labels.pongName
        namespace: config.namespace
        labels: labels.pongLabels
    }
    spec: {
        type: "ClusterIP"
        ports: [
            {
                name: "http"
                port: config.servicePort
                targetPort: "http"
                protocol: "TCP"
            }
        ]
        selector: labels.pongSelectorLabels
    }
} if config.pongEnabled else Undefined

pongHTTPRoute = {
    apiVersion: "gateway.networking.k8s.io/v1"
    kind: "HTTPRoute"
    metadata: {
        name: labels.pongName
        namespace: config.namespace
        labels: labels.pongLabels
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
        if labels.pongHost:
            hostnames: [labels.pongHost]
        rules: [
            {
                matches: [{
                    path: {type: "PathPrefix", value: "/"}
                }]
                backendRefs: [
                    {
                        name: labels.pongName
                        port: config.servicePort
                    }
                ]
            }
        ]
    }
} if config.pongEnabled and config.pongHttpRouteEnabled else Undefined
