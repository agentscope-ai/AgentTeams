#!/bin/bash
# push-worker-skills.sh - Distribute Worker skills and reconcile Worker CR specs.

set -euo pipefail

WORKER_NAME=""
SKILL_NAME=""
ADD_SKILL=""
REMOVE_SKILL=""
NOTIFY=true

if [ -f /opt/agentteams/scripts/lib/agentteams-env.sh ]; then
    # shellcheck disable=SC1091
    source /opt/agentteams/scripts/lib/agentteams-env.sh
fi
AGENTTEAMS_STORAGE_PREFIX="${AGENTTEAMS_STORAGE_PREFIX:-agentteams/agentteams-storage}"

find_skill_source() {
    local skill="$1"
    local candidate
    for candidate in \
        "${HOME}/worker-skills/${skill}" \
        "/root/manager-workspace/worker-skills/${skill}" \
        "/root/agentteams-fs/agents/manager/worker-skills/${skill}" \
        "/opt/agentteams/agent/worker-skills/${skill}"
    do
        if [ -f "${candidate}/SKILL.md" ]; then
            printf '%s\n' "${candidate}"
            return 0
        fi
    done
    return 1
}

mirror_skill() {
    local worker="$1"
    local skill="$2"
    local source
    local destination="${AGENTTEAMS_STORAGE_PREFIX}/agents/${worker}/skills/${skill}"
    if declare -F ensure_mc_credentials >/dev/null 2>&1; then
        ensure_mc_credentials
    fi
    if source=$(find_skill_source "${skill}"); then
        # Keep the Worker copy exact so files removed from a replacement Skill
        # do not survive in object storage.
        mc mirror "${source}/" "${destination}/" --overwrite --remove
        # Some mc releases can print an S3 authorization error yet still exit
        # zero from `mirror`. Verify the contract file before changing the CR,
        # otherwise the Manager would report success for a missing skill.
        if ! mc stat "${destination}/SKILL.md" >/dev/null 2>&1; then
            echo "Worker skill upload verification failed: ${destination}/SKILL.md" >&2
            return 1
        fi
        return
    fi
    # A Manager may have uploaded a custom skill immediately before updating
    # the CR. Controller reconciliation can reuse that verified remote copy
    # even when it cannot mount the Manager workspace (for example on K8s).
    if mc stat "${destination}/SKILL.md" >/dev/null 2>&1; then
        return
    fi
    echo "Worker skill not found: ${HOME}/worker-skills/${skill}/SKILL.md (or built-in /opt/agentteams/agent/worker-skills/${skill}/SKILL.md)" >&2
    return 1
}

notify_worker() {
    local worker="$1"
    local room_id="$2"
    local skills_list="$3"
    local token="${AGENTTEAMS_MANAGER_MATRIX_TOKEN:-}"
    local matrix_url="${AGENTTEAMS_MATRIX_URL:-}"
    local matrix_domain="${AGENTTEAMS_MATRIX_DOMAIN:-}"
    local worker_id message payload txn_id

    if [ -z "${room_id}" ] || [ -z "${token}" ] || [ -z "${matrix_url}" ] || [ -z "${matrix_domain}" ]; then
        echo "Worker skill files were pushed, but Matrix notification is unavailable for ${worker}" >&2
        return 0
    fi

    worker_id="@${worker}:${matrix_domain}"
    message="${worker_id} Your Manager updated these workspace skills: [${skills_list}]. Use your file-sync skill now to pull the latest files."
    payload=$(jq -nc \
        --arg body "${message}" \
        --arg worker_id "${worker_id}" \
        '{msgtype:"m.text", body:$body, "m.mentions":{user_ids:[$worker_id]}}')
    txn_id="worker-skills-$(date +%s)-$$"

    curl -fsS -X PUT \
        "${matrix_url%/}/_matrix/client/v3/rooms/${room_id}/send/m.room.message/${txn_id}" \
        -H "Authorization: Bearer ${token}" \
        -H 'Content-Type: application/json' \
        --data "${payload}" >/dev/null \
        || echo "Worker skill files were pushed, but Matrix notification failed for ${worker}" >&2
}

