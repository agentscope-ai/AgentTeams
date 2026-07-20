{{/*
Chart name.
*/}}
{{- define "agentteams.name" -}}
{{- if and (eq .Release.Name "hiclaw") (not .Values.nameOverride) -}}
{{- "hiclaw" -}}
{{- else -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end }}

{{/*
Fully qualified app name.
*/}}
{{- define "agentteams.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else if and (eq .Release.Name "hiclaw") (not .Values.nameOverride) }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Chart label.
*/}}
{{- define "agentteams.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Namespace for all resources.
*/}}
{{- define "agentteams.namespace" -}}
{{- default .Release.Namespace .Values.global.namespace }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "agentteams.commonLabels" -}}
helm.sh/chart: {{ include "agentteams.chart" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
{{- end }}

{{/*
Global image tag: uses explicit global.imageTag if set, otherwise derives from Chart.AppVersion.
Usage: include "agentteams.globalImageTag" .
*/}}
{{- define "agentteams.globalImageTag" -}}
{{- if .Values.global.imageTag }}
{{-   .Values.global.imageTag }}
{{- else }}
{{-   printf "v%s" .Chart.AppVersion }}
{{- end }}
{{- end }}

{{/*
Image tag: defaults to global.imageTag.
Usage: include "agentteams.imageTag" (dict "tag" .Values.foo.image.tag "global" .Values.global "root" $)
*/}}
{{- define "agentteams.imageTag" -}}
{{- $tag := .tag }}
{{- if not $tag }}
{{-   $tag = .global.imageTag }}
{{- end }}
{{- if not $tag }}
{{-   $tag = printf "v%s" .root.Chart.AppVersion }}
{{- end }}
{{- $tag }}
{{- end }}

{{/*
Shared runtime Secret name.
*/}}
{{- define "agentteams.secretName" -}}
{{- printf "%s-runtime-env" (include "agentteams.fullname" .) }}
{{- end }}

{{/* ── Component naming helpers ────────────────────────────────────────── */}}

{{- define "agentteams.manager.fullname" -}}
{{- printf "%s-manager" (include "agentteams.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "agentteams.controller.fullname" -}}
{{- printf "%s-controller" (include "agentteams.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "agentteams.tuwunel.fullname" -}}
{{- printf "%s-tuwunel" (include "agentteams.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "agentteams.minio.fullname" -}}
{{- printf "%s-minio" (include "agentteams.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "agentteams.elementWeb.fullname" -}}
{{- printf "%s-element-web" (include "agentteams.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* ── Component label helpers ─────────────────────────────────────────── */}}

{{- define "agentteams.component.labels" -}}
{{ include "agentteams.commonLabels" .root }}
{{ include "agentteams.component.selectorLabels" . }}
{{- end }}

{{- define "agentteams.component.selectorLabels" -}}
app.kubernetes.io/name: {{ include "agentteams.name" .root }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{/* ── Service URL helpers ─────────────────────────────────────────────── */}}

{{- define "agentteams.tuwunel.clusterFQDN" -}}
{{- printf "%s.%s.svc.cluster.local" (include "agentteams.tuwunel.fullname" .) (include "agentteams.namespace" .) }}
{{- end }}

{{- define "agentteams.tuwunel.internalURL" -}}
{{- printf "http://%s:%d" (include "agentteams.tuwunel.clusterFQDN" .) (.Values.matrix.tuwunel.service.port | int) }}
{{- end }}

{{- define "agentteams.tuwunel.serverName" -}}
{{- if .Values.matrix.serverName }}
{{- .Values.matrix.serverName }}
{{- else }}
{{- include "agentteams.tuwunel.clusterFQDN" . }}
{{- end }}
{{- end }}

{{- define "agentteams.minio.internalURL" -}}
{{- printf "http://%s.%s.svc.cluster.local:%d" (include "agentteams.minio.fullname" .) (include "agentteams.namespace" .) (.Values.storage.minio.service.apiPort | int) }}
{{- end }}

{{- define "agentteams.controller.internalURL" -}}
{{- printf "http://%s.%s.svc.cluster.local:%d" (include "agentteams.controller.fullname" .) (include "agentteams.namespace" .) (.Values.controller.service.port | int) }}
{{- end }}

{{- define "agentteams.higress.consoleURL" -}}
{{- printf "http://higress-console.%s.svc.cluster.local:8080" (include "agentteams.namespace" .) }}
{{- end }}

{{- define "agentteams.higress.gatewayURL" -}}
{{- $port := 80 }}
{{- if and .Values.higress (index .Values.higress "higress-core") }}
{{- $gw := index (index .Values.higress "higress-core") "gateway" | default dict }}
{{- $port = $gw.httpPort | default 80 }}
{{- end }}
{{- printf "http://higress-gateway.%s.svc.cluster.local:%d" (include "agentteams.namespace" .) ($port | int) }}
{{- end }}

{{/* ── ServiceAccount helpers ──────────────────────────────────────────── */}}

{{- define "agentteams.controller.serviceAccountName" -}}
{{- if .Values.controller.serviceAccount.create }}
{{- default (include "agentteams.controller.fullname" .) .Values.controller.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.controller.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* ── Manager image helper (used by controller to create Manager CR) ──── */}}

{{- define "agentteams.manager.image" -}}
{{- $tag := default (include "agentteams.globalImageTag" .) .Values.manager.image.tag }}
{{- printf "%s:%s" .Values.manager.image.repository $tag }}
{{- end }}

{{/* ── Worker image helpers ────────────────────────────────────────────── */}}

{{- define "agentteams.worker.openclawImage" -}}
{{- $tag := default (include "agentteams.globalImageTag" .) .Values.worker.defaultImage.openclaw.tag }}
{{- printf "%s:%s" .Values.worker.defaultImage.openclaw.repository $tag }}
{{- end }}

{{- define "agentteams.worker.copawImage" -}}
{{- $tag := default (include "agentteams.globalImageTag" .) .Values.worker.defaultImage.copaw.tag }}
{{- printf "%s:%s" .Values.worker.defaultImage.copaw.repository $tag }}
{{- end }}

{{- define "agentteams.worker.hermesImage" -}}
{{- $tag := default (include "agentteams.globalImageTag" .) .Values.worker.defaultImage.hermes.tag }}
{{- printf "%s:%s" .Values.worker.defaultImage.hermes.repository $tag }}
{{- end }}

{{- define "agentteams.worker.openhumanImage" -}}
{{- $tag := default (include "agentteams.globalImageTag" .) .Values.worker.defaultImage.openhuman.tag }}
{{- printf "%s:%s" .Values.worker.defaultImage.openhuman.repository $tag }}
{{- end }}
