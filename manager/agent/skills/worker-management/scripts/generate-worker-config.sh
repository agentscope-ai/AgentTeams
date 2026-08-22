#!/bin/bash
# generate-worker-config.sh - Generate Worker openclaw.json from template
#
# Usage:
#   generate-worker-config.sh <WORKER_NAME> <MATRIX_TOKEN> <GATEWAY_KEY> [MODEL_ID] [TEAM_LEADER_NAME]
#
# Reads env vars: HICLAW_MATRIX_DOMAIN, HICLAW_AI_GATEWAY_DOMAIN, HICLAW_ADMIN_USER, HICLAW_DEFAULT_MODEL
# Output: /root/hiclaw-fs/agents/<WORKER_NAME>/openclaw.json
#
# If TEAM_LEADER_NAME is provided, groupAllowFrom and dm.allowFrom will use
# [Leader, Admin] instead of [Manager, Admin].

set -e
source /opt/hiclaw/scripts/lib/hiclaw-env.sh

WORKER_NAME="$1"
WORKER_MATRIX_TOKEN="$2"
WORKER_GATEWAY_KEY="$3"
MODEL_NAME="${4:-${HICLAW_DEFAULT_MODEL:-qwen3.5-plus}}"
TEAM_LEADER_NAME="${5:-}"
# Strip provider prefix if caller passed "hiclaw-gateway/<model>" by mistake
MODEL_NAME="${MODEL_NAME#hiclaw-gateway/}"

if [ -z "${WORKER_NAME}" ] || [ -z "${WORKER_MATRIX_TOKEN}" ] || [ -z "${WORKER_GATEWAY_KEY}" ]; then
    echo "Usage: generate-worker-config.sh <WORKER_NAME> <MATRIX_TOKEN> <GATEWAY_KEY> [MODEL_ID]"
    exit 1
fi

MATRIX_DOMAIN="${HICLAW_MATRIX_DOMAIN:-matrix-local.hiclaw.io:8080}"
AI_GATEWAY_DOMAIN="${HICLAW_AI_GATEWAY_DOMAIN:-aigw-local.hiclaw.io}"
ADMIN_USER="${HICLAW_ADMIN_USER:-admin}"

# Matrix Domain for user IDs (keep original port like :9080)
# Matrix Server for connection uses internal port 8080
MATRIX_DOMAIN_FOR_ID="${MATRIX_DOMAIN}"
MATRIX_SERVER_PORT="8080"

case "${MODEL_NAME}" in
    gpt-5.3-codex|gpt-5-mini|gpt-5-nano)
        CTX=400000; MAX=128000 ;;
    claude-opus-4-6)
        CTX=1000000; MAX=128000 ;;
    claude-sonnet-4-6)
        CTX=1000000; MAX=64000 ;;
    claude-haiku-4-5)
        CTX=200000; MAX=64000 ;;
    qwen3.5-plus)
        CTX=200000; MAX=64000 ;;
    deepseek-chat|deepseek-reasoner|kimi-k2.5)
        CTX=256000; MAX=128000 ;;
    glm-5|MiniMax-M2.7|MiniMax-M2.7-highspeed|MiniMax-M2.5)
        CTX=200000; MAX=128000 ;;
    *)
        CTX=150000; MAX=128000 ;;
esac

# Override with user-supplied custom model parameters from env (set during install)
[ -n "${HICLAW_MODEL_CONTEXT_WINDOW:-}" ] && CTX="${HICLAW_MODEL_CONTEXT_WINDOW}"
[ -n "${HICLAW_MODEL_MAX_TOKENS:-}" ] && MAX="${HICLAW_MODEL_MAX_TOKENS}"

# Resolve input modalities: only vision-capable models get "image"
case "${MODEL_NAME}" in
    gpt-5.4|gpt-5.3-codex|gpt-5-mini|gpt-5-nano|claude-opus-4-6|claude-sonnet-4-6|claude-haiku-4-5|qwen3.5-plus|kimi-k2.5)
        INPUT='["text", "image"]' ;;
    *)
        INPUT='["text"]' ;;
