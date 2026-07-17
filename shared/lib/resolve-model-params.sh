#!/bin/bash
# resolve-model-params.sh - Resolve model metadata from known-models.json
#
# Single source of truth for context window, max tokens, reasoning, and input
# modalities. Consumers should read manager/configs/known-models.json only
# through this helper.
#
# Usage (source and call):
#   source /opt/hiclaw/scripts/lib/resolve-model-params.sh
#   resolve_model_params claude-sonnet-4-6
#   echo "${MODEL_CONTEXT_WINDOW} ${MODEL_MAX_TOKENS} ${MODEL_REASONING} ${MODEL_INPUT}"
#
# Sets (when a model entry is found or defaults apply):
#   MODEL_CONTEXT_WINDOW, MODEL_MAX_TOKENS, MODEL_REASONING, MODEL_INPUT

resolve_model_params() {
    local model_id="${1:-}"
    local known_models_file="${KNOWN_MODELS_FILE:-/opt/hiclaw/configs/known-models.json}"

    model_id="${model_id#agentteams-gateway/}"

    if [ -z "${model_id}" ]; then
        echo "resolve_model_params: model id is required" >&2
        return 1
    fi

    if [ ! -f "${known_models_file}" ]; then
        MODEL_CONTEXT_WINDOW=150000
        MODEL_MAX_TOKENS=128000
        MODEL_REASONING=true
        MODEL_INPUT='["text"]'
        return 0
    fi

    local entry
    entry="$(jq -c --arg id "${model_id}" '[.[] | select(.id == $id)][0] // empty' "${known_models_file}" 2>/dev/null || true)"

    if [ -n "${entry}" ] && [ "${entry}" != "null" ]; then
        MODEL_CONTEXT_WINDOW="$(printf '%s' "${entry}" | jq -r '.contextWindow')"
        MODEL_MAX_TOKENS="$(printf '%s' "${entry}" | jq -r '.maxTokens')"
        MODEL_REASONING="$(printf '%s' "${entry}" | jq -r '.reasoning // true')"
        MODEL_INPUT="$(printf '%s' "${entry}" | jq -c '.input // ["text"]')"
        return 0
    fi

    MODEL_CONTEXT_WINDOW=150000
    MODEL_MAX_TOKENS=128000
    MODEL_REASONING=true
    MODEL_INPUT='["text"]'
}
