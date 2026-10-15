# meerkat Helm chart

Deploys [meerkat](../../README.md) from `ghcr.io/zegit-zoo/meerkat` on
Kubernetes: the hosted MCP server (`mk mcp serve-http`, the default) or the
HTTP/OpenAPI server (`mk http serve`, `server.mode: http`).

The chart is not published to a chart repository or as an OCI artifact
yet. Install it from a checkout:

```bash
helm install meerkat ./charts/meerkat -n meerkat --create-namespace -f my-values.yaml
```

Chart `0.1.0` defaults to meerkat `0.18.0` (`appVersion`). Image tags have
no leading `v`: git tag `v0.18.0` publishes image tag `0.18.0`, and the
chart strips a `v` from `image.tag` if you give one. Pin by digest with
`image.digest`; see [docs/CONTAINER.md](../../docs/CONTAINER.md) for
verifying the image with cosign first.

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

`values.schema.json` validates all of it, rejects unknown top-level keys,
and enforces the `server.mode` enum and the image fields.

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