while [ $# -gt 0 ]; do
    case "$1" in
        --worker) WORKER_NAME="$2"; shift 2 ;;
        --skill) SKILL_NAME="$2"; shift 2 ;;
        --add-skill) ADD_SKILL="$2"; shift 2 ;;
        --remove-skill) REMOVE_SKILL="$2"; shift 2 ;;
        --no-notify) NOTIFY=false; shift ;;
        *) echo "Unknown option: $1" >&2; exit 2 ;;
    esac
done

if [ -z "${WORKER_NAME}" ] && [ -z "${SKILL_NAME}" ]; then
    echo "Usage: $0 --worker NAME [--add-skill SKILL|--remove-skill SKILL] | --skill SKILL" >&2
    exit 2
fi
[ -z "${ADD_SKILL}" ] || [ -z "${REMOVE_SKILL}" ] || { echo "--add-skill and --remove-skill are mutually exclusive" >&2; exit 2; }

# Controller reconciliation already knows the Worker and assigned Skill. This
# internal form avoids calling the Controller API from inside its own reconcile
# loop, which may run before the API bearer token is ready during startup.
if [ -n "${WORKER_NAME}" ] && [ -n "${SKILL_NAME}" ] \
    && [ -z "${ADD_SKILL}" ] && [ -z "${REMOVE_SKILL}" ] \
    && [ "${NOTIFY}" = false ]; then
    mirror_skill "${WORKER_NAME}" "${SKILL_NAME}"
    exit 0
fi

workers_json=$(agt get workers -o json)
if [ -n "${WORKER_NAME}" ]; then
    targets="${WORKER_NAME}"
else
    targets=$(echo "${workers_json}" | jq -r --arg skill "${SKILL_NAME}" '.workers[] | select((.skills // []) | index($skill)) | .name')
fi

for worker in ${targets}; do
    current=$(agt get workers "${worker}" -o json | jq '.skills // []')
    if [ -n "${ADD_SKILL}" ]; then
        desired=$(echo "${current}" | jq --arg skill "${ADD_SKILL}" 'if index($skill) then . else . + [$skill] end')
    elif [ -n "${REMOVE_SKILL}" ]; then
        desired=$(echo "${current}" | jq --arg skill "${REMOVE_SKILL}" 'map(select(. != $skill))')
    else
        desired="${current}"
    fi

    while IFS= read -r skill; do
        [ -z "${skill}" ] || mirror_skill "${worker}" "${skill}"
    done < <(echo "${desired}" | jq -r '.[]')

    # Re-push mode mirrors current assignments without rewriting the same CR.
    if [ -z "${ADD_SKILL}" ] && [ -z "${REMOVE_SKILL}" ]; then
        if [ "${NOTIFY}" = true ]; then
            room_id=$(echo "${workers_json}" | jq -r --arg worker "${worker}" \
                '.workers[] | select(.name == $worker) | .roomID // empty')
            notify_skills="${SKILL_NAME:-$(echo "${desired}" | jq -r 'join(", ")')}"
            notify_worker "${worker}" "${room_id}" "${notify_skills}"
        fi
        continue
    fi
    csv=$(echo "${desired}" | jq -r 'join(",")')
    agt update worker --name "${worker}" --skills "${csv}"
    confirmed=$(agt get workers "${worker}" -o json | jq '.skills // []')
    if [ -n "${ADD_SKILL}" ]; then
        if ! echo "${confirmed}" | jq -e --arg skill "${ADD_SKILL}" 'index($skill)' >/dev/null; then
            echo "Worker Skill assignment verification failed: ${worker} does not list ${ADD_SKILL}" >&2
            exit 1
        fi
    elif [ -n "${REMOVE_SKILL}" ]; then
        if echo "${confirmed}" | jq -e --arg skill "${REMOVE_SKILL}" 'index($skill)' >/dev/null; then
            echo "Worker Skill removal verification failed: ${worker} still lists ${REMOVE_SKILL}" >&2
            exit 1
        fi
    fi
    if [ "${NOTIFY}" = true ]; then
        room_id=$(echo "${workers_json}" | jq -r --arg worker "${worker}" \
            '.workers[] | select(.name == $worker) | .roomID // empty')
        notify_skills="${ADD_SKILL:-${REMOVE_SKILL:-${csv}}}"
        notify_worker "${worker}" "${room_id}" "${notify_skills}"
    fi
done
