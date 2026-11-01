{{/* Chart name, overridable. */}}
{{- define "meerkat.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Fully qualified app name, at most 63 characters. */}}
{{- define "meerkat.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "meerkat.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "meerkat.labels" -}}
helm.sh/chart: {{ include "meerkat.chart" . }}
{{ include "meerkat.selectorLabels" . }}
app.kubernetes.io/version: {{ include "meerkat.imageTag" . | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "meerkat.selectorLabels" -}}
app.kubernetes.io/name: {{ include "meerkat.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "meerkat.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "meerkat.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* Image tag: image.tag or appVersion, without a leading "v" (published image tags have none). */}}
{{- define "meerkat.imageTag" -}}
{{- default .Chart.AppVersion .Values.image.tag | trimPrefix "v" }}
{{- end }}

{{- define "meerkat.image" -}}
{{- $ref := printf "%s:%s" .Values.image.repository (include "meerkat.imageTag" .) }}
{{- if .Values.image.digest }}
{{- $ref = printf "%s@%s" $ref .Values.image.digest }}
{{- end }}
{{- $ref }}
{{- end }}

{{/* Container port: server.port, else the mode's CLI default. */}}
{{- define "meerkat.port" -}}
{{- if .Values.server.port }}
{{- .Values.server.port }}
{{- else if eq .Values.server.mode "http" }}
{{- 4004 }}
{{- else }}
{{- 4005 }}
{{- end }}
{{- end }}

{{- define "meerkat.servicePort" -}}
{{- default (include "meerkat.port" .) .Values.service.port }}
{{- end }}

{{/* Name of the ConfigMap holding content-source.yaml, or "" when there is none. */}}
{{- define "meerkat.contentSourceConfigMap" -}}
{{- if .Values.contentSource.existingConfigMap }}
{{- .Values.contentSource.existingConfigMap }}
{{- else if or .Values.contentSource.config .Values.contentSource.raw }}
{{- printf "%s-content-source" (include "meerkat.fullname" .) }}
{{- end }}
{{- end }}

{{/* content-source.yaml body from contentSource.raw (verbatim) or contentSource.config. */}}
{{- define "meerkat.contentSourceBody" -}}
{{- if .Values.contentSource.raw }}
{{- .Values.contentSource.raw | trimSuffix "\n" }}
{{- else }}
{{- toYaml .Values.contentSource.config }}
{{- end }}
{{- end }}

{{/*
"true" when the inline contentSource carries a top-level auth: block. An
existing ConfigMap cannot be inspected, so it reports "unknown".
*/}}
{{- define "meerkat.authConfigured" -}}
{{- if .Values.contentSource.existingConfigMap }}
{{- "unknown" }}
{{- else if .Values.contentSource.raw }}
{{- regexMatch "(?m)^auth:" .Values.contentSource.raw }}
{{- else }}
{{- hasKey .Values.contentSource.config "auth" }}
{{- end }}
{{- end }}

{{/*
server.enabled, defaulting to true when the key is missing.
`helm upgrade --reuse-values` keeps the values stored with a release made
before the key existed, so server.enabled is nil there; plain truthiness
would drop that release's server. Only an explicit false turns it off.
*/}}
{{- define "meerkat.serverEnabled" -}}
{{- ne (toString .Values.server.enabled) "false" -}}
{{- end }}

{{/*
operator.networkPolicy.enabled, false when operator.networkPolicy is
missing (a --reuse-values upgrade from a release made before it existed).
*/}}
{{- define "meerkat.operator.networkPolicyEnabled" -}}
{{- dig "networkPolicy" "enabled" false (.Values.operator | default dict) | toString -}}
{{- end }}

{{- define "meerkat.pdbEnabled" -}}
{{- if kindIs "bool" .Values.podDisruptionBudget.enabled }}
{{- .Values.podDisruptionBudget.enabled }}
{{- else }}
{{- or (gt (int .Values.replicaCount) 1) .Values.autoscaling.enabled }}
{{- end }}
{{- end }}

{{/*
Fail early on combinations that cannot work: a release with nothing in it,
an operator NetworkPolicy that would cut it off from the API server, and
server settings the server would reject at startup.
*/}}
{{- define "meerkat.validate" -}}
{{- $server := eq (include "meerkat.serverEnabled" .) "true" }}
{{- if not (or $server .Values.operator.enabled) }}
{{- fail "server.enabled=false needs operator.enabled=true: with both off the release installs nothing" }}
{{- end }}
{{- if and .Values.operator.enabled (eq (include "meerkat.operator.networkPolicyEnabled" .) "true") (not (or (dig "apiServer" "cidrs" nil .Values.operator.networkPolicy) (dig "apiServer" "peers" nil .Values.operator.networkPolicy))) }}
{{- fail "operator.networkPolicy.enabled needs operator.networkPolicy.apiServer.cidrs or .peers: without a destination for the API server the operator cannot reach it" }}
{{- end }}
{{- if $server }}
{{- if and (eq .Values.server.mode "http") (not .Values.server.apiKey.existingSecret) }}
{{- fail "server.mode=http needs server.apiKey.existingSecret: `http serve` refuses to start without an API key" }}
{{- end }}
{{- if and (eq .Values.server.mode "http") .Values.serviceMonitor.enabled }}
{{- fail "serviceMonitor needs server.mode=mcp: `http serve` has no /metrics endpoint" }}
{{- end }}
{{- if and .Values.contentSource.config .Values.contentSource.raw }}
{{- fail "set contentSource.config or contentSource.raw, not both" }}
{{- end }}
{{- if and .Values.contentSource.existingConfigMap (or .Values.contentSource.config .Values.contentSource.raw) }}
{{- fail "contentSource.existingConfigMap replaces contentSource.config/raw; set one, not both" }}
{{- end }}
{{- end }}
{{- end }}

{{/* ---------- meerkat-operator (operator.enabled) ---------- */}}

{{/* Operator object name prefix, at most 63 characters. */}}
{{- define "meerkat.operator.fullname" -}}
{{- printf "%s-operator" (include "meerkat.fullname" . | trunc 54 | trimSuffix "-") }}
{{- end }}

{{/*
Operator selector labels. app.kubernetes.io/name differs from the server's,
so the server's Service, PodDisruptionBudget and NetworkPolicy never select
operator pods.
*/}}
{{- define "meerkat.operator.selectorLabels" -}}
app.kubernetes.io/name: {{ printf "%s-operator" (include "meerkat.name" . | trunc 54 | trimSuffix "-") }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: operator
{{- end }}

{{- define "meerkat.operator.labels" -}}
helm.sh/chart: {{ include "meerkat.chart" . }}
{{ include "meerkat.operator.selectorLabels" . }}
app.kubernetes.io/part-of: meerkat
app.kubernetes.io/version: {{ include "meerkat.operator.imageTag" . | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "meerkat.operator.serviceAccountName" -}}
{{- default (include "meerkat.operator.fullname" .) .Values.operator.serviceAccount.name }}
{{- end }}

{{- define "meerkat.operator.imageTag" -}}
{{- default .Chart.AppVersion .Values.operator.image.tag | trimPrefix "v" }}
{{- end }}

{{- define "meerkat.operator.image" -}}
{{- $ref := printf "%s:%s" .Values.operator.image.repository (include "meerkat.operator.imageTag" .) }}
{{- if .Values.operator.image.digest }}
{{- $ref = printf "%s@%s" $ref .Values.operator.image.digest }}
{{- end }}
{{- $ref }}
{{- end }}

{{/* Webhook Service and serving-certificate names. */}}
{{- define "meerkat.operator.webhookService" -}}
{{- printf "%s-webhook" (include "meerkat.operator.fullname" .) }}
{{- end }}

{{- define "meerkat.operator.certSecret" -}}
{{- printf "%s-webhook-cert" (include "meerkat.operator.fullname" .) }}
{{- end }}

{{/* "true" when the chart serves the webhook. */}}
{{- define "meerkat.operator.webhookEnabled" -}}
{{- and .Values.operator.enabled .Values.operator.webhook.enabled }}
{{- end }}

{{/*
The namespaces the operator watches, comma-separated: operator.watchNamespaces,
or the release namespace when that is empty or missing (a --reuse-values
upgrade from a release without it). "*" is every namespace.
*/}}
{{- define "meerkat.operator.watchNamespaces" -}}
{{- join "," (uniq (.Values.operator.watchNamespaces | default (list .Release.Namespace))) }}
{{- end }}
