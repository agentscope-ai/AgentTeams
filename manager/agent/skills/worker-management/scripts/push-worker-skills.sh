#!/bin/bash
# push-worker-skills.sh - Push skills to Worker(s) via MinIO
#
# Manages Worker skill distribution from Manager's central worker-skills/ repository.
# Uses workers-registry.json as the source of truth for which workers have which skills.
#
# Usage:
#   push-worker-skills.sh --worker <name>                              # Push all skills for a worker
#   push-worker-skills.sh --skill <skill-name>                        # Push skill to all workers that have it
#   push-worker-skills.sh --worker <name> --add-skill <skill-name>    # Add skill to worker and push
#   push-worker-skills.sh --worker <name> --remove-skill <skill-name> # Remove skill from worker
#   [--no-notify]  Skip Matrix notification

set -e
source /opt/hiclaw/scripts/lib/hiclaw-env.sh

# ============================================================
# Parse arguments
# ============================================================
WORKER_NAME=""
SKILL_NAME=""
ADD_SKILL=""
REMOVE_SKILL=""
NOTIFY=true

while [ $# -gt 0 ]; do
    case "$1" in
        --worker)       WORKER_NAME="$2"; shift 2 ;;
        --skill)        SKILL_NAME="$2"; shift 2 ;;
        --add-skill)    ADD_SKILL="$2"; shift 2 ;;
        --remove-skill) REMOVE_SKILL="$2"; shift 2 ;;
        --no-notify)    NOTIFY=false; shift ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

# Validate arguments
if [ -z "${WORKER_NAME}" ] && [ -z "${SKILL_NAME}" ]; then
    echo "Usage:"
    echo "  push-worker-skills.sh --worker <name>                    # Push all skills for worker"
    echo "  push-worker-skills.sh --skill <skill-name>               # Push skill to all workers"
    echo "  push-worker-skills.sh --worker <name> --add-skill <name> # Add skill to worker"
    echo "  push-worker-skills.sh --worker <name> --remove-skill <name> # Remove skill"
    echo "  [--no-notify]"
    exit 1
fi

if [ -n "${ADD_SKILL}" ] && [ -n "${REMOVE_SKILL}" ]; then
    echo "Error: --add-skill and --remove-skill cannot be used together"
    exit 1
fi

if [ -n "${SKILL_NAME}" ] && ([ -n "${ADD_SKILL}" ] || [ -n "${REMOVE_SKILL}" ]); then
    echo "Error: --skill cannot be combined with --add-skill or --remove-skill"
    exit 1
fi

REGISTRY_FILE="${HOME}/workers-registry.json"
WORKER_SKILLS_DIR="/opt/hiclaw/agent/worker-skills"
MATRIX_DOMAIN="${HICLAW_MATRIX_DOMAIN:-matrix-local.hiclaw.io:8080}"

# ============================================================
# Load or initialize registry
# ============================================================
_load_registry() {
    if [ ! -f "${REGISTRY_FILE}" ]; then
        log "Registry not found, initializing..."
        mkdir -p "$(dirname "${REGISTRY_FILE}")"
        echo '{"version":1,"updated_at":"","workers":{}}' > "${REGISTRY_FILE}"
    fi
    cat "${REGISTRY_FILE}"
}

_save_registry() {
    local registry="$1"
    # Update updated_at timestamp
    registry=$(echo "${registry}" | jq --arg ts "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" '.updated_at = $ts')
    echo "${registry}" | jq . > "${REGISTRY_FILE}"
    log "Registry saved"
}

_get_worker_skills() {
    local registry="$1"
    local worker="$2"
    echo "${registry}" | jq -r --arg w "${worker}" '.workers[$w].skills // [] | .[]'
}

_get_worker_room_id() {
    local registry="$1"
    local worker="$2"
    echo "${registry}" | jq -r --arg w "${worker}" '.workers[$w].room_id // empty'
}

_worker_exists() {
    local registry="$1"
    local worker="$2"
    echo "${registry}" | jq -e --arg w "${worker}" '.workers[$w] != null' > /dev/null 2>&1
}

