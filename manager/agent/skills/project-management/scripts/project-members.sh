#!/bin/bash
# project-members.sh - Member-list helpers for create-project.sh (sourced, not run).
#
# Kept apart from create-project.sh, which needs the Manager container, so the
# roster, de-duplication and join reporting can be exercised on their own
# (tests/check-create-project-team.sh).

# project_team_roster TEAM
#   The Team's leader, then its workers, one per line, read from the
#   controller's Team resource (leaderName, workerNames). Fails with a message
#   on stderr when the Team cannot be read or names nobody.
project_team_roster() {
    local team="$1"
    local team_json=""
    local roster=""

    if ! team_json=$(agt get teams "${team}" -o json 2>/dev/null); then
        echo "Team ${team} not found or not readable" >&2
        return 1
    fi
    if ! roster=$(printf '%s' "${team_json}" | jq -r '([.leaderName // empty] + (.workerNames // []))[] | select(. != null and . != "")' 2>/dev/null); then
        echo "Team ${team} returned no readable roster" >&2
        return 1
    fi
    if [ -z "${roster}" ]; then
        echo "Team ${team} has no leader or workers" >&2
        return 1
    fi
    printf '%s\n' "${roster}"
}

# project_merge_members CSV [CSV ...]
#   Every name in the given comma lists, trimmed, empty entries dropped, each
#   once in first-seen order, as one comma list.
project_merge_members() {
    printf '%s\n' "$@" | tr ',' '\n' | tr -d ' \r' | awk 'NF && !seen[$0]++' | paste -sd, -
}

# project_worker_auto_join WORKER ROOM_ID
#   Logs WORKER into Matrix with its stored credentials and joins ROOM_ID.
#   Returns 0 only when the join succeeded: an invite the worker never
#   accepted is not membership, so the caller reports it.
project_worker_auto_join() {
    local worker="$1"
    local room_id="$2"
    local creds_file="/data/worker-creds/${worker}.env"
    local worker_token=""
    local room_enc=""
    local WORKER_PASSWORD=""
    local WORKER_MATRIX_TOKEN=""

    if [ -z "${worker}" ] || [ -z "${room_id}" ]; then
        return 1
    fi

    if [ -f "${creds_file}" ]; then
        # shellcheck disable=SC1090
        source "${creds_file}"
    fi

    if [ -z "${WORKER_PASSWORD}" ]; then
        WORKER_PASSWORD=$(mc cat "${AGENTTEAMS_STORAGE_PREFIX}/agents/${worker}/credentials/matrix/password" 2>/dev/null || true)
    fi
    if [ -z "${WORKER_PASSWORD}" ] && [ -f "/root/agentteams-fs/agents/${worker}/credentials/matrix/password" ]; then
        WORKER_PASSWORD=$(cat "/root/agentteams-fs/agents/${worker}/credentials/matrix/password" 2>/dev/null || true)
    fi

    if [ -n "${WORKER_PASSWORD}" ]; then
        worker_token=$(curl -sf -X POST ${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/login \
            -H 'Content-Type: application/json' \
            -d '{"type":"m.login.password","identifier":{"type":"m.id.user","user":"'"${worker}"'"},"password":"'"${WORKER_PASSWORD}"'"}' \
            2>/dev/null | jq -r '.access_token // empty')
    elif [ -n "${WORKER_MATRIX_TOKEN}" ]; then
        worker_token="${WORKER_MATRIX_TOKEN}"
    fi

    if [ -z "${worker_token}" ]; then
        log "  WARNING: Could not obtain Matrix token for worker ${worker} — worker will need to accept project room invite"
        return 1
    fi

    room_enc=$(echo "${room_id}" | sed 's/!/%21/g')
    if curl -sf -X POST "${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/rooms/${room_enc}/join" \
        -H "Authorization: Bearer ${worker_token}" \
        -H 'Content-Type: application/json' \
        -d '{}' > /dev/null 2>&1; then
        log "  Worker ${worker} auto-joined project room"
        return 0
    fi
    log "  WARNING: Worker ${worker} failed to auto-join project room"
    return 1
}
