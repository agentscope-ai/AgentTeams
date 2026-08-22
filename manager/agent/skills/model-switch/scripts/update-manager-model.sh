#!/bin/bash
# update-manager-model.sh - Hot-update the Manager Agent's model
#
# Patches ~/manager-workspace/openclaw.json in-place.
# OpenClaw detects the file change (~300ms) and reloads config automatically.
#
# Usage:
#   update-manager-model.sh <MODEL_ID> [--context-window <SIZE>] [--no-reasoning]
#
# Example:
#   update-manager-model.sh claude-sonnet-4-6
#   update-manager-model.sh my-custom-model --context-window 300000
#   update-manager-model.sh deepseek-chat --no-reasoning

set -e
source /opt/hiclaw/scripts/lib/hiclaw-env.sh

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
# Strip provider prefix if caller passed "hiclaw-gateway/<model>" by mistake
MODEL_NAME="${MODEL_NAME#hiclaw-gateway/}"

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

CONFIG_FILE="${HOME}/manager-workspace/openclaw.json"
if [ ! -f "${CONFIG_FILE}" ]; then
    # Fallback: openclaw.json may live directly under HOME when HOME=manager-workspace
    CONFIG_FILE="${HOME}/openclaw.json"
fi
if [ ! -f "${CONFIG_FILE}" ]; then
    echo "ERROR: Manager openclaw.json not found (tried ${HOME}/manager-workspace/openclaw.json and ${HOME}/openclaw.json)"
    exit 1
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
    qwen3.5-plus)
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
    gpt-5.4|gpt-5.3-codex|gpt-5-mini|gpt-5-nano|claude-opus-4-6|claude-sonnet-4-6|claude-haiku-4-5|qwen3.5-plus|kimi-k2.5)
        INPUT='["text", "image"]' ;;
    *)
        INPUT='["text"]' ;;
esac

log "Updating Manager model: ${MODEL_NAME} (ctx=${CTX}, max=${MAX}, reasoning=${REASONING}, input=${INPUT})"

# ── Pre-flight: verify the model is reachable via AI Gateway ──────────────────
GATEWAY_URL="${HICLAW_AI_GATEWAY_SERVER}/v1/chat/completions"
GATEWAY_KEY="${HICLAW_MANAGER_GATEWAY_KEY:-}"
if [ -z "${GATEWAY_KEY}" ] && [ -f "/data/hiclaw-secrets.env" ]; then
    source /data/hiclaw-secrets.env
    GATEWAY_KEY="${HICLAW_MANAGER_GATEWAY_KEY:-}"
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
    if [ "${HICLAW_RUNTIME:-}" = "aliyun" ]; then
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

# Check if the model already exists in the models array
MODEL_EXISTS=$(jq --arg model "${MODEL_NAME}" \
    '[.models.providers["hiclaw-gateway"].models[] | select(.id == $model)] | length' \
    "${CONFIG_FILE}" 2>/dev/null)

TMP=$(mktemp)
if [ "${MODEL_EXISTS}" -gt 0 ]; then
    # Known model: switch the primary model pointer and update reasoning
    jq --arg model "${MODEL_NAME}" \
       --argjson reasoning "${REASONING}" \
       '(.models.providers["hiclaw-gateway"].models[] | select(.id == $model)).reasoning = $reasoning
        | .agents.defaults.model.primary = ("hiclaw-gateway/" + $model)
        | .agents.defaults.models["hiclaw-gateway/" + $model] = { "alias": $model }' \
       "${CONFIG_FILE}" > "${TMP}" && mv "${TMP}" "${CONFIG_FILE}"

    log "Done. Model is now: ${MODEL_NAME}"
else
    # New model: add to models array and switch primary
    jq --arg model "${MODEL_NAME}" \
       --argjson ctx "${CTX}" \
       --argjson max "${MAX}" \
       --argjson reasoning "${REASONING}" \
       --argjson input "${INPUT}" \
       '.models.providers["hiclaw-gateway"].models += [{
           "id": $model,
           "name": $model,
           "reasoning": $reasoning,
           "contextWindow": $ctx,
           "maxTokens": $max,
           "input": $input
         }]
        | .agents.defaults.model.primary = ("hiclaw-gateway/" + $model)
        | .agents.defaults.models["hiclaw-gateway/" + $model] = { "alias": $model }' \
       "${CONFIG_FILE}" > "${TMP}" && mv "${TMP}" "${CONFIG_FILE}"

    log "Done. Model '${MODEL_NAME}' has been added to the models list."
fi

echo ""
echo "RESTART_REQUIRED: Run 'openclaw gateway restart' to apply the model switch."
