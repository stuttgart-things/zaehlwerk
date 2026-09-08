"""
The HTTPRoute, when a gateway is named.

**This route belongs on an internal gateway.** zaehlwerk has no
authentication — no token, no session, nothing — so the route is the access
control, and everything reachable through it is reachable by anyone who can
reach the hostname: creating a match, scoring it, taking a point back, ending
it, and with CATCHER_URL set, taking the LED panel.

One rule at "/" rather than a narrower set, and that is a deliberate refusal
rather than laziness. The three groups of endpoints are the scoring page under
/ui, the JSON API under /matches, and hardware ingest under /ingest — and the
page is self-contained, so exposing only /ui would serve it fully. It would
also grant every capability the API has, because the page starts, scores,
undoes and ends matches through its own routes. A path split here would shrink
the surface while taking away nothing, which is the kind of boundary that reads
as protection and is not.

So the split that matters is the network, not the path: the scorekeeper at the
table and the ESP32 hardware are both in the building. If this ever needs to be
reachable from further away, the thing to expose is GET /matches/{id}/stream —
read-only, for the Schmetterpause live tab in docs/adr/0001 — and that needs
allowedOrigins set as well. Authentication would be an ADR of its own and would
change this answer.

No HTTP-to-HTTPS redirect: unlike a login form there is nothing here that a
plaintext request could leak, and the gateway in front of this is the place to
decide that policy once for everything behind it rather than per application.

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
