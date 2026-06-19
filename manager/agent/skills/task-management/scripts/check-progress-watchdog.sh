#!/bin/bash
# check-progress-watchdog.sh - Detect task progress that has gone stale.
#
# Reads the latest progress block for an active finite task, compares it with
# the snapshot stored in ~/state.json, and updates watchdog fields atomically.

set -euo pipefail

STATE_FILE="${HOME}/state.json"
HICLAW_FS_ROOT="${HICLAW_FS_ROOT:-/root/hiclaw-fs}"

TASK_ID=""

_ts() {
    date -u '+%Y-%m-%dT%H:%M:%SZ'
}

_usage() {
    echo "Usage: $0 --task-id TASK_ID" >&2
}

_json_error() {
    local status="$1"
    local message="$2"
    jq -n \
        --arg task_id "${TASK_ID}" \
        --arg status "${status}" \
        --arg message "${message}" \
        '{task_id: $task_id, status: $status, message: $message}'
}

_extract_latest_block() {
    local task_id="$1"
    local progress_dir="${HICLAW_FS_ROOT}/shared/tasks/${task_id}/progress"

    if [ ! -d "${progress_dir}" ]; then
        return 1
    fi

    local latest_file
    latest_file=$(find "${progress_dir}" -type f -name '*.md' | sort | tail -n 1)
    if [ -z "${latest_file}" ] || [ ! -s "${latest_file}" ]; then
        return 1
    fi

    awk '
        /^##[[:space:]]/ {
            block = $0 "\n"
            in_block = 1
            next
        }
        in_block {
            block = block $0 "\n"
        }
        END {
            gsub(/[[:space:]]+$/, "", block)
            if (block != "") {
                print block
            }
        }
    ' "${latest_file}"
}

_fingerprint() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum | awk '{print $1}'
    else
        shasum -a 256 | awk '{print $1}'
    fi
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --task-id) TASK_ID="$2"; shift 2 ;;
        -h|--help) _usage; exit 0 ;;
        *)
            echo "Unknown argument: $1" >&2
            _usage
            exit 1
            ;;
    esac
done

if [ -z "${TASK_ID}" ]; then
    _usage
    exit 1
fi

if [ ! -f "${STATE_FILE}" ]; then
    _json_error "unknown" "state.json not found"
    exit 0
fi

task_count=$(jq -r --arg id "${TASK_ID}" '[.active_tasks[]? | select(.task_id == $id and .type == "finite")] | length' "${STATE_FILE}")
if [ "${task_count}" -eq 0 ]; then
    _json_error "unknown" "finite active task not found"
    exit 0
fi

latest_block="$(_extract_latest_block "${TASK_ID}" || true)"
if [ -z "${latest_block}" ]; then
    now="$(_ts)"
    tmp="$(mktemp)"
    jq --arg id "${TASK_ID}" --arg now "${now}" '
        (.active_tasks[] | select(.task_id == $id)) |= (
            .stale_heartbeat_count = ((.stale_heartbeat_count // 0) + 1)
            | .last_watchdog_action = "missing_progress"
            | .last_watchdog_checked_at = $now
        )
        | .updated_at = $now
    ' "${STATE_FILE}" > "${tmp}" && mv "${tmp}" "${STATE_FILE}"

    count=$(jq -r --arg id "${TASK_ID}" '.active_tasks[] | select(.task_id == $id) | .stale_heartbeat_count // 0' "${STATE_FILE}")
    jq -n \
        --arg task_id "${TASK_ID}" \
        --arg status "unknown" \
        --arg message "no progress log found" \
        --argjson stale_heartbeat_count "${count}" \
        '{task_id: $task_id, status: $status, message: $message, stale_heartbeat_count: $stale_heartbeat_count}'
    exit 0
fi

fingerprint="$(printf '%s' "${latest_block}" | _fingerprint)"
previous_fingerprint=$(jq -r --arg id "${TASK_ID}" '.active_tasks[] | select(.task_id == $id) | .last_progress_fingerprint // empty' "${STATE_FILE}")
previous_count=$(jq -r --arg id "${TASK_ID}" '.active_tasks[] | select(.task_id == $id) | .stale_heartbeat_count // 0' "${STATE_FILE}")
now="$(_ts)"

if printf '%s\n' "${latest_block}" | grep -Eiq '\b(blocked|blocker)\b'; then
    status="blocked"
    count=0
    action="progress_blocked"
elif [ "${fingerprint}" = "${previous_fingerprint}" ]; then
    status="repeated"
    count=$((previous_count + 1))
    action="repeated_progress"
else
    status="normal"
    count=0
    action="progress_changed"
fi

summary="$(printf '%s\n' "${latest_block}" | head -n 1 | sed 's/^##[[:space:]]*//')"

tmp="$(mktemp)"
jq --arg id "${TASK_ID}" \
   --arg now "${now}" \
   --arg fingerprint "${fingerprint}" \
   --arg action "${action}" \
    --arg summary "${summary}" \
    --argjson count "${count}" '
    (.active_tasks[] | select(.task_id == $id)) |= (
        (if $action == "progress_changed" or $action == "progress_blocked" then .last_progress_at = $now else . end)
        | .last_progress_fingerprint = $fingerprint
        | .stale_heartbeat_count = $count
        | .last_watchdog_action = $action
        | .last_watchdog_checked_at = $now
        | .last_progress_summary = $summary
    )
    | .updated_at = $now
' "${STATE_FILE}" > "${tmp}" && mv "${tmp}" "${STATE_FILE}"

jq -n \
    --arg task_id "${TASK_ID}" \
    --arg status "${status}" \
    --arg last_progress_summary "${summary}" \
    --arg last_watchdog_action "${action}" \
    --argjson stale_heartbeat_count "${count}" \
    '{
        task_id: $task_id,
        status: $status,
        stale_heartbeat_count: $stale_heartbeat_count,
        last_progress_summary: $last_progress_summary,
        last_watchdog_action: $last_watchdog_action
    }'
