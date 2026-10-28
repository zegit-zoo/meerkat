# meerkat Helm chart

Deploys [meerkat](../../README.md) from `ghcr.io/zegit-zoo/meerkat` on
Kubernetes: the hosted MCP server (`mk mcp serve-http`, the default) or the
HTTP/OpenAPI server (`mk http serve`, `server.mode: http`).

## Installing

Each meerkat release publishes the chart as a signed OCI artifact, versioned
in lockstep with the app: git tag `vX.Y.Z` publishes
`oci://ghcr.io/zegit-zoo/charts/meerkat` version `X.Y.Z`, whose `appVersion`
(the default image tag) is `X.Y.Z` too. Chart and image versions have no
leading `v`. The first release that publishes the chart is the one after
v0.19.0.

```bash
helm install meerkat oci://ghcr.io/zegit-zoo/charts/meerkat --version 0.20.0 \
  -n meerkat --create-namespace -f my-values.yaml
```

Verify the chart before you install it. It is signed keyless by the release
workflow, the same identity as the image:

```bash
cosign verify \
  --certificate-identity-regexp '^https://github.com/zegit-zoo/meerkat/\.github/workflows/release\.yml@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  ghcr.io/zegit-zoo/charts/meerkat:0.20.0
```

Pin the image by digest with `image.digest`; see
[docs/CONTAINER.md](../../docs/CONTAINER.md) for verifying the image with
cosign first. `image.tag` overrides the chart's `appVersion`, and a leading
`v` in it is stripped.

### Flux

Point an `OCIRepository` at the chart and let Flux verify the signature, then
reference it from the `HelmRelease`. Renovate reads OCI tags (the `docker`
datasource), so pinning `ref.tag` gets you a bump PR for each release:

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: meerkat
  namespace: meerkat
spec:
  interval: 1h
  url: oci://ghcr.io/zegit-zoo/charts/meerkat
  ref:
    tag: 0.20.0
  layerSelector:
    mediaType: application/vnd.cncf.helm.chart.content.v1.tar+gzip
    operation: copy
  verify:
    provider: cosign
    matchOIDCIdentity:
      - issuer: ^https://token\.actions\.githubusercontent\.com$
        subject: ^https://github\.com/zegit-zoo/meerkat/\.github/workflows/release\.yml@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: meerkat
  namespace: meerkat
spec:
  interval: 1h
  chartRef:
    kind: OCIRepository
    name: meerkat
  values: {}
```

### From a checkout

In git the chart carries the placeholder version `0.0.0-dev`, so a checkout
has no real default image tag. Set one:

```bash
helm install meerkat ./charts/meerkat -n meerkat --create-namespace \
  -f my-values.yaml --set image.tag=0.20.0
