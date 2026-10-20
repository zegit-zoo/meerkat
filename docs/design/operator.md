# The meerkat operator: KnowledgeBase and MeerkatMob

Status: third slice (meerkat-mob#10, phase 2). The CRD types, their
manifests, the validating webhook, `cmd/meerkat-operator` and a
KnowledgeBase reconciler that reports each tree in status exist, and
each release ships the operator as an image that the Helm chart installs
(`operator.enabled`, below). The controller that renders Deployments
from them does not exist yet.

## Resources

Both are namespaced, in the group `meerkat-mob.dev`, version
`v1alpha1`. The manifests are generated into `deploy/operator/crds/`
from `internal/operator/api/v1alpha1` by `make crds`, and CI fails when
they are stale (`make crds-check`). The same target writes the
operator's ClusterRole to `deploy/operator/rbac/` and its
`ValidatingWebhookConfiguration` to `deploy/operator/webhook/`, from
the `+kubebuilder:rbac` and `+kubebuilder:webhook` markers.

A `KnowledgeBase` (short name `kb`) is one node of a knowledge-base
tree. Its keys mean what the same keys mean in a `tree:` manifest
([tree.md](tree.md)):

```yaml
apiVersion: meerkat-mob.dev/v1alpha1
kind: KnowledgeBase
metadata: {name: platform, namespace: kb}
spec:
  parent: root            # empty for the root; an object name
  mount: lazy             # eager (default) | lazy
  placement: shared       # shared (default) | dedicated
  tier: 1
  depthLimit: 2           # lowers the cap below this KB, never raises it
  access: {paths: [root/platform], identities: [alice]}
  resources: {maxResidentBytes: 64Mi}
  refresh: {interval: 5m}
  source:                 # exactly one of s3, gcs, bundle
    s3: {bucket: kb, prefix: platform/, endpoint: "https://s3.example.net", pathStyle: true}
```

A `MeerkatMob` (short name `mob`) serves a tree from its `root`:
`placement` (`perTier`, `perKB` or `single`, the default),
`scaleToZero` (`idle` defaults to 30m), `telemetry` (one OTLP endpoint
and its `protocol`, `grpc` by default or `http/protobuf`, as the
server's `observability.otlp`; a gRPC endpoint may be a bare
`host:port`), `auth` (OIDC
issuer and audience, with the authz policy in a referenced ConfigMap)
and `librarian.schedule` (a five-field cron expression). No resource
carries a credential. Credentials come from the pods' environment, as
for the server.

The defaults above (`mount`, `placement`, mob `placement`, `idle`,
`telemetry.protocol`) are schema defaults, so the API server fills them
in and `kubectl get -o yaml` shows them. Both statuses carry
`observedGeneration`; a KnowledgeBase's served content version is
`status.contentGeneration`.

## What admission refuses

The CRD schema refuses, on any API server with CEL:

- a source with other than exactly one of `s3`, `gcs` and `bundle`;
- an `http://` S3 or OTLP endpoint without `insecure: true`, the rule
  every other S3 and OTLP setting follows ([object-stores.md](object-stores.md));
- a bundle URL that is not `https://`, or a `sha256` that is not 64 hex
  digits;
- a bucket name containing `/`, a `depthLimit` or `tier` outside 0–5,
  and a `mount`, `placement`, mob `placement` or `telemetry.protocol`
  outside its enum;
- a `parent` or mob `root` that is not a Kubernetes object name
  (a DNS-1123 subdomain), and a telemetry endpoint that is neither a
  URL nor `host:port`.

The webhook (`internal/operator/webhook`) adds the rules that need the
other objects in the namespace. These are the manifest loader's rules,
with the cap taken from `contentsource.MaxTreeDepth`, so the webhook
admits the same trees the server loads:

- **Depth.** No KnowledgeBase may sit deeper than 5 (root = 0). A
  `depthLimit` on an ancestor lowers that for everything below it.
  Creating or re-parenting a KnowledgeBase checks the whole subtree
  below it, so a move that pushes a grandchild to depth 6 is refused,
  and the error names the grandchild and its path.
- **Cycles.** A KnowledgeBase that is its own parent, or whose parent
  chain comes back to it, is refused, and the error shows the loop.
- **Parents created later are allowed.** A parent that does not exist
  yet is admitted with a warning, because `kubectl apply -f dir/` has
  no order. Each missing parent still counts as one level, and the
  rules are checked again when the parent is created: creating it
  under its own descendant is refused as a cycle.
- **Fail closed.** If the namespace cannot be listed, a create or
  update is refused. A delete is always admitted, with a warning
  naming any children it leaves without a parent.
- **Only edits that change the tree are checked against it.** The
  tree rules run on a create and on an update that changes
  `spec.parent` or `spec.depthLimit`. Any other update (a description,
  a label, a finalizer, an owner reference) is admitted even inside a
  tree that already breaks the rules, for example one admitted while
  the webhook was down; its source is still checked. A re-parent or a
  new `depthLimit` is checked against the whole tree, so it is refused
  if the edited KnowledgeBase or anything below it is still too deep,
  but violations elsewhere in the namespace are not reported.
- **Admission is a check at write time.** Two concurrent creates that
  together close a loop or exceed the cap can both pass. The server's
  loader refuses such a tree at startup, and the reconciler checks it
  again and reports it (below).

A MeerkatMob is refused for a `scaleToZero.idle` under 1m or a `root`
that is not an object name, and warned
about when its root does not exist yet or has a parent of its own.

## The KnowledgeBase reconciler

`cmd/meerkat-operator` runs a controller-runtime manager with the
webhook (`--webhooks`, on by default; it needs a serving certificate in
`--webhook-cert-dir`) and the KnowledgeBase reconciler
(`internal/operator/controller`). The reconciler renders nothing. For
each KnowledgeBase it runs the webhook's tree rules again against the
namespace as it is now, and writes status:

- `observedGeneration`: the generation it checked.
- `depth`: the KnowledgeBase's depth (root = 0), counted as the webhook
  counts it, so one under a missing parent is at depth 1 at least. For
  a tree that loops it is a lower bound.
- The `TreeValid` condition: `True` (`TreeValid`) when the chain ends
  at a root and the subtree is within the limits, `Unknown`
  (`ParentMissing`) while a parent in the chain does not exist yet, and
  `False` (`TreeRefused`) for a loop or a depth over the cap or an
  ancestor's `depthLimit`, with the webhook's message.

A KnowledgeBase's condition depends on its ancestors and its subtree,
so a create, delete or spec change of any KnowledgeBase re-queues every
KnowledgeBase in its namespace. Status writes do not change the
generation, so they do not re-trigger it, and a status that has not
changed is not written. The MeerkatMob reconciler is to render only
from KnowledgeBases whose `TreeValid` is `True`.

The KnowledgeBase webhook fails closed on a create or update. Its
delete hook is separate and set to `failurePolicy: Ignore`, because a
delete is only warned about, and a down operator must not block it.

## Packaging

`release.yml` builds `ghcr.io/zegit-zoo/meerkat-operator` from the
Dockerfile's `operator` target (distroless, non-root, its own
`THIRD-PARTY-LICENSES.txt`) and signs it like the meerkat image.
`charts/meerkat` installs it behind `operator.enabled` (off by default):
the CRDs, a ServiceAccount, the ClusterRole and its binding, a
Deployment with leader election, the webhook Service and the
`ValidatingWebhookConfiguration`. `make crds` copies the generated CRDs,
ClusterRole and webhook configuration into `charts/meerkat/files/operator/`
and `make crds-check` covers the copies, so the chart renders the same
rules and paths the code declares. The chart sets only the names, the
Service reference and the CA.

The CRDs are templates, not a `crds/` directory: they install only with
the operator, `helm upgrade` updates them, and they carry
`helm.sh/resource-policy: keep` so an uninstall does not delete every
KnowledgeBase.

The webhook's serving certificate comes from cert-manager by default (a
`Certificate` from a given issuer or a chart-made self-signed `Issuer`,
with the CA injected through `cert-manager.io/inject-ca-from`), or,
without cert-manager, from a CA and certificate Helm generates once and
reuses on upgrade. The chart README has the rotation steps.

## Not built yet

The MeerkatMob reconciler (one Deployment per placement unit, rendered
`content-source.yaml`, lazy children rewritten to sibling Services,
NetworkPolicy for dedicated KBs), MeerkatMob status, scale-to-zero, and
the librarian CronJob. Also still to come: a chart switch to install the
operator without the meerkat server, a NetworkPolicy for the operator
pod, and a registry allow-list or a pin for a MeerkatMob's
`spec.image`, which until then lets anyone who can create a MeerkatMob
choose the image its pods run. meerkat-mob#10 tracks them.
