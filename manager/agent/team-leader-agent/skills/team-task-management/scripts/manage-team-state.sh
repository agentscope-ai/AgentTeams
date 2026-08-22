#!/bin/bash
# manage-team-state.sh - Atomic team-state.json operations for team task tracking
#
# Same interface as Manager's manage-state.sh but operates on ~/team-state.json.
#
# Usage:
#   manage-team-state.sh --action init
#   manage-team-state.sh --action add-finite    --task-id T --title TITLE --assigned-to W --room-id R
#   manage-team-state.sh --action complete      --task-id T
#   manage-team-state.sh --action list

set -euo pipefail

STATE_FILE="${HOME}/team-state.json"

_ts() {
    date -u '+%Y-%m-%dT%H:%M:%SZ'
}

_ensure_state_file() {
    if [ ! -f "$STATE_FILE" ]; then
        cat > "$STATE_FILE" << EOF
{
  "team_id": null,
  "active_tasks": [],
  "updated_at": "$(_ts)"
}
EOF
    fi
}

action_init() {
    _ensure_state_file
    echo "OK: team-state.json ready at $STATE_FILE"
}

action_add_finite() {
    _ensure_state_file

    local existing
    existing=$(jq -r --arg id "$TASK_ID" \
        '[.active_tasks[] | select(.task_id == $id)] | length' "$STATE_FILE")
    if [ "$existing" -gt 0 ]; then
        echo "SKIP: task $TASK_ID already in active_tasks"
        return 0
    fi

    local tmp
    tmp=$(mktemp)
    jq --arg id "$TASK_ID" \
       --arg title "$TITLE" \
       --arg worker "$ASSIGNED_TO" \
       --arg room "$ROOM_ID" \
       --arg ts "$(_ts)" \
       '.active_tasks += [{
            task_id: $id,
            title: $title,
            type: "finite",
            assigned_to: $worker,
            room_id: $room
        }]
        | .updated_at = $ts' \
       "$STATE_FILE" > "$tmp" && mv "$tmp" "$STATE_FILE"

    echo "OK: added sub-task $TASK_ID \"$TITLE\" (assigned to $ASSIGNED_TO)"
}

action_complete() {
    _ensure_state_file

    local existing
    existing=$(jq -r --arg id "$TASK_ID" \
        '[.active_tasks[] | select(.task_id == $id)] | length' "$STATE_FILE")
    if [ "$existing" -eq 0 ]; then
        echo "SKIP: task $TASK_ID not found in active_tasks"
        return 0
    fi

    local tmp
    tmp=$(mktemp)
    jq --arg id "$TASK_ID" --arg ts "$(_ts)" \
       '.active_tasks = [.active_tasks[] | select(.task_id != $id)]
        | .updated_at = $ts' \
       "$STATE_FILE" > "$tmp" && mv "$tmp" "$STATE_FILE"

    echo "OK: removed sub-task $TASK_ID from active_tasks"
}

action_list() {
    _ensure_state_file

    local count
    count=$(jq '.active_tasks | length' "$STATE_FILE")
    if [ "$count" -eq 0 ]; then
        echo "No active team tasks."
        return 0
    fi

    jq -r '.active_tasks[] | [.task_id, .type, .assigned_to, (.title // "-")] | @tsv' "$STATE_FILE" | \
        while IFS=$'\t' read -r tid ttype worker title; do
            echo "  $tid  type=$ttype  worker=$worker  title=\"$title\""
        done
    echo "Total: $count active sub-task(s). Updated: $(jq -r '.updated_at' "$STATE_FILE")"
}

# ─── Argument parsing ─────────────────────────────────────────────────────────

ACTION=""
TASK_ID=""
TITLE=""
ASSIGNED_TO=""
ROOM_ID=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --action)      ACTION="$2";      shift 2 ;;
        --task-id)     TASK_ID="$2";     shift 2 ;;
        --title)       TITLE="$2";       shift 2 ;;
        --assigned-to) ASSIGNED_TO="$2"; shift 2 ;;
        --room-id)     ROOM_ID="$2";     shift 2 ;;
        *)
            echo "Unknown argument: $1" >&2
            exit 1
            ;;
    esac
done

if [ -z "$ACTION" ]; then
    echo "Usage: $0 --action <init|add-finite|complete|list> [options]" >&2
    exit 1
fi

_validate_required() {
    local missing=()
    for var in "$@"; do
        eval "val=\$$var"
        if [ -z "$val" ]; then
            missing+=("--$(echo "$var" | tr '_' '-' | tr '[:upper:]' '[:lower:]')")
        fi
    done
    if [ ${#missing[@]} -gt 0 ]; then
        echo "ERROR: missing required arguments for '$ACTION': ${missing[*]}" >&2
        exit 1
    fi
}

case "$ACTION" in
    init)
        action_init ;;
    add-finite)
        _validate_required TASK_ID TITLE ASSIGNED_TO ROOM_ID
        action_add_finite ;;
    complete)
        _validate_required TASK_ID
        action_complete ;;
    list)
        action_list ;;
    *)
        echo "ERROR: Unknown action '$ACTION'. Use: init, add-finite, complete, list" >&2
        exit 1
        ;;
esac