```

## Authentication

With **no `auth:` block**, `mcp serve-http` serves every mounted collection
to any caller, so it **refuses to start** on `0.0.0.0` (which a pod must
bind) unless authentication is configured or explicitly waived. The
defaults therefore render, but the pod exits at startup until you choose
one of:

1. **OIDC (recommended).** Put an `auth:` block in `contentSource.config`:

   ```yaml
   contentSource:
     config:
       content:
         type: s3
         bucket: my-org-knowledge
         prefix: kb/live/
         endpoint: https://s3.example.net
         region: garage
         path_style: true
       auth:
         resource: https://mcp.example.com/mcp
         providers:
           - issuer: https://login.microsoftonline.com/<tenant-id>/v2.0
             audience: api://meerkat
             claims: { groups: groups, tenant: tid }
         rules:
           - name: everyone
             groups: [staff]
             collections: ["*"]
             capabilities: [read]
   ```

2. **A gateway that authenticates every request.** Set
   `server.insecureNoAuth: true` (passes `--insecure-no-auth`), or put
   `auth: {allow_unauthenticated: true}` in `contentSource.config`. Turn on
   `networkPolicy` with `ingressFrom` naming the gateway, so nothing can
   reach the pod around it.

`helm install` prints a notice when it can tell that neither is set. See
[Authentication and per-collection authorization](../../README.md#authentication-and-per-collection-authorization)
for the policy format.

`server.mode: http` uses a static bearer key instead and has no anonymous
mode: `server.apiKey.existingSecret` is required, and rendering fails
without it.

## Credentials

Nothing secret goes in values. Create Secrets out of band and reference
them:

| purpose | values | becomes |
| --- | --- | --- |
| S3 content, memory, intake | `secrets.s3.existingSecret` | `envFrom` the Secret (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, optionally `AWS_REGION`) |
| traversal-log HMAC key | `secrets.traversalLogKey.{existingSecret,key,envName}` | `envName` from the Secret key; `envName` must equal `observability.traversal_log.hmac_key_env` |
| HTTP API key | `server.apiKey.{existingSecret,key,mountAsFile}` | `MEERKAT_API_KEY`, or a mounted file and `--api-key-file` |
| anything else (OTLP headers, …) | `secrets.env[]` (`name`, `secretName`, `key`, `optional`) | `env[].valueFrom.secretKeyRef` |

OIDC token verification needs no secret: meerkat only fetches the issuer's
public keys. For GCS or IRSA, annotate the ServiceAccount through
`serviceAccount.annotations`.

## Values

| key | default | description |
| --- | --- | --- |
| `replicaCount` | `1` | ignored when `autoscaling.enabled` |
| `image.repository` | `ghcr.io/zegit-zoo/meerkat` | |
| `image.tag` | `""` | defaults to `appVersion`; a leading `v` is stripped |
| `image.digest` | `""` | `sha256:…`; renders `repository:tag@digest` |
| `image.pullPolicy` | `IfNotPresent` | |
| `imagePullSecrets` | `[]` | |
| `server.enabled` | `true` | `false` installs only the operator; see [Operator only](#operator-only) |
| `server.mode` | `mcp` | `mcp` (`mcp serve-http`) or `http` (`http serve`) |
| `server.port` | `null` | container and Service port; `null` is 4005 (mcp) or 4004 (http) |
| `server.path` | `/mcp` | mcp: `--path` |
| `server.stateful` | `false` | mcp: `--stateful`; needs sticky routing with several replicas |
| `server.insecureNoAuth` | `false` | mcp: `--insecure-no-auth`; see [Authentication](#authentication) |
| `server.securityTxt` | `true` | `--security-txt` |
| `server.apiKey.existingSecret` | `""` | http: Secret holding the bearer key (required in http mode) |
| `server.apiKey.key` | `api-key` | key in that Secret |
| `server.apiKey.mountAsFile` | `false` | mount it and pass `--api-key-file` instead of `MEERKAT_API_KEY` |
| `server.extraArgs` | `[]` | appended to the generated arguments |
| `contentSource.config` | `{}` | `content-source.yaml` as an object |
| `contentSource.raw` | `""` | the same file as a string, verbatim; set this or `config` |
| `contentSource.existingConfigMap` | `""` | use this ConfigMap instead of rendering one |
| `contentSource.key` | `content-source.yaml` | key holding the file in the ConfigMap |
| `cache.existingClaim` | `""` | PVC for `/home/nonroot/.cache` instead of an `emptyDir` |
| `cache.sizeLimit` | `1Gi` | `emptyDir` size limit |
| `cache.medium` | `""` | `Memory` for a tmpfs `emptyDir` |
| `secrets.*` | | see [Credentials](#credentials) |
| `extraEnv`, `extraEnvFrom` | `[]` | raw `EnvVar` / `EnvFromSource` objects |
| `extraVolumes`, `extraVolumeMounts` | `[]` | raw volumes and mounts |
| `serviceAccount.create` | `true` | |
| `serviceAccount.name` | `""` | defaults to the release fullname |
| `serviceAccount.annotations` | `{}` | e.g. IRSA or GKE workload identity |
| `automountServiceAccountToken` | `false` | meerkat never calls the Kubernetes API |
| `podAnnotations`, `podLabels` | `{}` | |
| `podSecurityContext` | non-root 65532, `fsGroup` 65532, `RuntimeDefault` seccomp | |
| `securityContext` | read-only root, no privilege escalation, drop `ALL` | |
| `resources` | requests `50m`/`128Mi`, limit `512Mi` memory | no CPU limit |
| `probes.{startup,liveness,readiness}` | enabled | timing fields; paths follow the mode |
| `terminationGracePeriodSeconds` | `30` | |
| `service.type` | `ClusterIP` | |
| `service.port` | `null` | defaults to the container port |
| `service.annotations` | `{}` | |
| `ingress.*` | disabled | `className`, `annotations`, `hosts[].paths[]`, `tls[]` |
| `httpRoute.*` | disabled | Gateway API v1: `parentRefs` (required), `hostnames`, `rules` |
| `serviceMonitor.*` | disabled | mcp only: `labels`, `interval`, `scrapeTimeout` |
| `podDisruptionBudget.enabled` | `null` | `null` creates one when `replicaCount > 1` or autoscaling is on |
| `podDisruptionBudget.minAvailable` | `1` | |
| `podDisruptionBudget.maxUnavailable` | `null` | when set, used instead of `minAvailable` |
| `autoscaling.*` | disabled | `minReplicas` 2, `maxReplicas` 5, CPU 80 % |
| `networkPolicy.enabled` | `false` | |
| `networkPolicy.ingressFrom` | `[]` | peers allowed to the server port; empty allows any source to that port only |
| `networkPolicy.egress.enabled` | `false` | restrict egress to DNS plus `egress.rules` |
| `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints`, `priorityClassName` | empty | |

| `operator.*` | disabled | see [Operator](#operator) |

`values.schema.json` validates all of it, rejects unknown top-level keys,
and enforces the `server.mode` enum and the image fields.

## Operator

`operator.enabled=true` also installs `meerkat-operator`
(meerkat-mob#10): the `KnowledgeBase` and `MeerkatMob` CRDs, their
validating webhook, the reconciler that reports each knowledge-base
tree in status, and the reconciler that serves a `MeerkatMob`'s tree as
meerkat Deployments ([docs/design/operator.md](../../docs/design/operator.md)).

A `MeerkatMob`'s instances run `operator.serverImage`, which defaults to
this chart's own `image.*`, so a chart upgrade moves the operator and
the servers it renders together. `spec.image` may override it only with
a repository in `operator.allowedImages`: whoever can write a
`MeerkatMob` would otherwise pick the image its pods run.

It installs cluster-scoped objects: the two CRDs, a ClusterRole and its
binding, and a `ValidatingWebhookConfiguration`. Install it from one
release per cluster. The CRDs, the ClusterRole's rules and the webhook
configuration are rendered from `files/operator/`, copies of what
`make crds` generates from the code, so they cannot drift from the binary.
The ClusterRole lets the operator create, change and delete Deployments,
Services, ConfigMaps, ServiceAccounts and NetworkPolicies in every
namespace, because a `MeerkatMob` may live in any of them. It grants no access to Secrets.

| key | default | description |
| --- | --- | --- |
| `operator.enabled` | `false` | |
| `operator.replicaCount` | `1` | one replica reconciles (leader election); every replica serves the webhook |
| `operator.image.repository` | `ghcr.io/zegit-zoo/meerkat-operator` | |
| `operator.image.tag`, `.digest`, `.pullPolicy` | as `image.*` | the tag defaults to `appVersion` |
| `operator.serverImage` | `""` | `--server-image`: what a `MeerkatMob` runs without `spec.image`; empty is this chart's `image.*` |
| `operator.allowedImages` | `[ghcr.io/zegit-zoo/meerkat]` | `--allowed-images`: repositories `spec.image` may name, exact or, ending in `/`, as a prefix; empty allows no override |
| `operator.crds.install` | `true` | `false` when another release or GitOps owns the CRDs |
| `operator.crds.keep` | `true` | `helm.sh/resource-policy: keep`, so an uninstall keeps the CRDs and every KnowledgeBase; they keep the release's ownership annotations, so a later install under another release name needs `crds.install=false` |
| `operator.serviceAccount.{name,annotations}` | `""`, `{}` | name defaults to `<fullname>-operator` |
| `operator.webhook.enabled` | `true` | off: no admission checks, `--webhooks=false` |
| `operator.webhook.port` | `9443` | container port; the Service listens on 443 |
| `operator.webhook.certManager.enabled` | `true` | serving certificate from cert-manager |
| `operator.webhook.certManager.issuerRef` | `{}` | `{name, kind, group}`; empty creates a self-signed `Issuer` |
| `operator.webhook.certManager.duration`, `.renewBefore` | `2160h`, `360h` | |
| `operator.webhook.selfSigned.validityDays` | `365` | Helm-generated certificate, when cert-manager is off |
| `operator.resources` | requests `10m`/`64Mi`, limit `256Mi` memory | |
| `operator.extraArgs` | `[]` | e.g. `--metrics-bind-address=:8080` |
| `operator.networkPolicy.enabled` | `false` | see [Operator NetworkPolicy](#operator-networkpolicy) |
| `operator.networkPolicy.apiServer.cidrs` | `[]` | API server destinations as `ipBlock` CIDRs; this or `.peers` is required when enabled |
| `operator.networkPolicy.apiServer.peers` | `[]` | API server destinations as `NetworkPolicyPeer` objects |
| `operator.networkPolicy.apiServer.ports` | `[443, 6443]` | TCP ports on those destinations; empty allows every port |
| `operator.networkPolicy.extraIngress`, `.extraEgress` | `[]` | more `NetworkPolicyIngressRule` / `NetworkPolicyEgressRule` objects |
| `operator.podAnnotations`, `.priorityClassName`, `.nodeSelector`, `.tolerations`, `.affinity` | empty | |

The pod uses the chart's `podSecurityContext` and `securityContext`, and
mounts its ServiceAccount token, because it talks to the API server.

### Operator only

`server.enabled=false` (with `operator.enabled=true`) installs the
operator without a meerkat server of its own: the CRDs, RBAC, the
operator Deployment and the webhook, and none of the server's
Deployment, Service, ServiceAccount, content-source ConfigMap, Ingress,
HTTPRoute, ServiceMonitor, PodDisruptionBudget, HPA or NetworkPolicy.
The servers then come only from `MeerkatMob` objects. Every `server.*`,
`contentSource.*`, `secrets.*` and other server value is ignored, and so
are the server's render-time checks (`server.mode=http` without an API
key renders). Some values are still used by the operator:

- `image.*` is the default `operator.serverImage`, the image a
  `MeerkatMob` runs without `spec.image`;
- `imagePullSecrets`, `podSecurityContext` and `securityContext` apply
  to the operator pod.

Rendering fails when both `server.enabled` and `operator.enabled` are
false, because the release would install nothing.

Only an explicit `false` turns the server off. `helm upgrade
--reuse-values` from a release made before `server.enabled` existed
keeps that release's stored values, which have no `server.enabled`; the
chart treats the missing key as `true`, so the server stays.

```bash
helm install meerkat-operator oci://ghcr.io/zegit-zoo/charts/meerkat --version 0.20.0 \
  -n meerkat-system --create-namespace \
  --set server.enabled=false --set operator.enabled=true
