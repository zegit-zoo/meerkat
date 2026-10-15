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

{{- define "meerkat.pdbEnabled" -}}
{{- if kindIs "bool" .Values.podDisruptionBudget.enabled }}
{{- .Values.podDisruptionBudget.enabled }}
{{- else }}
{{- or (gt (int .Values.replicaCount) 1) .Values.autoscaling.enabled }}
{{- end }}
{{- end }}

{{/* Fail early on combinations the server would reject at startup. */}}
{{- define "meerkat.validate" -}}
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
