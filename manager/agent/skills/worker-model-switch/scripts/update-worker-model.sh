#!/bin/bash
# update-worker-model.sh - Update a Worker Agent's LLM model
#
# Patches the Worker's openclaw.json in MinIO, updates workers-registry.json,
# and notifies the Worker via Matrix to reload config.
#
# Usage:
#   update-worker-model.sh --worker <name> --model <model-id> [--context-window <size>] [--no-reasoning]
#
# Example:
#   update-worker-model.sh --worker alice --model claude-sonnet-4-6
#   update-worker-model.sh --worker alice --model my-custom-model --context-window 300000
#   update-worker-model.sh --worker alice --model deepseek-chat --no-reasoning

set -euo pipefail
source /opt/hiclaw/scripts/lib/hiclaw-env.sh

REGISTRY_FILE="${HOME}/workers-registry.json"

_ts() {
    date -u '+%Y-%m-%dT%H:%M:%SZ'
}

_log() {
    echo "[worker-model-switch $(date '+%Y-%m-%d %H:%M:%S')] $1"
}

# Resolve context window, max tokens, and input modalities for a given model name
_resolve_model_params() {
    local model="$1"
    case "${model}" in
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
    case "${model}" in
        gpt-5.4|gpt-5.3-codex|gpt-5-mini|gpt-5-nano|claude-opus-4-6|claude-sonnet-4-6|claude-haiku-4-5|qwen3.5-plus|kimi-k2.5)
            INPUT='["text", "image"]' ;;
        *)
            INPUT='["text"]' ;;
    esac
}

# ─── Main logic ──────────────────────────────────────────────────────────────

