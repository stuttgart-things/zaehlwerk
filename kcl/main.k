"""
Entry point: every resource, with the ones that are switched off filtered out.

The resources come out as a list under one key rather than as a YAML stream,
and that is a contract rather than a style choice.

github.com/stuttgart-things/dagger/kcl — the module that turns this into a
kustomize base and pushes it as an OCI artifact — post-processes the render
with a fixed sed chain that assumes exactly this shape: drop the first line,
de-indent by two, turn "- " at column zero into a document separator. Handed a
yaml_stream it deletes the first resource's apiVersion and lifts every metadata
field to the top level. The result still parses as YAML, so nothing errors; it
just applies something else.

Use `task kcl:render` rather than `kcl run` directly: it applies the same
normalisation, so what is rendered locally is what the OCI artifact carries.

No Namespace is emitted. Several Applications can share a workload namespace,
and when more than one ships a Namespace resource ArgoCD flags it as a
SharedResource — then one Application's prune cycle deletes the namespace out
from under the others. The consuming Application sets CreateNamespace=true
instead. Learned in stuttgart-things/homerun2-omni-pitcher#132.

No Redis and no led-catcher either. Both are things this service talks to
rather than things it owns: they outlive any revision of it, they are shared
with everything else on the homerun bus, and a base that carried them would be
a base whose removal can prune somebody else's panel.
"""

import serviceaccount
import configmap
import externalsecret
import deploy
import service
import httproute

_resources = [
    serviceaccount.serviceAccount
    configmap.configMap
    externalsecret.externalSecret
    deploy.deployment
    service.service
    httproute.httproute
]

manifests = [r for r in _resources if r]
