# The meerkat operator: KnowledgeBase and MeerkatMob

Status: fifth slice (meerkat-mob#10, phase 2). The CRD types, their
manifests, the validating webhook, `cmd/meerkat-operator`, a
KnowledgeBase reconciler that reports each tree in status, and a
MeerkatMob reconciler that serves a tree as meerkat Deployments, with
their credentials and sizing, exist,
and each release ships the operator as an image that the Helm chart
installs (`operator.enabled`, below). Cross-process hops, scale-to-zero
and the librarian CronJob do not exist yet (see the end of this page).

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
`librarian.schedule` (a five-field cron expression), `credentials` (an
S3 Secret by name and the annotations of the mob's ServiceAccount) and
`resources` (every instance's container resources). No resource carries
a credential: the pods read the referenced Secret, or a cloud workload
identity, as the server reads its environment.

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
changed is not written.

The KnowledgeBase webhook fails closed on a create or update. Its
delete hook is separate and set to `failurePolicy: Ignore`, because a
delete is only warned about, and a down operator must not block it.

## The MeerkatMob reconciler

For each MeerkatMob, the reconciler takes `spec.root` and every
KnowledgeBase below it in the mob's namespace, depth first with
siblings by name, and splits them into placement units:

| `spec.placement` | units |
| --- | --- |
| `single` (default) | one, `<mob>-all` |
| `perTier` | one per `spec.tier`, `<mob>-tier-<n>` |
| `perKB` | one per KnowledgeBase, `<mob>-kb-<name>` |

A KnowledgeBase with `placement: dedicated` gets a `<mob>-kb-<name>`
unit of its own under every placement. A name that is not a DNS-1035
label (a Service name) is shortened and given a hash suffix.

Each unit is a ConfigMap holding a `content-source.yaml`, a Deployment
of one replica and a ClusterIP Service on port 4005, all named after
the unit, labelled `meerkat-mob.dev/mob` and `meerkat-mob.dev/unit`, and
controlled by the mob through an owner reference, so deleting the mob
deletes them. They are written with server-side apply (field manager
`meerkat-operator`, forcing ownership), so a hand edit of a field the
operator sets is put back on the next reconcile. A unit the mob no
longer has is deleted. Only objects the mob controls are ever pruned.

The `content-source.yaml` is a `collections:` list, one entry per
KnowledgeBase in tree order, named after it: an `s3` source becomes
`type: s3` with the same bucket, prefix, endpoint, region, path style
and `insecure`; `gcs` becomes `type: gcs`; a `bundle` becomes `type:
url` with its `sha256`; `refresh.interval` and `description` carry
over. `spec.telemetry` becomes `observability:` (traces on, exported to
that one collector). `spec.auth` becomes `auth:`: the `auth.yaml` key of
the ConfigMap `spec.auth.configMapRef` names is the server's `auth:`
block (its `resource`, `rules` and any further providers), and the
operator adds `issuer` and `audience` as a provider unless the block
already lists that issuer. The operator then loads the file with the
server's own loader (`contentsource.Parse`), so a file the server would
refuse at startup is never deployed.

The Deployment runs `mcp serve-http` on port 4005 with the Helm
chart's pod: non-root 65532, read-only root filesystem, no privilege
escalation, every capability dropped, `RuntimeDefault` seccomp, no
ServiceAccount token, an `emptyDir` cache, and the chart's probes. A
checksum of the `content-source.yaml` on the pod template rolls the
pods when it changes.

### Credentials and sizing

Every mob gets a ServiceAccount of its own, `<mob>-meerkat`, controlled
by the mob, and its pods run as it, still without an API token.
`spec.credentials.serviceAccount.annotations` go on it, so a cloud
workload identity (`eks.amazonaws.com/role-arn`,
`iam.gke.io/gcp-service-account`) binds to this ServiceAccount by name:
the cloud side's trust policy decides which mob may assume which role.
If a ServiceAccount of that name already exists and the mob does not
control it, the mob is refused (`ConfigRefused`) rather than take it
over, because other pods may run as it; deleting it lets the mob render.

`spec.credentials.s3SecretRef` names a Secret in the mob's namespace.
Each instance reads `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`
from it, and `AWS_SESSION_TOKEN` when present, as the AWS default chain
does for every `type: s3` source. Only those keys are mapped, never the
whole Secret through `envFrom`, so the Secret cannot set `MEERKAT_*` or
any other variable. The operator does not read the Secret (it has no
RBAC on Secrets): a pod whose Secret is missing waits in
`CreateContainerConfigError`, and `Available` stays `False`. Pods read
it at start, so a rotated key needs a rollout restart.

`spec.resources` replaces the operator's defaults (requests 50m CPU and
128Mi memory, a 512Mi memory limit) for every instance. Each instance's
memory request and limit are then raised by the sum of the
`resources.maxResidentBytes` of the KnowledgeBases it serves; with
neither set, that sum becomes the request. A unit with a memory limit
runs with `GOMEMLIMIT` at 90% of it, so the Go collector works harder
before the kernel kills the pod. The server does not enforce
`maxResidentBytes` itself: it sizes the pod. A negative value or
`spec.resources.claims` refuses the mob (`ConfigRefused`).

The mob is not rendered, and what its last good spec rendered keeps
running, when:

- `spec.root` names no KnowledgeBase (`RootMissing`);
- a KnowledgeBase under the root breaks the depth or cycle rules, run
  again here because admission is time-of-check (`TreeRefused`);
- `spec.image` is refused (`ImageRefused`, below);
- `spec.credentials.serviceAccount.annotations` has a key the API
  server refuses, or the ServiceAccount's name is taken (above), or
  `spec.resources` cannot be applied (`ConfigRefused`);
- `spec.auth` is unset, its ConfigMap or key is missing, or a rendered
  file does not load: for example a KnowledgeBase name with a dot (a
  valid object name, not a collection name), a `refresh` on a bundle, or
  an `auth:` block without a `resource` (`ConfigRefused`, with the
  loader's message). The operator never starts a hosted server without
  authentication.

Status carries `observedGeneration`, `image`, `instances` and
`knowledgeBases`, and two conditions. `Rendered` is `True` once every
unit was applied, or `False` with one of the reasons above. `Available`
is `True` once every unit's Deployment has rolled out its generation
with an available replica, `False` (`Progressing`) before that, and
`Unknown` (`NotRendered`) while the current spec is not rendered.

The mob is reconciled again when it changes, when any KnowledgeBase in
its namespace is created, deleted or changes spec (any of them can join
or leave its tree), when its auth ConfigMap changes, when a
ServiceAccount of its ServiceAccount's name changes or goes away, and
when an object it rendered changes.

### Which image a mob runs

`spec.image` is a privilege boundary: without a check, whoever can write
a MeerkatMob picks the image its pods run, with the S3, GCS and OIDC
configuration the operator gives them. The operator's
`--server-image` is what a mob runs when `spec.image` is empty (the
chart passes its own `image.*`). `--allowed-images` lists the
repositories an override may name: an entry matches one repository
exactly (`ghcr.io/zegit-zoo/meerkat`, the default), or every repository
below it when it ends in `/`. An entry must name its registry host, and
a repository that only starts with an allowed one
(`ghcr.io/zegit-zoo/meerkat-evil`) does not match. The webhook refuses
any other image at admission, and the reconciler refuses it again
(`ImageRefused`) in case it got in while the webhook was down.

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

meerkat-mob#10 tracks these:

- **Cross-process hops.** Each instance serves its unit's
  KnowledgeBases as flat collections. A parent's pointer to a child in
  another unit is not yet rewritten to the sibling Service, so a hop
  does not cross processes, and `mount: lazy` is not yet rendered
  (`collections:` has no lazy mount).
- **Access and dedicated isolation.** `access.paths` and
  `access.identities` are not yet mapped to authz rules, and a dedicated
  unit has its own Deployment but no NetworkPolicy yet.
- **Contract and per-unit identity.** `contract` is not rendered,
  because the server's `update:` block is structured and the CRD field
  is free text. Credentials and resources are per mob, so a dedicated
  KnowledgeBase shares its mob's identity; the server does not enforce
  `maxResidentBytes` itself.
- **KnowledgeBase status from instances.** `mounted`, `pages`,
  `contentGeneration` and `lastTraversal` are not filled in.
- Scale-to-zero, the librarian CronJob, a chart switch to install the
  operator without the meerkat server, a NetworkPolicy for the operator
  pod, and acceptance on a real k3s cluster.
