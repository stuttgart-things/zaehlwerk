"""
The Deployment.

Three things here are decisions rather than boilerplate.

**One replica, enforced in schema.k.** The running match lives in this
process's memory and this service is its only writer (docs/adr/0001). A second
pod would keep a second score.

**Recreate, not RollingUpdate.** The default strategy starts the new pod before
the old one goes away, and for twenty seconds two processes would each believe
they own the match — the very thing the replica count exists to prevent. A
rolling update also cannot be made safe by readiness here, because the old pod
stays ready right up to its SIGTERM. Recreate accepts a few seconds of downtime
between the two, which for a service whose state does not survive a restart
anyway is the smaller loss: a match in progress is lost either way, and losing
it once is better than having it scored into two pods.

**A liveness probe and no readiness probe.** /healthz touches neither Redis nor
the omni-pitcher, so it says the process is up and serving and nothing else.
Ready and live do not differ for a service that loads nothing at startup, and a
readiness probe that checked the panel would take the pod down whenever the
panel is switched off — a supported way to run this. The README says the same
under "There is no /readyz".
"""

import labels

config = labels.config

_secretEnv = [
    {
        name: "OMNI_PITCHER_TOKEN"
        valueFrom: {
            secretKeyRef: {name: labels.secretName, key: "OMNI_PITCHER_TOKEN", optional: True}
        }
    }
    {
        name: "REDIS_PASSWORD"
        valueFrom: {
            secretKeyRef: {name: labels.secretName, key: "REDIS_PASSWORD", optional: True}
        }
    }
] if labels.renderSecret else []

deployment = {
    apiVersion: "apps/v1"
    kind: "Deployment"
    metadata: {
        name: config.name
        namespace: config.namespace
        labels: labels.commonLabels
    }
    spec: {
        replicas: config.replicas
        strategy: {type: "Recreate"}
        selector: {matchLabels: labels.selectorLabels}
        template: {
            metadata: {labels: labels.commonLabels}
            spec: {
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
                        name: config.name
                        image: config.image
                        imagePullPolicy: config.imagePullPolicy
                        ports: [{name: "http", containerPort: config.containerPort, protocol: "TCP"}]
                        envFrom: [{
                            configMapRef: {name: labels.configMapName}
                        }]
                        if _secretEnv:
                            env: _secretEnv
                        livenessProbe: {
                            httpGet: {path: "/healthz", port: "http"}
                            initialDelaySeconds: 3
                            periodSeconds: 10
                            timeoutSeconds: 2
                            failureThreshold: 3
                        }
                        resources: {
                            requests: {cpu: config.cpuRequest, memory: config.memoryRequest}
                            limits: {cpu: config.cpuLimit, memory: config.memoryLimit}
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
}