esac
# Override with user-supplied vision setting from env
if [ "${HICLAW_MODEL_VISION:-}" = "true" ]; then
    INPUT='["text", "image"]'
elif [ "${HICLAW_MODEL_VISION:-}" = "false" ]; then
    INPUT='["text"]'
fi

GATEWAY_AUTH_TOKEN=$(openssl rand -hex 32)

export WORKER_NAME
export WORKER_GATEWAY_AUTH_TOKEN="${GATEWAY_AUTH_TOKEN}"
export WORKER_MATRIX_TOKEN
export WORKER_GATEWAY_KEY
# Matrix Server URL:
#   Cloud mode: Worker connects directly via NLB (HICLAW_MATRIX_URL), not through Higress
#   Local mode: always use fixed internal domain so workers on hiclaw-net can reach Higress
#   regardless of user-configured Matrix domain (Higress matrix-homeserver route uses domains:[])
if [ "${HICLAW_RUNTIME}" = "aliyun" ] && [ -n "${HICLAW_MATRIX_URL:-}" ]; then
    export HICLAW_MATRIX_SERVER="${HICLAW_MATRIX_URL}"
else
    export HICLAW_MATRIX_SERVER="http://matrix-local.hiclaw.io:8080"
fi
# Matrix Domain for user IDs keeps original port (e.g., :9080)
export HICLAW_MATRIX_DOMAIN="${MATRIX_DOMAIN_FOR_ID}"
# AI Gateway URL: cloud uses HICLAW_AI_GATEWAY_URL; local always uses fixed internal domain
# so workers on hiclaw-net can reach Higress regardless of user-configured AI gateway domain.
if [ "${HICLAW_RUNTIME}" = "aliyun" ] && [ -n "${HICLAW_AI_GATEWAY_URL:-}" ]; then
    export HICLAW_AI_GATEWAY="${HICLAW_AI_GATEWAY_URL}"
else
    export HICLAW_AI_GATEWAY="http://aigw-local.hiclaw.io:8080"
fi
export HICLAW_ADMIN_USER="${ADMIN_USER}"
export HICLAW_DEFAULT_MODEL="${MODEL_NAME}"
export MODEL_REASONING=true
# Override with user-supplied reasoning setting from env
[ -n "${HICLAW_MODEL_REASONING:-}" ] && export MODEL_REASONING="${HICLAW_MODEL_REASONING}"
export MODEL_CONTEXT_WINDOW="${CTX}"
export MODEL_MAX_TOKENS="${MAX}"
export MODEL_INPUT="${INPUT}"

# E2EE: convert HICLAW_MATRIX_E2EE to JSON boolean for template substitution
if [ "${HICLAW_MATRIX_E2EE:-0}" = "1" ] || [ "${HICLAW_MATRIX_E2EE:-}" = "true" ]; then
    export MATRIX_E2EE_ENABLED=true
else
    export MATRIX_E2EE_ENABLED=false
fi

OUTPUT_DIR="/root/hiclaw-fs/agents/${WORKER_NAME}"
mkdir -p "${OUTPUT_DIR}"

envsubst < /opt/hiclaw/agent/skills/worker-management/references/worker-openclaw.json.tmpl > "${OUTPUT_DIR}/openclaw.json"

