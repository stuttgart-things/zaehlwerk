"""
The two values that must not be readable from the namespace.

OMNI_PITCHER_TOKEN is the bearer token /pitch answers 401 without, and
REDIS_PASSWORD is the panel stream's. Both are optional in the same way the
panel itself is: a deployment with neither pitcher nor Redis has no secret to
have, which is what secretsMode "none" is for and why it is the default.

secretsMode "existing" renders nothing and expects a Secret of this name to be
in the namespace already — the mode for a cluster with no External Secrets
Operator, and the one docs/deployment.md describes creating by hand.

The keys are optional inside the Secret too, and that is deliberate:
deploy.k reads them with optional: true, so a Secret that carries only the
Redis password does not stop the pod from starting.
"""

import labels

config = labels.config

externalSecret = {
    apiVersion: config.externalSecretApiVersion
    kind: "ExternalSecret"
    metadata: {
        name: labels.secretName
        namespace: config.namespace
        labels: labels.commonLabels
    }
    spec: {
        refreshInterval: config.refreshInterval
        secretStoreRef: {
            kind: config.secretStoreKind
            name: config.secretStoreName
        }
        target: {
            name: labels.secretName
            creationPolicy: "Owner"
        }
        data: [
            {
                secretKey: "OMNI_PITCHER_TOKEN"
                remoteRef: {
                    key: config.vaultPath
                    property: config.vaultKeyPitcherToken
                }
            }
            {
                secretKey: "REDIS_PASSWORD"
                remoteRef: {
                    key: config.vaultPath
                    property: config.vaultKeyRedisPassword
                }
            }
        ]
    }
} if labels.renderExternalSecret else Undefined