# ============================================================
# Push a single skill to a single worker via MinIO
# ============================================================
_push_skill_to_worker() {
    local worker="$1"
    local skill="$2"
    local skill_dst="${HICLAW_STORAGE_PREFIX}/agents/${worker}/skills/${skill}/"

    # Runtime-specific skills: check if skill exists in worker-agent or copaw-worker-agent
    local worker_runtime
    worker_runtime=$(echo "${REGISTRY}" | jq -r --arg w "${worker}" '.workers[$w].runtime // "openclaw"')
    local _rt_src
    if [ "${worker_runtime}" = "copaw" ]; then
        _rt_src="/opt/hiclaw/agent/copaw-worker-agent/skills/${skill}"
    else
        _rt_src="/opt/hiclaw/agent/worker-agent/skills/${skill}"
    fi
    if [ -d "${_rt_src}" ]; then
        log "  Pushing skill '${skill}' (runtime=${worker_runtime}) to worker '${worker}'..."
        mc mirror "${_rt_src}/" "${skill_dst}" --overwrite \
            2>&1 | tail -3 || {
            log "  WARNING: Failed to push skill '${skill}' to worker '${worker}'"
            return 1
        }
        log "  Pushed skill '${skill}' to worker '${worker}'"
        return 0
    fi

    local skill_src="${WORKER_SKILLS_DIR}/${skill}"
    if [ ! -d "${skill_src}" ]; then
        log "  WARNING: Skill source not found: ${skill_src}"
        return 1
    fi

    log "  Pushing skill '${skill}' to worker '${worker}'..."
    mc mirror "${skill_src}/" "${skill_dst}" --overwrite \
        2>&1 | tail -3 || {
        log "  WARNING: Failed to push skill '${skill}' to worker '${worker}'"
        return 1
    }
    log "  Pushed skill '${skill}' to worker '${worker}'"
}

# ============================================================
# Send Matrix notification to a worker
# ============================================================
_notify_worker() {
    local worker="$1"
    local room_id="$2"
    local skills_list="$3"

    if [ -z "${room_id}" ]; then
        log "  WARNING: No room_id for worker '${worker}', skipping notification"
        return 0
    fi

    # Ensure we have a Manager Matrix token
    local token="${MANAGER_MATRIX_TOKEN:-}"
    if [ -z "${token}" ]; then
        local secrets_file="/data/hiclaw-secrets.env"
        [ -f "${secrets_file}" ] && source "${secrets_file}"
        token="${MANAGER_MATRIX_TOKEN:-}"
    fi
    if [ -z "${token}" ]; then
        log "  WARNING: MANAGER_MATRIX_TOKEN not available, skipping notification"
        return 0
    fi

    local matrix_url="${HICLAW_MATRIX_SERVER}"
    local msg="@${worker}:${MATRIX_DOMAIN} 我已向你的工作区推送了以下 skills 更新：[${skills_list}]。请使用 file-sync 技能同步最新文件。"
    local worker_id="@${worker}:${MATRIX_DOMAIN}"

    local txn_id
    txn_id="pws-$(date +%s%N)"

    curl -sf -X PUT \
        "${matrix_url}/_matrix/client/v3/rooms/${room_id}/send/m.room.message/${txn_id}" \
        -H "Authorization: Bearer ${token}" \
        -H 'Content-Type: application/json' \
        -d "{\"msgtype\":\"m.text\",\"body\":\"${msg}\",\"m.mentions\":{\"user_ids\":[\"${worker_id}\"]}}" \
        > /dev/null 2>&1 \
        || log "  WARNING: Failed to send Matrix notification to ${worker}"
    log "  Notified worker '${worker}' via Matrix"
}

# ============================================================
# Main logic
# ============================================================
REGISTRY=$(_load_registry)

# Handle --add-skill: update registry first
if [ -n "${ADD_SKILL}" ] && [ -n "${WORKER_NAME}" ]; then
    if ! _worker_exists "${REGISTRY}" "${WORKER_NAME}"; then
        log "ERROR: Worker '${WORKER_NAME}' not found in registry"
        exit 1
    fi

    # Verify skill source exists before updating registry
    _add_skill_src="${WORKER_SKILLS_DIR}/${ADD_SKILL}"
    if [ ! -d "${_add_skill_src}" ]; then
        log "ERROR: Skill source not found: ${_add_skill_src}"
        exit 1
    fi

    ALREADY=$(echo "${REGISTRY}" | jq -r --arg w "${WORKER_NAME}" --arg s "${ADD_SKILL}" \
        '.workers[$w].skills // [] | map(select(. == $s)) | length')
    if [ "${ALREADY}" -gt 0 ]; then
        log "Skill '${ADD_SKILL}' already assigned to '${WORKER_NAME}', will re-push"
    else
        REGISTRY=$(echo "${REGISTRY}" | jq --arg w "${WORKER_NAME}" --arg s "${ADD_SKILL}" \
            '.workers[$w].skills += [$s]')
        log "Added skill '${ADD_SKILL}' to worker '${WORKER_NAME}' in registry"
    fi
    SKILL_NAME="${ADD_SKILL}"
fi