# Inject custom model if not in the built-in list
if ! jq -e --arg model "${MODEL_NAME}" '.models.providers["hiclaw-gateway"].models | map(.id) | index($model)' "${OUTPUT_DIR}/openclaw.json" > /dev/null 2>&1; then
    log "Custom model '${MODEL_NAME}' not in built-in list, injecting into worker config..."
    jq --arg model "${MODEL_NAME}" \
       --argjson ctx "${CTX}" \
       --argjson max "${MAX}" \
       --argjson reasoning "${MODEL_REASONING}" \
       --argjson input "${INPUT}" \
       '
        .models.providers["hiclaw-gateway"].models += [{"id": $model, "name": $model, "reasoning": $reasoning, "contextWindow": $ctx, "maxTokens": $max, "input": $input}]
        | .agents.defaults.models += {("hiclaw-gateway/" + $model): {"alias": $model}}
       ' "${OUTPUT_DIR}/openclaw.json" > "${OUTPUT_DIR}/openclaw.json.tmp" && \
        mv "${OUTPUT_DIR}/openclaw.json.tmp" "${OUTPUT_DIR}/openclaw.json"
fi

log "Generated ${OUTPUT_DIR}/openclaw.json (model=${MODEL_NAME}, ctx=${CTX}, max=${MAX})"