update_worker_model() {
    local worker="$1"
    local new_model="$2"
    # Strip provider prefix if caller passed "hiclaw-gateway/<model>" by mistake
    new_model="${new_model#hiclaw-gateway/}"

    if [ -z "${worker}" ] || [ -z "${new_model}" ]; then
        _log "ERROR: --worker and --model are required"
        return 1
    fi

    if [ ! -f "$REGISTRY_FILE" ]; then
        _log "ERROR: $REGISTRY_FILE not found"
        return 1
    fi

    local exists
    exists=$(jq -r --arg w "$worker" '.workers | has($w)' "$REGISTRY_FILE" 2>/dev/null)
    if [ "$exists" != "true" ]; then
        _log "ERROR: Worker '$worker' not found in registry"
        return 1
    fi

    local CTX MAX INPUT
    _resolve_model_params "${new_model}"
    _log "Updating worker $worker model to ${new_model} (ctx=${CTX}, max=${MAX}, reasoning=${REASONING}, input=${INPUT})"

    # ── Pre-flight: verify the model is reachable via AI Gateway ─────────────
    local gateway_url="${HICLAW_AI_GATEWAY_SERVER}/v1/chat/completions"
    local gateway_key="${HICLAW_MANAGER_GATEWAY_KEY:-}"
    if [ -z "${gateway_key}" ] && [ -f "/data/hiclaw-secrets.env" ]; then
        source /data/hiclaw-secrets.env
        gateway_key="${HICLAW_MANAGER_GATEWAY_KEY:-}"
    fi
    _log "Testing model reachability: ${gateway_url} (model=${new_model})..."
    local http_code
    http_code=$(curl -s -o /tmp/model-test-resp-${worker}.json -w '%{http_code}' \
        -X POST "${gateway_url}" \
        -H "Authorization: Bearer ${gateway_key}" \
        -H "Content-Type: application/json" \
        -d "{\"model\":\"${new_model}\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}" \
        --connect-timeout 10 --max-time 30 2>/dev/null) || http_code="000"
    if [ "${http_code}" != "200" ]; then
        local resp_body
        resp_body=$(cat /tmp/model-test-resp-${worker}.json 2>/dev/null | head -c 300 || true)
        rm -f /tmp/model-test-resp-${worker}.json
        _log "ERROR: MODEL_NOT_REACHABLE"
        _log "Model: ${new_model}"
        _log "HTTP status: ${http_code}"
        _log "Response: ${resp_body}"
        _log ""
        _log "The model '${new_model}' is not reachable via the AI Gateway."
        _log "This most likely means the current default AI Provider does not support this model."
        _log ""
        if [ "${HICLAW_RUNTIME:-}" = "aliyun" ]; then
            _log "To fix this, the human admin needs to check the Alibaba Cloud AI Gateway console"
            _log "to confirm the model route is configured for this model."
        else
            _log "To fix this, the human admin needs to open the Higress Console and:"
            _log "  1. Create a NEW AI Provider for the model vendor (e.g. 'kimi', 'deepseek', 'minimax')"
            _log "  2. Create a NEW AI Route that matches this model by name prefix"
            _log "     (e.g. for provider 'kimi', set model name predicate to match 'kimi-*')"
            _log "     so requests for models with that prefix are routed to the new provider,"
            _log "     while unmatched models still go through the default AI Route."
            _log ""
            _log "WARNING: Do NOT modify the default AI Provider — it is managed by the"
            _log "initialization config and will be overwritten on restart."
        fi
        return 1
    fi
    rm -f /tmp/model-test-resp-${worker}.json
    _log "Model test passed (HTTP 200)"
    # ─────────────────────────────────────────────────────────────────────────

    # Pull openclaw.json from MinIO
    local minio_path="${HICLAW_STORAGE_PREFIX}/agents/${worker}/openclaw.json"
    local tmp_in="/tmp/openclaw-${worker}-model-update-in.json"
    local tmp_out="/tmp/openclaw-${worker}-model-update-out.json"

    if ! mc cp "${minio_path}" "${tmp_in}" 2>/dev/null; then
        _log "ERROR: Could not pull openclaw.json for ${worker} from MinIO"
        return 1
    fi

    # Check if the model already exists in the models array
    local model_exists
    model_exists=$(jq --arg model "${new_model}" \
        '[.models.providers["hiclaw-gateway"].models[] | select(.id == $model)] | length' \
        "${tmp_in}" 2>/dev/null)

    local restart_required=true
    if [ "${model_exists}" -gt 0 ]; then
        # Known model: switch the primary model pointer and update reasoning
        jq --arg model "${new_model}" \
           --argjson reasoning "${REASONING}" \
           '(.models.providers["hiclaw-gateway"].models[] | select(.id == $model)).reasoning = $reasoning
            | .agents.defaults.model.primary = ("hiclaw-gateway/" + $model)
            | .agents.defaults.models["hiclaw-gateway/" + $model] = { "alias": $model }' \
           "${tmp_in}" > "${tmp_out}"
    else
        # New model: add to models array and switch primary
        jq --arg model "${new_model}" \
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
           "${tmp_in}" > "${tmp_out}"
    fi

    if ! mc cp "${tmp_out}" "${minio_path}" 2>/dev/null; then
        _log "ERROR: Failed to push updated openclaw.json for ${worker} to MinIO"
        rm -f "${tmp_in}" "${tmp_out}"
        return 1
    fi
    rm -f "${tmp_in}" "${tmp_out}"
    _log "openclaw.json updated in MinIO for ${worker}"

    # Update workers-registry.json with model field
    local tmp_reg
    tmp_reg=$(mktemp)
    jq --arg w "$worker" --arg m "${new_model}" --arg ts "$(_ts)" \
        '.workers[$w].model = $m | .updated_at = $ts' \
        "$REGISTRY_FILE" > "$tmp_reg" && mv "$tmp_reg" "$REGISTRY_FILE"
    _log "Registry updated: ${worker}.model = ${new_model}"

    # Notify worker to use file-sync skill
    local room_id
    room_id=$(jq -r --arg w "$worker" '.workers[$w].room_id // empty' "$REGISTRY_FILE" 2>/dev/null)
    local matrix_domain="${HICLAW_MATRIX_DOMAIN:-matrix-local.hiclaw.io:8080}"
    local manager_token="${MANAGER_MATRIX_TOKEN:-}"

    # Try to get token from secrets file if not in env
    if [ -z "${manager_token}" ] && [ -f "/data/hiclaw-secrets.env" ]; then
        source /data/hiclaw-secrets.env
        manager_token="${MANAGER_MATRIX_TOKEN:-}"
    fi

    if [ -n "${room_id}" ] && [ -n "${manager_token}" ]; then
        local txn_id
        txn_id=$(openssl rand -hex 8)
        local msg_body
        msg_body="@${worker}:${matrix_domain} Your model has been updated to \`${new_model}\` (reasoning=${REASONING}). Please use your file-sync skill to sync the latest config."
        curl -sf -X PUT \
            "${HICLAW_MATRIX_SERVER}/_matrix/client/v3/rooms/${room_id}/send/m.room.message/${txn_id}" \
            -H "Authorization: Bearer ${manager_token}" \
            -H 'Content-Type: application/json' \
            -d "{\"msgtype\":\"m.text\",\"body\":\"${msg_body}\",\"m.mentions\":{\"user_ids\":[\"@${worker}:${matrix_domain}\"]}}" \
            > /dev/null 2>&1 \
            && _log "Notified @${worker} to use file-sync skill" \
            || _log "WARNING: Failed to notify @${worker} (container may be stopped)"
    else
        _log "WARNING: Could not send Matrix notification (missing room_id or token)"
    fi

    _log "Model update complete for ${worker}: ${new_model} (ctx=${CTX}, max=${MAX}, reasoning=${REASONING}, input=${INPUT})"
    echo ""
    echo "RESTART_REQUIRED: Worker '${worker}' needs a restart for the model switch to '${new_model}' to take effect."
}

# ─── Argument parsing ─────────────────────────────────────────────────────────

WORKER=""
MODEL=""
CTX_OVERRIDE=""
REASONING="true"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --worker)
            WORKER="$2"
            shift 2
            ;;
        --model)
            MODEL="$2"
            shift 2
            ;;
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

if [ -z "$WORKER" ] || [ -z "$MODEL" ]; then
    echo "Usage: $0 --worker <name> --model <model-id> [--context-window <size>] [--no-reasoning]" >&2
    echo "Example: $0 --worker alice --model claude-sonnet-4-6" >&2
    echo "         $0 --worker alice --model my-custom-model --context-window 300000" >&2
    echo "         $0 --worker alice --model deepseek-chat --no-reasoning" >&2
    exit 1
fi

update_worker_model "$WORKER" "$MODEL"
