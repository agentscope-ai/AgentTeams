#!/bin/bash
# update-manager-model.sh - Hot-update the Manager Agent's model
#
# Patches config files in-place based on runtime:
#   - OpenClaw: ~/manager-workspace/openclaw.json (model list + context window in one file)
#   - CoPaw: ~/.copaw.secret/providers.json (model list) + ~/.copaw/config.json (context window)
#
# OpenClaw detects the file change (~300ms) and reloads config automatically.
# CoPaw may require a restart.
#
# Usage:
#   update-manager-model.sh <MODEL_ID> [--context-window <SIZE>] [--no-reasoning]
#
# Example:
#   update-manager-model.sh claude-sonnet-4-6
#   update-manager-model.sh my-custom-model --context-window 300000
#   update-manager-model.sh deepseek-chat --no-reasoning

set -e
source /opt/agentteams/scripts/lib/agentteams-env.sh

# Detect runtime
MANAGER_RUNTIME="${AGENTTEAMS_MANAGER_RUNTIME:-openclaw}"

_get_max_tokens_param() {
    local model="$1"
    if [[ "${model}" =~ ^gpt-5(\.|-|[0-9]|$) ]]; then
        echo "max_completion_tokens"
    else
        echo "max_tokens"
    fi
}

MODEL_NAME="${1:-}"
if [ -z "${MODEL_NAME}" ]; then
    echo "Usage: $0 <MODEL_ID> [--context-window <SIZE>] [--no-reasoning]"
    echo "Example: $0 claude-sonnet-4-6"
    echo "         $0 my-custom-model --context-window 300000"
    echo "         $0 deepseek-chat --no-reasoning"
    exit 1
fi
shift
# Strip provider prefix if caller passed "agentteams-gateway/<model>" by mistake
MODEL_NAME="${MODEL_NAME#agentteams-gateway/}"

CTX_OVERRIDE=""
REASONING="true"
while [[ $# -gt 0 ]]; do
    case "$1" in
        --context-window)
            CTX_OVERRIDE="$2"
            shift 2
            ;;
        --no-reasoning)
            REASONING="false"
            shift
            ;;
        *)
            echo "Unknown argument: $1" >&2
            exit 1
            ;;
    esac
done

# Determine config files based on runtime
if [ "${MANAGER_RUNTIME}" = "copaw" ]; then
    CONFIG_FILE="${HOME}/.copaw/config.json"
    PROVIDERS_FILE="${HOME}/.copaw.secret/providers.json"
    if [ ! -f "${CONFIG_FILE}" ] || [ ! -f "${PROVIDERS_FILE}" ]; then
        echo "ERROR: CoPaw config not found"
        echo "  config.json: ${CONFIG_FILE} ($([ -f "${CONFIG_FILE}" ] && echo 'exists' || echo 'MISSING'))"
        echo "  providers.json: ${PROVIDERS_FILE} ($([ -f "${PROVIDERS_FILE}" ] && echo 'exists' || echo 'MISSING'))"
        exit 1
    fi
else
    CONFIG_FILE="${HOME}/manager-workspace/openclaw.json"
    if [ ! -f "${CONFIG_FILE}" ]; then
        CONFIG_FILE="${HOME}/openclaw.json"
    fi
    if [ ! -f "${CONFIG_FILE}" ]; then
        echo "ERROR: OpenClaw config not found (checked ~/manager-workspace/openclaw.json and ~/openclaw.json)"
        exit 1
    fi
fi

# Resolve context window and max tokens
case "${MODEL_NAME}" in
    gpt-5.4)
        CTX=1050000; MAX=128000 ;;
    gpt-5.3-codex|gpt-5-mini|gpt-5-nano)
        CTX=400000; MAX=128000 ;;
    claude-opus-4-6)
        CTX=1000000; MAX=128000 ;;
    claude-sonnet-4-6)
        CTX=1000000; MAX=64000 ;;
    claude-haiku-4-5)
        CTX=200000; MAX=64000 ;;
    qwen3.6-plus|qwen3.5-plus)
        CTX=200000; MAX=64000 ;;
    deepseek-chat|deepseek-reasoner|kimi-k2.5)
        CTX=256000; MAX=128000 ;;
    glm-5|MiniMax-M2.7|MiniMax-M2.7-highspeed|MiniMax-M2.5)
        CTX=200000; MAX=128000 ;;
    *)
        CTX=150000; MAX=128000 ;;
esac

# Allow explicit context-window override (for unknown models)
if [ -n "${CTX_OVERRIDE:-}" ]; then
    CTX="${CTX_OVERRIDE}"
fi

# Resolve input modalities: only vision-capable models get "image"
case "${MODEL_NAME}" in
    gpt-5.4|gpt-5.3-codex|gpt-5-mini|gpt-5-nano|claude-opus-4-6|claude-sonnet-4-6|claude-haiku-4-5|qwen3.6-plus|qwen3.5-plus|kimi-k2.5)
        INPUT='["text", "image"]' ;;
    *)
        INPUT='["text"]' ;;
esac

log "Updating Manager model: ${MODEL_NAME} (ctx=${CTX}, max=${MAX}, reasoning=${REASONING}, input=${INPUT})"

# ── Pre-flight: verify the model is reachable via AI Gateway ──────────────────
GATEWAY_URL="${AGENTTEAMS_AI_GATEWAY_URL}/v1/chat/completions"
GATEWAY_KEY="${AGENTTEAMS_MANAGER_GATEWAY_KEY:-}"
if [ -z "${GATEWAY_KEY}" ] && [ -f "/data/agentteams-secrets.env" ]; then
    source /data/agentteams-secrets.env
    GATEWAY_KEY="${AGENTTEAMS_MANAGER_GATEWAY_KEY:-}"