# Handle --remove-skill: update registry and return (no push needed)
if [ -n "${REMOVE_SKILL}" ] && [ -n "${WORKER_NAME}" ]; then
    if ! _worker_exists "${REGISTRY}" "${WORKER_NAME}"; then
        log "ERROR: Worker '${WORKER_NAME}' not found in registry"
        exit 1
    fi

    # Builtin skills are always present (pushed from worker-agent/skills/ or
    # copaw-worker-agent/skills/) and must not be removed from the registry.
    BUILTIN_SKILLS=("file-sync" "task-progress" "project-participation" "mcporter" "find-skills")
    for _bs in "${BUILTIN_SKILLS[@]}"; do
        if [ "${REMOVE_SKILL}" = "${_bs}" ]; then
            log "ERROR: Cannot remove builtin skill '${_bs}'"
            exit 1
        fi
    done

    REGISTRY=$(echo "${REGISTRY}" | jq --arg w "${WORKER_NAME}" --arg s "${REMOVE_SKILL}" \
        '.workers[$w].skills = [.workers[$w].skills // [] | .[] | select(. != $s)]')
    _save_registry "${REGISTRY}"
    log "Removed skill '${REMOVE_SKILL}' from worker '${WORKER_NAME}'"
    # Actually delete skill files from MinIO
    mc rm --recursive --force \
        "${HICLAW_STORAGE_PREFIX}/agents/${WORKER_NAME}/skills/${REMOVE_SKILL}/" \
        2>&1 || log "WARNING: mc rm failed (files may not exist in MinIO)"
    # Notify worker so it picks up the deletion promptly
    if [ "${NOTIFY}" = true ]; then
        room_id=$(_get_worker_room_id "${REGISTRY}" "${WORKER_NAME}")
        _notify_worker "${WORKER_NAME}" "${room_id}" "${REMOVE_SKILL} (removed)"
    fi
    exit 0
fi

# ============================================================
# Determine target (worker, skill) pairs to push
# ============================================================
declare -A WORKER_SKILLS_MAP  # worker -> comma-separated skills pushed

if [ -n "${WORKER_NAME}" ] && [ -n "${SKILL_NAME}" ]; then
    # Push specific skill to specific worker (--worker + --add-skill resolved above)
    if ! _worker_exists "${REGISTRY}" "${WORKER_NAME}"; then
        log "ERROR: Worker '${WORKER_NAME}' not found in registry"
        exit 1
    fi
    _push_skill_to_worker "${WORKER_NAME}" "${SKILL_NAME}"
    WORKER_SKILLS_MAP["${WORKER_NAME}"]="${SKILL_NAME}"

elif [ -n "${WORKER_NAME}" ]; then
    # Push all skills for a specific worker
    if ! _worker_exists "${REGISTRY}" "${WORKER_NAME}"; then
        log "ERROR: Worker '${WORKER_NAME}' not found in registry"
        exit 1
    fi
    pushed_skills=""
    while IFS= read -r skill; do
        [ -z "${skill}" ] && continue
        if _push_skill_to_worker "${WORKER_NAME}" "${skill}"; then
            pushed_skills="${pushed_skills:+${pushed_skills}, }${skill}"
        fi
    done < <(_get_worker_skills "${REGISTRY}" "${WORKER_NAME}")
    [ -n "${pushed_skills}" ] && WORKER_SKILLS_MAP["${WORKER_NAME}"]="${pushed_skills}"

elif [ -n "${SKILL_NAME}" ]; then
    # Push specific skill to all workers that have it
    WORKER_LIST=$(echo "${REGISTRY}" | jq -r --arg s "${SKILL_NAME}" \
        '.workers | to_entries[] | select(.value.skills // [] | map(select(. == $s)) | length > 0) | .key')
    if [ -z "${WORKER_LIST}" ]; then
        log "No workers found with skill '${SKILL_NAME}'"
        exit 0
    fi
    while IFS= read -r worker; do
        [ -z "${worker}" ] && continue
        if _push_skill_to_worker "${worker}" "${SKILL_NAME}"; then
            WORKER_SKILLS_MAP["${worker}"]="${SKILL_NAME}"
        fi
    done <<< "${WORKER_LIST}"
fi

# ============================================================
# Update skills_updated_at in registry for affected workers
# ============================================================
NOW=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
for worker in "${!WORKER_SKILLS_MAP[@]}"; do
    REGISTRY=$(echo "${REGISTRY}" | jq --arg w "${worker}" --arg ts "${NOW}" \
        '.workers[$w].skills_updated_at = $ts')
done

_save_registry "${REGISTRY}"

# ============================================================
# Send Matrix notifications
# ============================================================
if [ "${NOTIFY}" = true ] && [ ${#WORKER_SKILLS_MAP[@]} -gt 0 ]; then
    for worker in "${!WORKER_SKILLS_MAP[@]}"; do
        skills_pushed="${WORKER_SKILLS_MAP[$worker]}"
        room_id=$(_get_worker_room_id "${REGISTRY}" "${worker}")
        _notify_worker "${worker}" "${room_id}" "${skills_pushed}"
    done
fi

log "Done. Skills pushed to ${#WORKER_SKILLS_MAP[@]} worker(s)."