```

### Operator NetworkPolicy

`operator.networkPolicy.enabled=true` puts a NetworkPolicy on the
operator pod that isolates it for ingress and egress:

- **Ingress:** the webhook port, from any source. The API server is the
  only caller, but the address its calls come from differs per cluster
  and per network plugin (a node IP, a control-plane address outside the
  pod network, a tunnel address), so the policy does not try to name it.
  The webhook does not authenticate its caller (no client certificates),
  so any pod that can reach the port can send it an AdmissionReview; it
  answers allowed or refused and changes nothing. Without the webhook
  the policy admits no ingress at all. The kubelet's probes on the health
  port need no rule, because the NetworkPolicy spec allows traffic from
  the pod's own node. Like the rest of the policy, that is enforced by
  the network plugin, so check that yours does before relying on it. A metrics port opened with
  `--metrics-bind-address` needs an `extraIngress` rule.
- **Egress:** DNS (port 53, UDP and TCP, to any destination), and the
  API server at `operator.networkPolicy.apiServer`, on
  `apiServer.ports`. Rendering fails when neither `apiServer.cidrs` nor
  `apiServer.peers` is set, because the operator would lose the API
  server and stop reconciling. `extraEgress` adds more rules.

The chart checks `apiServer.cidrs` only for shape: an IPv4 address with
a prefix up to `/32`, or an address containing `:` with a prefix up to
`/128`. It does not check that the address itself is valid. `peers`,
`extraIngress` and `extraEgress` are passed through as given; the API
server validates them when the release is applied, so a malformed entry
fails `helm install` rather than `helm template`.

Limits, which come from the NetworkPolicy API rather than this chart:

- **There is no portable way to name the API server.** Most network
  plugins apply the policy after the `kubernetes` Service's DNAT, so the
  Service IP alone (`10.96.0.1`) is not enough: list the endpoint
  addresses as well
  (`kubectl get endpointslices -n default -l kubernetes.io/service-name=kubernetes`),
  on the endpoint port (often 6443) as well as 443. Whether DNAT comes
  before or after policy is left to each plugin. When the endpoints
  change (a control-plane node replaced, a managed cluster's endpoint
  rotated), update the CIDRs or the operator loses the API server.
- **Some plugins do not match the API server with `ipBlock`.** Cilium,
  for one, treats the API server as the `kube-apiserver` entity and may
  need its own `CiliumNetworkPolicy` (`toEntities: [kube-apiserver]`)
  next to or instead of this one. Check your plugin's documentation, and
  watch the operator log for timeouts after you enable the policy.
- **A plugin without NetworkPolicy support ignores the object.** The
  policy is then only documentation.
- **A wrong destination can go unnoticed until the operator
  reconnects.** Some plugins let a new pod's connections through until
  they have seen the pod; kind's kindnet does. The operator opens its
  API server connection within milliseconds of starting and keeps it, so
  a fresh pod can work with wrong `apiServer` destinations and fail only
  after a reconnect, hours later. To test the destinations, restart the
  operator container in its running pod (so the plugin already knows
  it) and check that it acquires its leader lease. The pod stays Ready
  while it cannot reach the API server, because its readiness probe does
  not check that connection.
- A pod on the same node as the API server reaches it regardless of the
  egress rules where the plugin allows node traffic as the spec says; a
  single-node cluster therefore cannot show that the egress rule is
  right.

### Webhook certificate

The API server calls the webhook over TLS, so the operator needs a
serving certificate for `<fullname>-operator-webhook.<namespace>.svc`,
and the `ValidatingWebhookConfiguration` needs the CA that signed it.

- **cert-manager (default).** The chart creates a `Certificate` from
  `operator.webhook.certManager.issuerRef`, or from a self-signed `Issuer`
  it creates when that is empty, and annotates the webhook configuration
  with `cert-manager.io/inject-ca-from`. cert-manager's CA injector fills in
  the `caBundle` and cert-manager renews the certificate; the operator
  reloads it from the mounted Secret. cert-manager must be installed first.
- **Self-signed by Helm** (`operator.webhook.certManager.enabled=false`).
  On install the chart generates a CA and a serving certificate, valid for
  `operator.webhook.selfSigned.validityDays`, stores them in the Secret
  `<fullname>-operator-webhook-cert`, and writes the CA into the webhook
  configuration. An upgrade reads the Secret back (`lookup`) and reuses
  it. Nothing renews it, so rotate it before it expires: delete the
  Secret and run `helm upgrade` again, which generates a new pair and
  rolls the CA in the same release. `helm template` and a GitOps tool
  that renders without cluster access (Flux's `helm-controller` does have
  access) cannot read the Secret back, and generate a new pair on every
  render; use cert-manager there.

The KnowledgeBase and MeerkatMob create/update webhooks fail closed: while
no operator replica is ready, those writes are refused. Deletes are
admitted (`failurePolicy: Ignore`).

## What the pod looks like

- **Probes.** In mcp mode the startup and liveness probes hit `/livez` and
  readiness hits `/readyz`; in http mode all three hit `/healthz`. The
  listener opens only after content is resolved and indexed, so the startup
  probe allows ten minutes (`probes.startup.failureThreshold` × `periodSeconds`)
  before liveness takes over.
- **Filesystem.** The root filesystem is read-only. `/home/nonroot/.cache`
  is the only writable path, which is where `type: url`, `gcs` and `s3`
  content is cached; an `emptyDir` re-fetches after every restart, a PVC
  keeps the cache.
- **Config.** `content-source.yaml` is mounted read-only at
  `/home/nonroot/.config/content-source.yaml` and named by
  `MEERKAT_CONTENT_SOURCE`. meerkat reads it once at startup. A
  `checksum/content-source` annotation rolls the pod when the rendered
  file changes; an `existingConfigMap` edit needs `kubectl rollout restart`.
- **Metrics.** `/metrics` is on the server port and unauthenticated by
  design, as are the probes (see
  [docs/design/hosted-mcp.md](../../docs/design/hosted-mcp.md#production-endpoints)).
  An Ingress or HTTPRoute exposing `/` also exposes them; route only
  `server.path` and `/.well-known/` through `httpRoute.rules` or the
  Ingress paths if that matters to you.

## Validating changes

```bash
make chart-check   # helm lint --strict + helm template | kubeconform, for
                   # the defaults and every ci/*-values.yaml
```
