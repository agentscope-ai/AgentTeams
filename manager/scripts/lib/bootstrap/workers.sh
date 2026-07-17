#!/bin/bash
# bootstrap/workers.sh - Worker config upgrade, recreate, notify

SEND_MANAGER_MESSAGE="/opt/hiclaw/scripts/lib/send-manager-message.sh"

bootstrap_manage_workers() {
# Upgrade Worker openclaw.json: merge known models + E2EE flag into existing configs
# Existing workers in MinIO may have old single-model configs or missing encryption field.
# Merge template models so they can hot-switch without restart.
# ============================================================
REGISTRY_FILE="/root/manager-workspace/workers-registry.json"
if [ -f "${REGISTRY_FILE}" ]; then
    # Use known-models.json (valid JSON) instead of template (contains ${VAR} placeholders)
    KNOWN_MODELS_FILE="/opt/hiclaw/configs/known-models.json"
    if [ -f "${KNOWN_MODELS_FILE}" ]; then
        _KNOWN_MODELS=$(cat "${KNOWN_MODELS_FILE}")
        for _wname in $(jq -r '.workers | keys[]' "${REGISTRY_FILE}" 2>/dev/null); do
            [ -z "${_wname}" ] && continue
            _minio_path="${AGENTTEAMS_STORAGE_PREFIX}/agents/${_wname}/openclaw.json"
            _tmp_in="/tmp/openclaw-${_wname}-models-upgrade-in.json"
            if mc cp "${_minio_path}" "${_tmp_in}" 2>/dev/null; then
                _tmp_out="/tmp/openclaw-${_wname}-models-upgrade-out.json"
                # Idempotent merge: add missing known models, rebuild aliases, set e2ee.
                # Always runs — jq deduplicates by model id, so re-runs are safe.
                jq --argjson known_models "${_KNOWN_MODELS}" \
                   --argjson e2ee "${MATRIX_E2EE_ENABLED}" '
                    .models.providers["agentteams-gateway"].models as $existing
                    | ($existing | map(.id)) as $existing_ids
                    | ($known_models | map(select(.id as $id | $existing_ids | index($id) | not))) as $new
                    | .models.providers["agentteams-gateway"].models = ($existing + $new)
                    | (.models.providers["agentteams-gateway"].models | map({ ("agentteams-gateway/" + .id): { "alias": .id } }) | add // {}) as $aliases
                    | .agents.defaults.models = ((.agents.defaults.models // {}) + $aliases)
                    | .channels.matrix.encryption = $e2ee
                    | .channels.matrix.autoJoin = "always"
                    | .tools = (.tools // {})
                    | .tools.exec = ((.tools.exec // {}) + {"host":"gateway","security":"full","ask":"off"})
                    | .tools.elevated = (.tools.elevated // {})
                    | .tools.elevated.enabled = true
                    | .tools.elevated.allowFrom |= ((. // {}) | .matrix = ["*"])
                    | .agents.defaults.elevatedDefault = "full"
                ' "${_tmp_in}" > "${_tmp_out}" 2>/dev/null
                if ! diff -q "${_tmp_in}" "${_tmp_out}" > /dev/null 2>&1; then
                    if mc cp "${_tmp_out}" "${_minio_path}" 2>/dev/null; then
                        _new_count=$(jq '.models.providers["agentteams-gateway"].models | length' "${_tmp_out}" 2>/dev/null)
                        log "Worker ${_wname}: upgraded openclaw.json (models: ${_new_count}, e2ee: ${MATRIX_E2EE_ENABLED})"
                    fi
                fi
                rm -f "${_tmp_in}" "${_tmp_out}"
            fi
        done
    fi
fi

# ============================================================
# Ensure Worker Matrix password files exist in MinIO (E2EE fix)
# Workers need to re-login on restart to get a fresh device_id.
# Older workers created before this fix won't have the password file.
# ============================================================
if [ -f "${REGISTRY_FILE}" ]; then
    for _wname in $(jq -r '.workers | keys[]' "${REGISTRY_FILE}" 2>/dev/null); do
        [ -z "${_wname}" ] && continue
        _creds_file="/data/worker-creds/${_wname}.env"
        if [ -f "${_creds_file}" ]; then
            # Check if password file already exists in MinIO
            if ! mc stat "${AGENTTEAMS_STORAGE_PREFIX}/agents/${_wname}/credentials/matrix/password" > /dev/null 2>&1; then
                source "${_creds_file}"
                if [ -n "${WORKER_PASSWORD}" ]; then
                    _tmp_pw="/tmp/matrix-pw-${_wname}"
                    echo -n "${WORKER_PASSWORD}" > "${_tmp_pw}"
                    mc cp "${_tmp_pw}" "${AGENTTEAMS_STORAGE_PREFIX}/agents/${_wname}/credentials/matrix/password" 2>/dev/null \
                        && log "Worker ${_wname}: wrote Matrix password to MinIO (E2EE re-login fix)" \
                        || log "Worker ${_wname}: WARNING: failed to write Matrix password to MinIO"
                    rm -f "${_tmp_pw}"
                fi
            fi
        fi
    done
fi

# ============================================================
# Recreate Worker containers as needed after Manager restart.
# Workers are on agentteams-net; Docker DNS resolves *-local.agentteams.io via
# the Manager's network aliases, so IP changes don't require worker recreation.
# Only recreate stopped/missing workers.
# ============================================================
if container_api_available; then
    REGISTRY_FILE="/root/manager-workspace/workers-registry.json"
    if [ -f "${REGISTRY_FILE}" ]; then
        for _worker_name in $(jq -r '.workers | keys[]' "${REGISTRY_FILE}" 2>/dev/null); do
            [ -z "${_worker_name}" ] && continue

            # Skip remote workers — they are not Manager-managed containers.
            _deployment=$(jq -r --arg w "${_worker_name}" '.workers[$w].deployment // "local"' "${REGISTRY_FILE}" 2>/dev/null)
            if [ "${_deployment}" = "remote" ]; then
                log "Worker ${_worker_name} is remote, skipping container recreate"
                continue
            fi

            _status=$(container_status_worker "${_worker_name}")
            if [ "${_status}" = "running" ]; then
                log "Worker running: ${_worker_name}, skipping"
                continue
            fi
            # Container missing or stopped — recreate.
            log "Worker container ${_status}: ${_worker_name}, recreating..."
            _creds_file="/data/worker-creds/${_worker_name}.env"
            if [ -f "${_creds_file}" ]; then
                source "${_creds_file}"
                _runtime=$(jq -r --arg w "${_worker_name}" '.workers[$w].runtime // "openclaw"' "${REGISTRY_FILE}" 2>/dev/null)
                _recreated=false
                for _attempt in 1 2 3; do
                    local _env_map _create_body
                    _env_map=$(jq -cn \
                        --arg name "${_worker_name}" \
                        --arg fak "${_worker_name}" \
                        --arg fsk "${WORKER_MINIO_PASSWORD:-}" \
                        --arg fs_domain "${AGENTTEAMS_FS_DOMAIN:-fs-local.agentteams.io}" \
                        --arg controller_url "${AGENTTEAMS_CONTROLLER_URL:-}" \
                        '{
                            "AGENTTEAMS_WORKER_NAME": $name,
                            "AGENTTEAMS_FS_ENDPOINT": ("http://" + ($fs_domain | split(":")[0]) + ":9000"),
                            "AGENTTEAMS_FS_ACCESS_KEY": $fak,
                            "AGENTTEAMS_FS_SECRET_KEY": $fsk
                        }
                        | if $controller_url != "" then . + {"AGENTTEAMS_CONTROLLER_URL": $controller_url} else . end')
                    _create_body=$(jq -cn --arg name "${_worker_name}" --arg runtime "${_runtime}" --argjson env "${_env_map}" '{name: $name, runtime: $runtime, env: $env}')
                    worker_backend_create "${_create_body}" > /dev/null 2>&1 && _recreated=true && break
                    log "  Attempt ${_attempt}/3 failed for ${_worker_name}, retrying in $((5 * _attempt))s..."
                    sleep $((5 * _attempt))
                done
                if [ "${_recreated}" = true ]; then
                    log "  Recreated ${_runtime} worker: ${_worker_name}"
                else
                    log "  ERROR: Failed to recreate ${_worker_name} after 3 attempts"
                fi
            else
                log "  WARNING: No credentials found for ${_worker_name} (${_creds_file} missing), skipping"
            fi
        done
    fi
fi

# ============================================================
# Notify workers of builtin updates if upgrade happened
# Builtin files (AGENTS.md, skills) are already synced by upgrade-builtins.sh
#
# Cooldown: skip notification if the last successful notify was within
# NOTIFY_COOLDOWN_SECS (default 3600s / 1 hour). This prevents repeated
# notifications when the Manager crash-loops and re-runs upgrade-builtins
# on every restart (e.g. IMAGE_VERSION=latest always triggers upgrade).
# ============================================================
NOTIFY_COOLDOWN_SECS="${AGENTTEAMS_NOTIFY_COOLDOWN_SECS:-3600}"
NOTIFY_TS_FILE="/root/manager-workspace/.last-worker-notify-ts"

if [ -f /root/manager-workspace/.upgrade-pending-worker-notify ]; then
    _now=$(date +%s)
    _last_notify=$(cat "${NOTIFY_TS_FILE}" 2>/dev/null || echo "0")
    _elapsed=$(( _now - _last_notify ))

    if [ "${_elapsed}" -lt "${NOTIFY_COOLDOWN_SECS}" ]; then
        log "Skipping worker builtin notification (last notify ${_elapsed}s ago, cooldown ${NOTIFY_COOLDOWN_SECS}s)"
        rm -f /root/manager-workspace/.upgrade-pending-worker-notify
    else
        log "Notifying workers about builtin updates..."
        REGISTRY_FILE="/root/manager-workspace/workers-registry.json"
        _notify_ok=false
        if [ -f "${REGISTRY_FILE}" ]; then
            for _worker_name in $(jq -r '.workers | keys[]' "${REGISTRY_FILE}" 2>/dev/null); do
                [ -z "${_worker_name}" ] && continue
                _room_id=$(jq -r --arg w "${_worker_name}" '.workers[$w].room_id // empty' "${REGISTRY_FILE}" 2>/dev/null)
                if [ -n "${_room_id}" ]; then
                    _worker_id="@${_worker_name}:${MATRIX_DOMAIN}"
                    _msg="@${_worker_name}:${MATRIX_DOMAIN} Manager upgraded builtin files (AGENTS.md, skills). Please use your file-sync skill to sync the latest config."
                    _notify_args=(
                        bash "${SEND_MANAGER_MESSAGE}"
                        --room "${_room_id}"
                        --text "${_msg}"
                        --mention-user "${_worker_id}"
                    )
                    if [ "${AGENTTEAMS_MANAGER_RUNTIME:-openclaw}" = "copaw" ]; then
                        _notify_args+=(--wait-runtime)
                    else
                        _notify_args+=(--token "${MANAGER_TOKEN}")
                    fi
                    if "${_notify_args[@]}" > /dev/null 2>&1; then
                        log "  Notified ${_worker_name}"; _notify_ok=true
                    else
                        log "  WARNING: Failed to notify ${_worker_name} via send-manager-message.sh"
                    fi
                fi
            done
        fi
        # Record timestamp only if at least one notification succeeded
        if [ "${_notify_ok}" = true ]; then
            echo "${_now}" > "${NOTIFY_TS_FILE}"
        fi
        rm -f /root/manager-workspace/.upgrade-pending-worker-notify
    fi
fi

}