fi

log "Testing model reachability: ${GATEWAY_URL} (model=${MODEL_NAME})..."
MAX_TOKENS_PARAM=$(_get_max_tokens_param "${MODEL_NAME}")
HTTP_CODE=$(curl -s -o /tmp/model-test-resp.json -w '%{http_code}' \
    -X POST "${GATEWAY_URL}" \
    -H "Authorization: Bearer ${GATEWAY_KEY}" \
    -H "Content-Type: application/json" \
    -d "{\"model\":\"${MODEL_NAME}\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"${MAX_TOKENS_PARAM}\":1}" \
    --connect-timeout 10 --max-time 30 2>/dev/null) || HTTP_CODE="000"

if [ "${HTTP_CODE}" != "200" ]; then
    RESP_BODY=$(cat /tmp/model-test-resp.json 2>/dev/null | head -c 300 || true)
    echo "ERROR: MODEL_NOT_REACHABLE"
    echo "Model: ${MODEL_NAME}"
    echo "HTTP status: ${HTTP_CODE}"
    echo "Response: ${RESP_BODY}"
    echo ""
    echo "The model '${MODEL_NAME}' is not reachable via the AI Gateway."
    echo "This most likely means the current default AI Provider does not support this model."
    echo ""
    if [ "${AGENTTEAMS_RUNTIME:-}" = "aliyun" ]; then
        echo "To fix this, the human admin needs to check the Alibaba Cloud AI Gateway console"
        echo "to confirm the model route is configured for this model."
    else
        echo "To fix this, the human admin needs to open the Higress Console and:"
        echo "  1. Create a NEW AI Provider for the model vendor (e.g. 'kimi', 'deepseek', 'minimax')"
        echo "  2. Create a NEW AI Route that matches this model by name prefix"
        echo "     (e.g. for provider 'kimi', set model name predicate to match 'kimi-*')"
        echo "     so requests for models with that prefix are routed to the new provider,"
        echo "     while unmatched models still go through the default AI Route."
        echo ""
        echo "WARNING: Do NOT modify the default AI Provider — it is managed by the"
        echo "initialization config and will be overwritten on restart."
    fi
    exit 1
fi
log "Model test passed (HTTP 200)"
rm -f /tmp/model-test-resp.json
# ─────────────────────────────────────────────────────────────────────────────

TMP=$(mktemp)

if [ "${MANAGER_RUNTIME}" = "copaw" ]; then
    # ── CoPaw: update providers.json (model list) + config.json (context window) ──
    # providers.json: .custom_providers["agentteams-gateway"].models + .active_llm.model
    # config.json: .agents.running.max_input_length
    jq --arg model "${MODEL_NAME}" \
       '.custom_providers["agentteams-gateway"].models = [{"id": $model, "name": $model}]
        | .active_llm.model = $model' \
       "${PROVIDERS_FILE}" > "${TMP}" && mv "${TMP}" "${PROVIDERS_FILE}"
    cp "${PROVIDERS_FILE}" "${HOME}/.copaw/providers.json"

    jq --argjson ctx "${CTX}" \
       '.agents.running.max_input_length = $ctx' \
       "${CONFIG_FILE}" > "${TMP}" && mv "${TMP}" "${CONFIG_FILE}"

    log "Done. CoPaw model is now: ${MODEL_NAME} (ctx=${CTX})"
    echo ""
    echo "RESTART_REQUIRED: Restart the CoPaw service to apply the model switch."
else
    # ── OpenClaw: update openclaw.json ──
    MODEL_EXISTS=$(jq --arg model "${MODEL_NAME}" \
        '[.models.providers["agentteams-gateway"].models[] | select(.id == $model)] | length' \
        "${CONFIG_FILE}" 2>/dev/null || echo "0")

    if [ "${MODEL_EXISTS}" -gt 0 ]; then
        jq --arg model "${MODEL_NAME}" \
           --argjson reasoning "${REASONING}" \
           '(.models.providers["agentteams-gateway"].models[] | select(.id == $model)).reasoning = $reasoning
            | .agents.defaults.model.primary = ("agentteams-gateway/" + $model)
            | .agents.defaults.models["agentteams-gateway/" + $model] = { "alias": $model }' \
           "${CONFIG_FILE}" > "${TMP}" && mv "${TMP}" "${CONFIG_FILE}"
        log "Done. Model is now: ${MODEL_NAME}"
    else
        jq --arg model "${MODEL_NAME}" \
           --argjson ctx "${CTX}" \
           --argjson max "${MAX}" \
           --argjson reasoning "${REASONING}" \
           --argjson input "${INPUT}" \
           '.models.providers["agentteams-gateway"].models += [{
               "id": $model,
               "name": $model,
               "reasoning": $reasoning,
               "contextWindow": $ctx,
               "maxTokens": $max,
               "input": $input
             }]
            | .agents.defaults.model.primary = ("agentteams-gateway/" + $model)
            | .agents.defaults.models["agentteams-gateway/" + $model] = { "alias": $model }' \
           "${CONFIG_FILE}" > "${TMP}" && mv "${TMP}" "${CONFIG_FILE}"
        log "Done. Model '${MODEL_NAME}' has been added to the models list."
    fi
    echo ""
    echo "RESTART_REQUIRED: Run 'openclaw gateway restart' to apply the model switch."
fi

rm -f "${TMP}"
