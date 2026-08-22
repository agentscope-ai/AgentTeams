#!/bin/bash
# aliyun-sae.sh - Alibaba Cloud SAE provider for HiClaw worker management
#
# Sourced by container-api.sh when the file exists.
# All SAE operations are delegated to aliyun-api.py.
#
# Prerequisites:
#   - HICLAW_SAE_WORKER_IMAGE env var set (signals cloud SAE mode)
#   - /opt/hiclaw/scripts/lib/cloud/aliyun-api.py available
#   - RRSA OIDC configured on the SAE application

CLOUD_WORKER_API="/opt/hiclaw/scripts/lib/cloud/aliyun-api.py"

cloud_sae_available() {
    [ -n "${HICLAW_SAE_WORKER_IMAGE:-}" ] && [ -f "${CLOUD_WORKER_API}" ]
}

# ── SAE Worker lifecycle ──────────────────────────────────────────────────────

sae_create_worker() {
    local worker_name="$1"
    local extra_envs_json="$2"
    local image_override="${3:-}"
    extra_envs_json="${extra_envs_json:-"{}"}"
    _log "Creating SAE application for worker: ${worker_name}"
    local envs_file
    envs_file=$(mktemp /tmp/sae-envs-XXXXXX.json)
    printf '%s' "${extra_envs_json}" > "${envs_file}"
    local image_arg=""
    if [ -n "${image_override}" ]; then
        image_arg="--image ${image_override}"
    fi
    python3 "${CLOUD_WORKER_API}" sae-create --name "${worker_name}" --envs "@${envs_file}" ${image_arg}
    local rc=$?
    rm -f "${envs_file}"
    return ${rc}
}

sae_delete_worker() {
    local worker_name="$1"
    _log "Deleting SAE application for worker: ${worker_name}"
    python3 "${CLOUD_WORKER_API}" sae-delete --name "${worker_name}"
}

sae_stop_worker() {
    local worker_name="$1"
    _log "Stopping SAE application for worker: ${worker_name}"
    python3 "${CLOUD_WORKER_API}" sae-stop --name "${worker_name}"
}

sae_start_worker() {
    local worker_name="$1"
    _log "Starting SAE application for worker: ${worker_name}"
    python3 "${CLOUD_WORKER_API}" sae-start --name "${worker_name}"
}

sae_status_worker() {
    local worker_name="$1"
    local result
    result=$(python3 "${CLOUD_WORKER_API}" sae-status --name "${worker_name}" 2>/dev/null)
    echo "${result}" | jq -r '.status // "unknown"' 2>/dev/null
}

sae_list_workers() {
    python3 "${CLOUD_WORKER_API}" sae-list
}

# ── AI Gateway consumer operations ────────────────────────────────────────────

cloud_create_consumer() {
    local consumer_name="$1"
    python3 "${CLOUD_WORKER_API}" gw-create-consumer --name "${consumer_name}"
}

cloud_bind_consumer() {
    local consumer_id="$1"
    local api_id="$2"
    local env_id="$3"
    python3 "${CLOUD_WORKER_API}" gw-bind-consumer \
        --consumer-id "${consumer_id}" --api-id "${api_id}" --env-id "${env_id}"
}
