#!/bin/bash
# push-worker-skills.sh - Distribute Worker skills and reconcile Worker CR specs.

set -euo pipefail

WORKER_NAME=""
SKILL_NAME=""
ADD_SKILL=""
REMOVE_SKILL=""

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
        mc mirror "${source}/" "${destination}/" --overwrite
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

while [ $# -gt 0 ]; do
    case "$1" in
        --worker) WORKER_NAME="$2"; shift 2 ;;
        --skill) SKILL_NAME="$2"; shift 2 ;;
        --add-skill) ADD_SKILL="$2"; shift 2 ;;
        --remove-skill) REMOVE_SKILL="$2"; shift 2 ;;
        --no-notify) shift ;;
        *) echo "Unknown option: $1" >&2; exit 2 ;;
    esac
done

if [ -z "${WORKER_NAME}" ] && [ -z "${SKILL_NAME}" ]; then
    echo "Usage: $0 --worker NAME [--add-skill SKILL|--remove-skill SKILL] | --skill SKILL" >&2
    exit 2
fi
[ -z "${ADD_SKILL}" ] || [ -z "${REMOVE_SKILL}" ] || { echo "--add-skill and --remove-skill are mutually exclusive" >&2; exit 2; }

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

    # Controller reconciliation passes only --worker/--no-notify. Files must
    # still be mirrored, but rewriting the same CR here would recurse.
    if [ -z "${ADD_SKILL}" ] && [ -z "${REMOVE_SKILL}" ]; then
        continue
    fi
    csv=$(echo "${desired}" | jq -r 'join(",")')
    agt update worker --name "${worker}" --skills "${csv}"
done
