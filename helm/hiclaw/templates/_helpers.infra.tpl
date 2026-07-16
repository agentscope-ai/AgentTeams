{{/*
Infrastructure abstraction helpers.

Phase 2 reads the new public values API (`matrix.*`, `gateway.*`, `storage.*`).
The Higress dependency still consumes top-level `higress:` values because the
dependency name remains `higress`; `gateway.higress.enabled` is only the
materialized condition flag.
*/}}

{{- define "hiclaw.matrix.internalURL" -}}
{{- if and (eq .Values.matrix.provider "tuwunel") (eq .Values.matrix.mode "managed") -}}
{{- include "hiclaw.tuwunel.internalURL" . -}}
{{- else -}}
{{- .Values.matrix.internalURL | default "" -}}
{{- end -}}
{{- end }}

{{- define "hiclaw.matrix.serverName" -}}
{{- if and (eq .Values.matrix.provider "tuwunel") (eq .Values.matrix.mode "managed") -}}
{{- include "hiclaw.tuwunel.serverName" . -}}
{{- else -}}
{{- .Values.matrix.serverName | default "" -}}
{{- end -}}
{{- end }}

{{- define "hiclaw.gateway.publicURL" -}}
{{- required "gateway.publicURL is required" .Values.gateway.publicURL -}}
{{- end }}

{{- define "hiclaw.gateway.internalURL" -}}
{{- if and (eq .Values.gateway.provider "higress") (eq .Values.gateway.mode "managed") -}}
{{- include "hiclaw.higress.gatewayURL" . -}}
{{- else if eq .Values.gateway.provider "ai-gateway" -}}
{{/* External APIG: workers reach the gateway via its public URL. */ -}}
{{- include "hiclaw.gateway.publicURL" . -}}
{{- else -}}
{{- fail (printf "unsupported gateway combination %s/%s" .Values.gateway.provider .Values.gateway.mode) -}}
{{- end -}}
{{- end }}

{{- define "hiclaw.gateway.adminURL" -}}
{{- if and (eq .Values.gateway.provider "higress") (eq .Values.gateway.mode "managed") -}}
{{- include "hiclaw.higress.consoleURL" . -}}
{{- else if eq .Values.gateway.provider "ai-gateway" -}}
{{/* APIG does not expose a console URL from within the cluster: the
     controller talks to it via the regional Aliyun OpenAPI endpoint, so
     no admin URL is meaningful here. Leave empty to mean "unset" and let
     callers decide whether to guard emission of AGENTTEAMS_AI_GATEWAY_ADMIN_URL. */ -}}
{{- else -}}
{{- fail (printf "unsupported gateway admin combination %s/%s" .Values.gateway.provider .Values.gateway.mode) -}}
{{- end -}}
{{- end }}

{{- define "hiclaw.gateway.higress.enabled" -}}
{{- if and (eq .Values.gateway.provider "higress") (eq .Values.gateway.mode "managed") -}}true{{- else -}}false{{- end -}}
{{- end }}

{{- define "hiclaw.storage.endpoint" -}}
{{- if and (eq .Values.storage.provider "minio") (eq .Values.storage.mode "managed") -}}
{{- include "hiclaw.minio.internalURL" . -}}
{{- else if eq .Values.storage.provider "oss" -}}
{{/* External OSS: the authoritative endpoint is returned by the
     credential-provider sidecar alongside each STS token. If the chart
     user supplies an override (storage.oss.endpoint) we honour it so that
     worker scripts can hard-code mc hosts when the provider isn't
     reachable from the worker network (rare). */ -}}
{{- .Values.storage.oss.endpoint | default "" -}}
{{- else -}}
{{- fail (printf "unsupported storage combination %s/%s" .Values.storage.provider .Values.storage.mode) -}}
{{- end -}}
{{- end }}

{{- define "hiclaw.storage.bucket" -}}
{{- required "storage.bucket is required" .Values.storage.bucket -}}
{{- end }}

{{- define "hiclaw.storage.remoteRoot" -}}
{{- printf "hiclaw/%s" (include "hiclaw.storage.bucket" .) -}}
{{- end }}

{{- define "hiclaw.storage.adminSecretName" -}}
{{- if and (eq .Values.storage.provider "minio") (eq .Values.storage.mode "managed") -}}
{{- include "hiclaw.minio.fullname" . -}}
{{- end -}}
{{- end }}

{{- define "hiclaw.storage.adminAccessKeyKey" -}}
{{- if and (eq .Values.storage.provider "minio") (eq .Values.storage.mode "managed") -}}MINIO_ROOT_USER{{- end -}}
{{- end }}

{{- define "hiclaw.storage.adminSecretKeyKey" -}}
{{- if and (eq .Values.storage.provider "minio") (eq .Values.storage.mode "managed") -}}MINIO_ROOT_PASSWORD{{- end -}}
{{- end }}

{{- define "hiclaw.manager.spec" -}}
{{- $spec := dict
  "model" (.Values.manager.model | default .Values.credentials.defaultModel)
  "runtime" (.Values.manager.runtime | default "openclaw")
  "image" (include "hiclaw.manager.image" .)
  "resources" .Values.manager.resources
-}}
{{- $spec | toJson -}}
{{- end }}
