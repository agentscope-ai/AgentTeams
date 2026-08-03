#!/bin/bash
# find-worker.sh - Find suitable Worker CRs for a task.

set -euo pipefail

STATE_FILE="${HOME}/state.json"
LIFECYCLE_FILE="${HOME}/worker-lifecycle.json"
AGENTS_DIR="/root/agentteams-fs/agents"

FILTER_SKILLS=""
FILTER_WORKER=""
FILTER_TEAM=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --skills) FILTER_SKILLS="$2"; shift 2 ;;
        --worker) FILTER_WORKER="$2"; shift 2 ;;
        --team) FILTER_TEAM="$2"; shift 2 ;;
        *) echo "Usage: $0 [--skills s1,s2] [--worker <name>] [--team <team-name>]" >&2; exit 1 ;;
    esac
done

WORKERS_JSON=$(agt get workers -o json 2>/dev/null || echo '{"workers":[],"total":0}')

_get_role() {
    local soul="${AGENTS_DIR}/$1/SOUL.md"
    [ -f "${soul}" ] || return 0
    awk '/^## (Role|角色)/{found=1; next} /^## /{if(found) exit} found && NF{print}' "${soul}" 2>/dev/null \
        | sed 's/^- //' | paste -sd '|' - | sed 's/|/ | /g' || true
}

RESULTS='[]'
while IFS= read -r worker; do
    [ -n "${worker}" ] || continue
    name=$(echo "${worker}" | jq -r '.name')
    team=$(echo "${worker}" | jq -r '.team // ""')
    skills=$(echo "${worker}" | jq '.skills // []')

    [ -z "${FILTER_WORKER}" ] || [ "${name}" = "${FILTER_WORKER}" ] || continue
    [ -z "${FILTER_TEAM}" ] || [ "${team}" = "${FILTER_TEAM}" ] || continue

    if [ -n "${FILTER_SKILLS}" ]; then
        match=true
        IFS=',' read -ra required <<< "${FILTER_SKILLS}"
        for skill in "${required[@]}"; do
            skill=$(echo "${skill}" | tr -d ' ')
            [ -z "${skill}" ] && continue
            if ! echo "${skills}" | jq -e --arg skill "${skill}" 'index($skill) != null' >/dev/null; then
                match=false
                break
            fi
        done
        [ "${match}" = true ] || continue
    fi

    finite_tasks=0
    infinite_tasks=0
    active_tasks='[]'
    if [ -f "${STATE_FILE}" ]; then
        finite_tasks=$(jq --arg w "${name}" '[.active_tasks[] | select(.assigned_to == $w and .type == "finite")] | length' "${STATE_FILE}" 2>/dev/null || echo 0)
        infinite_tasks=$(jq --arg w "${name}" '[.active_tasks[] | select(.assigned_to == $w and .type == "infinite")] | length' "${STATE_FILE}" 2>/dev/null || echo 0)
        active_tasks=$(jq --arg w "${name}" '[.active_tasks[] | select(.assigned_to == $w) | {task_id: (.task_id // .id), type, title}]' "${STATE_FILE}" 2>/dev/null || echo '[]')
    fi

    container_status=$(echo "${worker}" | jq -r '.containerState // "unknown"')
    idle_since=null
    if [ -f "${LIFECYCLE_FILE}" ]; then
        lifecycle_status=$(jq -r --arg w "${name}" '.workers[$w].container_status // empty' "${LIFECYCLE_FILE}" 2>/dev/null || true)
        [ -z "${lifecycle_status}" ] || container_status="${lifecycle_status}"
        idle_since=$(jq --arg w "${name}" '.workers[$w].idle_since // null' "${LIFECYCLE_FILE}" 2>/dev/null || echo null)
    fi

    availability=idle
    case "${container_status}" in
        not_found) availability=unavailable ;;
        stopped|exited) availability=stopped ;;
        *) [ "${finite_tasks}" -eq 0 ] || availability=busy ;;
    esac

    role=$(_get_role "${name}")
    item=$(echo "${worker}" | jq \
        --arg availability "${availability}" \
        --arg container_status "${container_status}" \
        --arg role_description "${role}" \
        --argjson finite_tasks "${finite_tasks}" \
        --argjson infinite_tasks "${infinite_tasks}" \
        --argjson active_tasks "${active_tasks}" \
        --argjson idle_since "${idle_since}" \
        '. + {
            availability: $availability,
            container_status: $container_status,
            role_description: (if $role_description == "" then null else $role_description end),
            finite_tasks: $finite_tasks,
            infinite_tasks: $infinite_tasks,
            active_tasks: $active_tasks,
            idle_since: $idle_since
        }')
    RESULTS=$(echo "${RESULTS}" | jq --argjson worker "${item}" '. + [$worker]')
done < <(echo "${WORKERS_JSON}" | jq -c '.workers[]')

jq -n --argjson workers "${RESULTS}" '{
    total: ($workers | length),
    idle: ([$workers[] | select(.availability == "idle")] | length),
    busy: ([$workers[] | select(.availability == "busy")] | length),
    stopped: ([$workers[] | select(.availability == "stopped")] | length),
    unavailable: ([$workers[] | select(.availability == "unavailable")] | length),
    workers: $workers
}'
