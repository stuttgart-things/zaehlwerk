"""
The whole configuration except the two values that must not be readable.

Every key here is one row of the environment table in README.md. A value left
empty in the schema is left out of the ConfigMap entirely rather than set to
"", because main.go decides what is enabled by asking whether the variable is
set — an empty CATCHER_URL and an absent one mean the same thing to the code,
but only the absent one says so in `kubectl get cm`.

OMNI_PITCHER_TOKEN and REDIS_PASSWORD are not here. See externalsecret.k.
"""

import labels

config = labels.config

configMap = {
    apiVersion: "v1"
    kind: "ConfigMap"
    metadata: {
        name: labels.configMapName
        namespace: config.namespace
        labels: labels.commonLabels
    }
    data: {
        HTTP_ADDR: ":{}".format(config.containerPort)
        LOG_LEVEL: config.logLevel
        MAX_RETAINED_MATCHES: "{}".format(config.maxRetainedMatches)
        STREAM_HEARTBEAT: config.streamHeartbeat
        PANEL_STREAM: config.panelStream
        if config.allowedOrigins:
            ALLOWED_ORIGINS: config.allowedOrigins
        if config.omniPitcherURL:
            OMNI_PITCHER_URL: config.omniPitcherURL
        if config.omniPitcherPath:
            OMNI_PITCHER_PATH: config.omniPitcherPath
        if config.redisAddr:
            REDIS_ADDR: config.redisAddr
            REDIS_PORT: "{}".format(config.redisPort)
        if config.catcherURL:
            CATCHER_URL: config.catcherURL
        if config.catcherMatchStreams:
            CATCHER_MATCH_STREAMS: config.catcherMatchStreams
        if config.catcherIdleStreams:
            CATCHER_IDLE_STREAMS: config.catcherIdleStreams
        if config.catcherIdleTimeout:
            CATCHER_IDLE_TIMEOUT: config.catcherIdleTimeout
        **config.extraEnvVars
    }
}
