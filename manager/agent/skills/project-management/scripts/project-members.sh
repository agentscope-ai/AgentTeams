#!/bin/bash
# project-members.sh - Member-list helpers for create-project.sh (sourced).

# project_team_roster TEAM
#   Emits runtime-name<TAB>credential-name rows, leader first. Team references
#   and credential files use Worker CR names; Matrix and workspace storage use
#   workerName, falling back to the CR name as EffectiveWorkerName does.
project_team_roster() {
    local team="$1"
    local team_json="" roster="" worker_json="" runtime_name="" worker=""
    local resolved_roster=""

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
    while IFS= read -r worker; do
        if ! worker_json=$(agt get workers "${worker}" -o json 2>/dev/null); then
            echo "Worker ${worker} in Team ${team} not found or not readable" >&2
            return 1
        fi
        if ! runtime_name=$(printf '%s' "${worker_json}" | jq -er --arg name "${worker}" '
            select(.name == $name)
            | (if (.workerName // "") == "" then .name else .workerName end)
            | select(type == "string" and length > 0)' 2>/dev/null); then
            echo "Worker ${worker} in Team ${team} returned no readable runtime identity" >&2
            return 1
        fi
        resolved_roster="${resolved_roster}${runtime_name}"$'\t'"${worker}"$'\n'
    done <<< "${roster}"
    # A later lookup may fail: never mix a partial roster into the error JSON.
    printf '%s' "${resolved_roster}"
}

# project_merge_members CSV [CSV ...]
#   Trim comma lists and keep each nonempty runtime name once, in order.
project_merge_members() {
    printf '%s\n' "$@" | tr ',' '\n' | tr -d ' \r' | awk 'NF && !seen[$0]++' | paste -sd, -
}
