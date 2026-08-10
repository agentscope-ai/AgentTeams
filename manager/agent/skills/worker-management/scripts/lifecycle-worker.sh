#!/bin/bash
# lifecycle-worker.sh - Manage Worker CR lifecycle through the Controller API.

set -euo pipefail

LIFECYCLE_FILE="${HOME}/worker-lifecycle.json"
STATE_FILE="${HOME}/state.json"

_ts() { date -u '+%Y-%m-%dT%H:%M:%SZ'; }
_log() { echo "[lifecycle $(date '+%Y-%m-%d %H:%M:%S')] $1"; }

_init_files() {
    if [ ! -f "${LIFECYCLE_FILE}" ]; then
        jq -n --argjson timeout "${AGENTTEAMS_WORKER_IDLE_TIMEOUT:-720}" --arg ts "$(_ts)" \
            '{version:1,idle_timeout_minutes:$timeout,updated_at:$ts,workers:{}}' > "${LIFECYCLE_FILE}"
    fi
    if [ ! -f "${STATE_FILE}" ]; then
        jq -n --arg ts "$(_ts)" '{active_tasks:[],updated_at:$ts}' > "${STATE_FILE}"
    fi
}

_worker_json() { agt get workers "$1" -o json; }
_worker_names() { agt get workers -o json | jq -r '.workers[].name'; }

_set_lifecycle() {
    local worker="$1" field="$2" value="$3" tmp
    tmp=$(mktemp)
    jq --arg w "${worker}" --arg f "${field}" --arg v "${value}" --arg ts "$(_ts)" \
        '.workers[$w] = (.workers[$w] // {}) | .workers[$w][$f] = $v | .updated_at = $ts' \
        "${LIFECYCLE_FILE}" > "${tmp}" && mv "${tmp}" "${LIFECYCLE_FILE}"
}

_has_finite_tasks() {
    jq -e --arg w "$1" '[.active_tasks[] | select(.assigned_to == $w and .type == "finite")] | length > 0' \
        "${STATE_FILE}" >/dev/null 2>&1
}

action_sync_status() {
    _init_files
    local worker status
    while IFS= read -r worker; do
        [ -n "${worker}" ] || continue
        status=$(_worker_json "${worker}" | jq -r '.containerState // .phase // "Pending"')
        _set_lifecycle "${worker}" container_status "${status}"
    done < <(_worker_names)
}

action_check_idle() {
    _init_files
    local timeout now worker state idle_since idle_epoch tmp
    timeout=$(jq -r '.idle_timeout_minutes // 720' "${LIFECYCLE_FILE}")
    now=$(date -u +%s)
    while IFS= read -r worker; do
        [ -n "${worker}" ] || continue
        state=$(_worker_json "${worker}" | jq -r '.state // "Running"')
        [ "${state}" = "Running" ] || continue
        if _has_finite_tasks "${worker}"; then
            _set_lifecycle "${worker}" idle_since ""
            continue
        fi
        idle_since=$(jq -r --arg w "${worker}" '.workers[$w].idle_since // empty' "${LIFECYCLE_FILE}")
        if [ -z "${idle_since}" ]; then
            _set_lifecycle "${worker}" idle_since "$(_ts)"
            continue
        fi
        idle_epoch=$(date -u -d "${idle_since}" +%s 2>/dev/null || date -j -f '%Y-%m-%dT%H:%M:%SZ' "${idle_since}" +%s 2>/dev/null || echo "${now}")
        if [ $((now - idle_epoch)) -ge $((timeout * 60)) ]; then
            action_stop "${worker}" true
        fi
    done < <(_worker_names)
}

action_stop() {
    local worker="$1" automatic="${2:-false}"
    _init_files
    agt update worker --name "${worker}" --state Sleeping >/dev/null
    _set_lifecycle "${worker}" container_status sleeping
    if [ "${automatic}" = true ]; then
        _set_lifecycle "${worker}" auto_stopped_at "$(_ts)"
    fi
    _log "Worker ${worker} desired state set to Sleeping"
}

action_start() {
    local worker="$1"
    _init_files
    agt update worker --name "${worker}" --state Running >/dev/null
    _set_lifecycle "${worker}" container_status pending
    _set_lifecycle "${worker}" last_started_at "$(_ts)"
    _log "Worker ${worker} desired state set to Running"
}

action_delete() {
    local worker="$1"
    agt delete worker "${worker}"
    local tmp
    tmp=$(mktemp)
    jq --arg w "${worker}" --arg ts "$(_ts)" 'del(.workers[$w]) | .updated_at = $ts' \
        "${LIFECYCLE_FILE}" > "${tmp}" && mv "${tmp}" "${LIFECYCLE_FILE}"
}

action_ensure_ready() {
    local worker="$1" current phase
    current=$(_worker_json "${worker}")
    phase=$(echo "${current}" | jq -r '.phase // "Pending"')
    if [ "${phase}" = "Running" ]; then
        jq -n --arg worker "${worker}" '{worker:$worker,status:"ready",container_status:"running"}'
        return 0
    fi
    action_start "${worker}" >&2
    jq -n --arg worker "${worker}" '{worker:$worker,status:"starting",container_status:"pending"}'
}

action=""
worker=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --action) action="$2"; shift 2 ;;
        --worker) worker="$2"; shift 2 ;;
        *) echo "Usage: $0 --action sync-status|check-idle|start|stop|delete|ensure-ready [--worker NAME]" >&2; exit 1 ;;
    esac
done

case "${action}" in
    sync-status) action_sync_status ;;
    check-idle) action_check_idle ;;
    start) [ -n "${worker}" ] || exit 2; action_start "${worker}" ;;
    stop) [ -n "${worker}" ] || exit 2; action_stop "${worker}" ;;
    delete) [ -n "${worker}" ] || exit 2; action_delete "${worker}" ;;
    ensure-ready) [ -n "${worker}" ] || exit 2; action_ensure_ready "${worker}" ;;
    *) echo "Unknown action: ${action}" >&2; exit 2 ;;
esac
