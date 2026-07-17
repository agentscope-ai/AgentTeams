#!/bin/bash
# bootstrap/openclaw-config.sh - Generate/update Manager openclaw.json

bootstrap_generate_openclaw_config() {
log "Generating Manager openclaw.json..."
export MANAGER_MATRIX_TOKEN="${MANAGER_TOKEN}"
export MANAGER_GATEWAY_KEY="${AGENTTEAMS_MANAGER_GATEWAY_KEY}"
# Resolve model parameters based on model name
MODEL_NAME="${AGENTTEAMS_DEFAULT_MODEL:-qwen3.6-plus}"
# TODO(H3.4): migrate to resolve-model-params.sh (see update-manager-model.sh)
case "${MODEL_NAME}" in
    gpt-5.3-codex|gpt-5-mini|gpt-5-nano)
        export MODEL_CONTEXT_WINDOW=400000 MODEL_MAX_TOKENS=128000 ;;
    claude-opus-4-6)
        export MODEL_CONTEXT_WINDOW=1000000 MODEL_MAX_TOKENS=128000 ;;
    claude-sonnet-4-6)
        export MODEL_CONTEXT_WINDOW=1000000 MODEL_MAX_TOKENS=64000 ;;
    claude-haiku-4-5)
        export MODEL_CONTEXT_WINDOW=200000 MODEL_MAX_TOKENS=64000 ;;
    qwen3.6-plus|qwen3.5-plus)
        export MODEL_CONTEXT_WINDOW=200000 MODEL_MAX_TOKENS=64000 ;;
    deepseek-chat|deepseek-reasoner|kimi-k2.5)
        export MODEL_CONTEXT_WINDOW=256000 MODEL_MAX_TOKENS=128000 ;;
    glm-5|MiniMax-M2.7|MiniMax-M2.7-highspeed|MiniMax-M2.5)
        export MODEL_CONTEXT_WINDOW=200000 MODEL_MAX_TOKENS=128000 ;;
    *)
        export MODEL_CONTEXT_WINDOW=150000 MODEL_MAX_TOKENS=128000 ;;
esac
export MODEL_REASONING=true

# Override with user-supplied custom model parameters from env (set during install)
[ -n "${AGENTTEAMS_MODEL_CONTEXT_WINDOW:-}" ] && export MODEL_CONTEXT_WINDOW="${AGENTTEAMS_MODEL_CONTEXT_WINDOW}"
[ -n "${AGENTTEAMS_MODEL_MAX_TOKENS:-}" ] && export MODEL_MAX_TOKENS="${AGENTTEAMS_MODEL_MAX_TOKENS}"
[ -n "${AGENTTEAMS_MODEL_REASONING:-}" ] && export MODEL_REASONING="${AGENTTEAMS_MODEL_REASONING}"

# E2EE: convert AGENTTEAMS_MATRIX_E2EE to JSON boolean for template substitution
if [ "${AGENTTEAMS_MATRIX_E2EE:-0}" = "1" ] || [ "${AGENTTEAMS_MATRIX_E2EE:-}" = "true" ]; then
    export MATRIX_E2EE_ENABLED=true
else
    export MATRIX_E2EE_ENABLED=false
fi
log "Matrix E2EE: ${MATRIX_E2EE_ENABLED}"

# Resolve input modalities: only vision-capable models get "image"
case "${MODEL_NAME}" in
    gpt-5.4|gpt-5.3-codex|gpt-5-mini|gpt-5-nano|claude-opus-4-6|claude-sonnet-4-6|claude-haiku-4-5|qwen3.6-plus|qwen3.5-plus|kimi-k2.5)
        export MODEL_INPUT='["text", "image"]' ;;
    *)
        export MODEL_INPUT='["text"]' ;;
esac
# Override with user-supplied vision setting from env
if [ "${AGENTTEAMS_MODEL_VISION:-}" = "true" ]; then
    export MODEL_INPUT='["text", "image"]'
elif [ "${AGENTTEAMS_MODEL_VISION:-}" = "false" ]; then
    export MODEL_INPUT='["text"]'
fi

log "Model: ${MODEL_NAME} (context=${MODEL_CONTEXT_WINDOW}, maxTokens=${MODEL_MAX_TOKENS}, reasoning=${MODEL_REASONING}, input=${MODEL_INPUT})"