# ============================================================
# Optional: inject openclaw-cms-plugin observability config
#
# The HICLAW_CMS_TRACES_ENABLED / HICLAW_CMS_METRICS_ENABLED switches live on
# the Manager container.  When enabled, ALL Workers created by this Manager
# will receive matching plugin config in their openclaw.json (stored in MinIO)
# so that both Manager and Workers report traces/metrics to the same ARMS endpoint.
#
# Worker service name is automatically set to "hiclaw-worker-<WORKER_NAME>"
# for per-worker observability granularity in ARMS.
# ============================================================
_cms_traces_lc="$(echo "${HICLAW_CMS_TRACES_ENABLED:-false}" | tr '[:upper:]' '[:lower:]')"
if [ "${_cms_traces_lc}" = "true" ]; then
    _cms_plugin_dir="${OPENCLAW_CMS_PLUGIN_DIR:-/opt/openclaw/extensions/openclaw-cms-plugin}"
    _cms_manifest="${_cms_plugin_dir}/openclaw.plugin.json"

    if [ ! -f "${_cms_manifest}" ]; then
        log "WARNING: openclaw-cms-plugin manifest not found at ${_cms_manifest}; skipping CMS config for worker ${WORKER_NAME}. Plugin should be bundled in Worker image by default — verify image build completed successfully."
    elif [ -z "${HICLAW_CMS_ENDPOINT:-}" ] || [ -z "${HICLAW_CMS_LICENSE_KEY:-}" ] || [ -z "${HICLAW_CMS_WORKSPACE:-}" ]; then
        log "WARNING: HICLAW_CMS_TRACES_ENABLED=true but HICLAW_CMS_ENDPOINT / HICLAW_CMS_LICENSE_KEY / HICLAW_CMS_WORKSPACE are not all set; skipping CMS config for worker ${WORKER_NAME}"
    else
        _cms_worker_service="hiclaw-worker-${WORKER_NAME}"
        _cms_metrics_lc="$(echo "${HICLAW_CMS_METRICS_ENABLED:-false}" | tr '[:upper:]' '[:lower:]')"
        _diag_plugin_dir="/opt/openclaw/extensions/diagnostics-otel"
        _diag_available="0"
        if [ "${_cms_metrics_lc}" = "true" ] && [ -f "${_diag_plugin_dir}/package.json" ]; then
            _diag_available="1"
        fi

        jq --arg pluginName "openclaw-cms-plugin" \
           --arg pluginDir "${_cms_plugin_dir}" \
           --arg endpoint "${HICLAW_CMS_ENDPOINT}" \
           --arg licenseKey "${HICLAW_CMS_LICENSE_KEY}" \
           --arg armsProject "${HICLAW_CMS_PROJECT:-}" \
           --arg cmsWorkspace "${HICLAW_CMS_WORKSPACE}" \
           --arg serviceName "${_cms_worker_service}" \
           --arg diagPluginName "diagnostics-otel" \
           --arg diagPluginDir "${_diag_plugin_dir}" \
           --arg metricsRaw "${_cms_metrics_lc}" \
           --arg diagAvailableRaw "${_diag_available}" \
           '
            .plugins = (.plugins // {})
            | .plugins.load = (.plugins.load // {})
            | .plugins.entries = (.plugins.entries // {})
            | if (.plugins.allow | type) != "array" then .plugins.allow = [] else . end
            | if (.plugins.allow | index($pluginName)) == null then .plugins.allow += [$pluginName] else . end
            | if (.plugins.load.paths | type) != "array" then .plugins.load.paths = [] else . end
            | if (.plugins.load.paths | index($pluginDir)) == null then .plugins.load.paths += [$pluginDir] else . end
            | .plugins.entries[$pluginName] = {
                "enabled": true,
                "config": {
                    "endpoint": $endpoint,
                    "headers": {
                        "x-arms-license-key": $licenseKey,
                        "x-arms-project": $armsProject,
                        "x-cms-workspace": $cmsWorkspace
                    },
                    "serviceName": $serviceName
                }
            }

            # diagnostics-otel metrics (optional, only when plugin is available in image)
            | ($metricsRaw | ascii_downcase) as $m
            | ($diagAvailableRaw == "1") as $diagAvailable
            | (($m == "true") and $diagAvailable) as $metricsEnabled
            | if $metricsEnabled then
                (if (.plugins.allow | index($diagPluginName)) == null then .plugins.allow += [$diagPluginName] else . end)
                | (if (.plugins.load.paths | index($diagPluginDir)) == null then .plugins.load.paths += [$diagPluginDir] else . end)
                | .plugins.entries[$diagPluginName].enabled = true
                | .diagnostics = (.diagnostics // {})
                | .diagnostics.otel = (.diagnostics.otel // {})
                | .diagnostics.enabled = true
                | .diagnostics.otel.enabled = true
                | .diagnostics.otel.endpoint = $endpoint
                | .diagnostics.otel.protocol = (.diagnostics.otel.protocol // "http/protobuf")
                | .diagnostics.otel.headers = {
                    "x-arms-license-key": $licenseKey,
                    "x-arms-project": $armsProject,
                    "x-cms-workspace": $cmsWorkspace
                }
                | .diagnostics.otel.serviceName = $serviceName
                | .diagnostics.otel.metrics = true
                | .diagnostics.otel.traces = (.diagnostics.otel.traces // false)
                | .diagnostics.otel.logs = (.diagnostics.otel.logs // false)
              else
                .
              end
           ' "${OUTPUT_DIR}/openclaw.json" > "${OUTPUT_DIR}/openclaw.json.cms-tmp" && \
            mv "${OUTPUT_DIR}/openclaw.json.cms-tmp" "${OUTPUT_DIR}/openclaw.json"
        log "CMS plugin config injected into Worker ${WORKER_NAME} openclaw.json (service=${_cms_worker_service}, metrics=${_cms_metrics_lc})"
    fi
fi

# If this worker belongs to a team, override groupAllowFrom and dm.allowFrom
# to use [Leader, Admin] instead of [Manager, Admin]
if [ -n "${TEAM_LEADER_NAME}" ]; then
    LEADER_MATRIX_ID="@${TEAM_LEADER_NAME}:${MATRIX_DOMAIN_FOR_ID}"
    ADMIN_MATRIX_ID="@${ADMIN_USER}:${MATRIX_DOMAIN_FOR_ID}"
    jq --arg leader "${LEADER_MATRIX_ID}" \
       --arg admin "${ADMIN_MATRIX_ID}" \
       '.channels.matrix.groupAllowFrom = [$leader, $admin]
        | .channels.matrix.dm.allowFrom = [$leader, $admin]' \
       "${OUTPUT_DIR}/openclaw.json" > "${OUTPUT_DIR}/openclaw.json.tmp"
    mv "${OUTPUT_DIR}/openclaw.json.tmp" "${OUTPUT_DIR}/openclaw.json"
    log "  Overrode groupAllowFrom/dm.allowFrom for team worker (leader=${TEAM_LEADER_NAME})"
fi
