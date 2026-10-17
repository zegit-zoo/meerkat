# The meerkat operator: KnowledgeBase and MeerkatMob

Status: first slice (meerkat-mob#10, phase 2). The CRD types, their
manifests and the validating webhook exist. The controller that renders
Deployments from them does not yet, and nothing ships the operator.

## Resources

Both are namespaced, in the group `meerkat-mob.dev`, version
`v1alpha1`. The manifests are generated into `deploy/operator/crds/`
from `internal/operator/api/v1alpha1` by `make crds`, and CI fails when
they are stale (`make crds-check`).

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
  loader refuses such a tree at startup; the reconciler is to re-check
  it and report a condition (not built yet).

A MeerkatMob is refused for a `scaleToZero.idle` under 1m or a `root`
that is not an object name, and warned
about when its root does not exist yet or has a parent of its own.

## Not built yet

`cmd/meerkat-operator` (the manager, the webhook server and its
certificates, `ValidatingWebhookConfiguration`, RBAC), the MeerkatMob
reconciler (one Deployment per placement unit, rendered
`content-source.yaml`, lazy children rewritten to sibling Services,
NetworkPolicy for dedicated KBs), status, scale-to-zero, and the
librarian CronJob. Also still to come: `+kubebuilder:webhook` markers
so `make crds` emits the `ValidatingWebhookConfiguration`, the CRDs
packaged with (or referenced from) `charts/meerkat/crds/`, and a
registry allow-list or a pin for a MeerkatMob's `spec.image`, which
until then lets anyone who can create a MeerkatMob choose the image
its pods run. meerkat-mob#10 tracks them.