if [ -f /root/manager-workspace/openclaw.json ]; then
    log "Manager openclaw.json already exists, updating dynamic fields only (preserving user customizations)..."
    # Merge known models into existing config (add missing, preserve user-added)
    # Use known-models.json (valid JSON) instead of template (contains ${VAR} placeholders)
    KNOWN_MODELS=$(cat /opt/hiclaw/configs/known-models.json 2>/dev/null || echo '[]')
    jq --arg token "${MANAGER_TOKEN}" \
       --arg key "${AGENTTEAMS_MANAGER_GATEWAY_KEY}" \
       --arg model "${MODEL_NAME}" \
       --arg emb_model "${AGENTTEAMS_EMBEDDING_MODEL}" \
       --arg aigw_domain "${AI_GATEWAY_DOMAIN}" \
       --arg matrix_user_id "@manager:${MATRIX_DOMAIN}" \
       --arg heartbeat_every "${AGENTTEAMS_MANAGER_HEARTBEAT_INTERVAL}" \
       --argjson e2ee "${MATRIX_E2EE_ENABLED}" \
       --argjson known_models "${KNOWN_MODELS}" \
       --argjson ctx "${MODEL_CONTEXT_WINDOW}" \
       --argjson max "${MODEL_MAX_TOKENS}" \
       --argjson reasoning "${MODEL_REASONING}" \
       --argjson input "${MODEL_INPUT}" \
       '
        # Merge known models: add any model id not already present
        .models.providers["agentteams-gateway"].models as $existing
        | ($existing | map(.id)) as $existing_ids
        | ($known_models | map(select(.id as $id | $existing_ids | index($id) | not))) as $new
        | .models.providers["agentteams-gateway"].models = ($existing + $new)
        # Ensure the user-chosen default model is in the list (custom model support)
        | if (.models.providers["agentteams-gateway"].models | map(.id) | index($model) | not) then
            .models.providers["agentteams-gateway"].models += [{"id": $model, "name": $model, "reasoning": $reasoning, "contextWindow": $ctx, "maxTokens": $max, "input": $input}]
          else . end
        # Rebuild model aliases from the full models list
        | (.models.providers["agentteams-gateway"].models | map({ ("agentteams-gateway/" + .id): { "alias": .id } }) | add // {}) as $aliases
        | .agents.defaults.models = ((.agents.defaults.models // {}) + $aliases)
        | .channels.matrix.accessToken = $token | .channels.matrix.userId = $matrix_user_id | .models.providers["agentteams-gateway"].apiKey = $key
        | ((.hooks.token // "") as $ht | if $ht == $key or $ht == ($key + "-hooks" | @base64) then del(.hooks) else . end)
        | .agents.defaults.model.primary = ("agentteams-gateway/" + $model)
        | .commands.restart = true
        | .gateway.port = 18799
        | .gateway.bind = "lan"
        | .gateway.controlUi = ((.gateway.controlUi // {}) + {"dangerouslyDisableDeviceAuth": true, "allowInsecureAuth": true, "allowedOrigins": ["*"]})
        | .channels.matrix.encryption = $e2ee
        | .channels.matrix.network = ((.channels.matrix.network // {}) + {"dangerouslyAllowPrivateNetwork": true})
        | .channels.matrix.autoJoin = "always"
        | .agents.defaults.heartbeat = ((.agents.defaults.heartbeat // {}) + {"every": $heartbeat_every})
        # OpenClaw YOLO defaults: host exec without approval prompts (see openclaw docs tools/exec-approvals)
        | .tools = (.tools // {})
        | .tools.exec = ((.tools.exec // {}) + {"host":"gateway","security":"full","ask":"off"})
        | .tools.elevated = (.tools.elevated // {})
        | .tools.elevated.enabled = true
        | .tools.elevated.allowFrom |= ((. // {}) | .matrix = ["*"])
        | .agents.defaults.elevatedDefault = "full"
        # Ensure memorySearch config exists (embedding model for memory) — skip if embedding model is empty
        | if $emb_model != "" then .agents.defaults.memorySearch //= {"provider":"openai","model":$emb_model,"remote":{"baseUrl":("http://" + $aigw_domain + ":8080/v1"),"apiKey":$key}} else . end
       ' \
       /root/manager-workspace/openclaw.json > /tmp/openclaw.json.tmp && \
        mv /tmp/openclaw.json.tmp /root/manager-workspace/openclaw.json
    # Disable openclaw's observe-recovery mechanism which compares config against
    # a lastKnownGood baseline in config-health.json. When meta is missing from the
    # current file but present in the baseline, observe-recovery restores from .bak,
    # undoing user customizations (plugins, channels, etc).
    # Clearing config-health.json removes the baseline so observe-recovery won't
    # interfere, while preserving .bak as a backup.
    if [ "${MANAGER_RUNTIME}" = "openclaw" ]; then
        rm -f /root/manager-workspace/.openclaw/logs/config-health.json
    fi
    # Verify the token was written correctly
    _written_token=$(jq -r '.channels.matrix.accessToken' /root/manager-workspace/openclaw.json 2>/dev/null)
    if [ -z "${_written_token}" ] || [ "${_written_token}" = "null" ]; then
        log "ERROR: Matrix token was not written correctly to openclaw.json (got: ${_written_token})"
    else
        log "Matrix token written to openclaw.json (prefix: ${_written_token:0:10}...)"
    fi
else
    log "Manager openclaw.json not found, generating from template..."
    envsubst < /opt/hiclaw/configs/manager-openclaw.json.tmpl > /root/manager-workspace/openclaw.json
    # Post-envsubst injection: memorySearch + custom model (single jq pass when possible)
    if ! jq -e --arg model "${MODEL_NAME}" '.models.providers["agentteams-gateway"].models | map(.id) | index($model)' /root/manager-workspace/openclaw.json > /dev/null 2>&1; then
        log "Custom model '${MODEL_NAME}' not in built-in list, injecting into config..."
        jq --arg emb_model "${AGENTTEAMS_EMBEDDING_MODEL}" \
           --arg aigw_domain "${AI_GATEWAY_DOMAIN}" \
           --arg key "${AGENTTEAMS_MANAGER_GATEWAY_KEY}" \
           --arg model "${MODEL_NAME}" \
           --arg heartbeat_every "${AGENTTEAMS_MANAGER_HEARTBEAT_INTERVAL}" \
           --argjson ctx "${MODEL_CONTEXT_WINDOW}" \
           --argjson max "${MODEL_MAX_TOKENS}" \
           --argjson reasoning "${MODEL_REASONING}" \
           --argjson input "${MODEL_INPUT}" \
           '
            (if $emb_model != "" then .agents.defaults.memorySearch = {"provider":"openai","model":$emb_model,"remote":{"baseUrl":("http://" + $aigw_domain + ":8080/v1"),"apiKey":$key}} else . end)
            | .models.providers["agentteams-gateway"].models += [{"id": $model, "name": $model, "reasoning": $reasoning, "contextWindow": $ctx, "maxTokens": $max, "input": $input}]
            | .agents.defaults.models += {("agentteams-gateway/" + $model): {"alias": $model}}
           ' /root/manager-workspace/openclaw.json > /tmp/openclaw.json.tmp && \
            mv /tmp/openclaw.json.tmp /root/manager-workspace/openclaw.json
    elif [ -n "${AGENTTEAMS_EMBEDDING_MODEL}" ]; then
        jq --arg emb_model "${AGENTTEAMS_EMBEDDING_MODEL}" \
           --arg aigw_domain "${AI_GATEWAY_DOMAIN}" \
           --arg key "${AGENTTEAMS_MANAGER_GATEWAY_KEY}" \
           '.agents.defaults.memorySearch = {"provider":"openai","model":$emb_model,"remote":{"baseUrl":("http://" + $aigw_domain + ":8080/v1"),"apiKey":$key}}' \
           /root/manager-workspace/openclaw.json > /tmp/openclaw.json.tmp && \
            mv /tmp/openclaw.json.tmp /root/manager-workspace/openclaw.json
    fi
    _written_token=$(jq -r '.channels.matrix.accessToken' /root/manager-workspace/openclaw.json 2>/dev/null)
    log "Matrix token written from template (prefix: ${_written_token:0:10}...)"
fi

# Cloud/K8s mode: overlay cloud-specific settings onto generated config
if is_cloud_runtime; then
    log "Applying cloud/k8s overlay to openclaw.json..."
    jq --arg homeserver "${AGENTTEAMS_MATRIX_URL}" \
       --arg gateway "${AGENTTEAMS_AI_GATEWAY_URL}/v1" \
       --arg key "${AGENTTEAMS_MANAGER_GATEWAY_KEY}" \
       '.channels.matrix.homeserver = $homeserver
        | .models.providers["agentteams-gateway"].baseUrl = $gateway
        | .models.providers["agentteams-gateway"].apiKey = $key
        | ((.hooks.token // "") as $ht | if $ht == $key or $ht == ($key + "-hooks" | @base64) then del(.hooks) else . end)
        | .commands.restart = false
        | if .agents.defaults.memorySearch then .agents.defaults.memorySearch.remote.baseUrl = $gateway | .agents.defaults.memorySearch.remote.apiKey = $key else . end' \
       /root/manager-workspace/openclaw.json > /tmp/openclaw-cloud.json && \
        mv /tmp/openclaw-cloud.json /root/manager-workspace/openclaw.json
    log "Cloud/K8s overlay applied"
fi

# ============================================================
}
