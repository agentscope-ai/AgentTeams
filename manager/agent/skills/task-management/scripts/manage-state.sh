#!/bin/bash
# manage-state.sh - Atomic state.json operations for task tracking
#
# Replaces manual jq edits by the LLM Agent with deterministic script calls.
# All writes use tmp+mv for atomicity.
#
# Usage:
#   manage-state.sh --action init
#   manage-state.sh --action add-finite    --task-id T --title TITLE --assigned-to W --room-id R [--project-room-id P] [--delegated-to-team TEAM]
#   manage-state.sh --action add-infinite  --task-id T --title TITLE --assigned-to W --room-id R --schedule CRON --timezone TZ --next-scheduled-at ISO
#   manage-state.sh --action complete      --task-id T
#   manage-state.sh --action executed      --task-id T --next-scheduled-at ISO
#   manage-state.sh --action set-admin-dm  --room-id R
#   manage-state.sh --action list

set -euo pipefail

STATE_FILE="${HOME}/state.json"

_ts() {
    date -u '+%Y-%m-%dT%H:%M:%SZ'
}

_ensure_state_file() {
    if [ ! -f "$STATE_FILE" ]; then
        cat > "$STATE_FILE" << EOF
{
  "admin_dm_room_id": null,
  "active_tasks": [],
  "updated_at": "$(_ts)"
}
EOF
    else
        # Backfill admin_dm_room_id for pre-existing state files
        local has_field
        has_field=$(jq 'has("admin_dm_room_id")' "$STATE_FILE")
        if [ "$has_field" = "false" ]; then
            local tmp
            tmp=$(mktemp)
            jq '. + {admin_dm_room_id: null}' "$STATE_FILE" > "$tmp" && mv "$tmp" "$STATE_FILE"
        fi
    fi
}

# ─── Actions ─────────────────────────────────────────────────────────────────

action_init() {
    _ensure_state_file
    echo "OK: state.json ready at $STATE_FILE"
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
       --arg proj "${PROJECT_ROOM_ID:-}" \
       --arg team "${DELEGATED_TO_TEAM:-}" \
       --arg ts "$(_ts)" \
       '.active_tasks += [{
            task_id: $id,
            title: $title,
            type: "finite",
            assigned_to: $worker,
            room_id: $room
        } + (if $proj != "" then {project_room_id: $proj} else {} end)
          + (if $team != "" then {delegated_to_team: $team} else {} end)]
        | .updated_at = $ts' \
       "$STATE_FILE" > "$tmp" && mv "$tmp" "$STATE_FILE"

    echo "OK: added finite task $TASK_ID \"$TITLE\" (assigned to $ASSIGNED_TO)"
}

action_add_infinite() {
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
       --arg sched "$SCHEDULE" \
       --arg tz "$TIMEZONE" \
       --arg next "$NEXT_SCHEDULED_AT" \
       --arg ts "$(_ts)" \
       '.active_tasks += [{
            task_id: $id,
            title: $title,
            type: "infinite",
            assigned_to: $worker,
            room_id: $room,
            schedule: $sched,
            timezone: $tz,
            last_executed_at: null,
            next_scheduled_at: $next
        }]
        | .updated_at = $ts' \
       "$STATE_FILE" > "$tmp" && mv "$tmp" "$STATE_FILE"

    echo "OK: added infinite task $TASK_ID \"$TITLE\" (assigned to $ASSIGNED_TO, next: $NEXT_SCHEDULED_AT)"
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

    echo "OK: removed task $TASK_ID from active_tasks"
}

action_executed() {
    _ensure_state_file

    local existing
    existing=$(jq -r --arg id "$TASK_ID" \
        '[.active_tasks[] | select(.task_id == $id and .type == "infinite")] | length' "$STATE_FILE")
    if [ "$existing" -eq 0 ]; then
        echo "WARN: infinite task $TASK_ID not found in active_tasks (may be a legacy task not yet registered). Skipping update."
        return 0
    fi

    local tmp
    tmp=$(mktemp)
    jq --arg id "$TASK_ID" \
       --arg next "$NEXT_SCHEDULED_AT" \
       --arg now "$(_ts)" \
       '(.active_tasks[] | select(.task_id == $id))
        |= (.last_executed_at = $now | .next_scheduled_at = $next)
        | .updated_at = $now' \
       "$STATE_FILE" > "$tmp" && mv "$tmp" "$STATE_FILE"

    echo "OK: updated infinite task $TASK_ID (last_executed_at=$(_ts), next_scheduled_at=$NEXT_SCHEDULED_AT)"
}

action_set_admin_dm() {
    _ensure_state_file

    local tmp
    tmp=$(mktemp)
    jq --arg room "$ROOM_ID" --arg ts "$(_ts)" \
       '.admin_dm_room_id = $room | .updated_at = $ts' \
       "$STATE_FILE" > "$tmp" && mv "$tmp" "$STATE_FILE"

    echo "OK: admin_dm_room_id set to $ROOM_ID"
}

action_list() {
    _ensure_state_file

    local admin_dm
    admin_dm=$(jq -r '.admin_dm_room_id // "null"' "$STATE_FILE")
    echo "Admin DM room: $admin_dm"

    local count
    count=$(jq '.active_tasks | length' "$STATE_FILE")
    if [ "$count" -eq 0 ]; then
        echo "No active tasks."
        return 0
    fi

    jq -r '.active_tasks[] | [.task_id, .type, .assigned_to, (.title // "-")] | @tsv' "$STATE_FILE" | \
        while IFS=$'\t' read -r tid ttype worker title; do
            echo "  $tid  type=$ttype  worker=$worker  title=\"$title\""
        done
    echo "Total: $count active task(s). Updated: $(jq -r '.updated_at' "$STATE_FILE")"
}

# ─── Argument parsing ─────────────────────────────────────────────────────────

ACTION=""
TASK_ID=""
TITLE=""
ASSIGNED_TO=""
ROOM_ID=""
PROJECT_ROOM_ID=""
DELEGATED_TO_TEAM=""
SCHEDULE=""
TIMEZONE=""
NEXT_SCHEDULED_AT=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --action)           ACTION="$2";            shift 2 ;;
        --task-id)          TASK_ID="$2";           shift 2 ;;
        --title)            TITLE="$2";             shift 2 ;;
        --assigned-to)      ASSIGNED_TO="$2";       shift 2 ;;
        --room-id)          ROOM_ID="$2";           shift 2 ;;
        --project-room-id)  PROJECT_ROOM_ID="$2";   shift 2 ;;
        --delegated-to-team) DELEGATED_TO_TEAM="$2"; shift 2 ;;
        --schedule)         SCHEDULE="$2";          shift 2 ;;
        --timezone)         TIMEZONE="$2";          shift 2 ;;
        --next-scheduled-at) NEXT_SCHEDULED_AT="$2"; shift 2 ;;
        *)
            echo "Unknown argument: $1" >&2
            exit 1
            ;;
    esac
done

if [ -z "$ACTION" ]; then
    echo "Usage: $0 --action <init|add-finite|add-infinite|complete|executed|set-admin-dm|list> [options]" >&2
    echo "" >&2
    echo "Actions:" >&2
    echo "  init          Ensure state.json exists (no-op if already present)" >&2
    echo "  add-finite    --task-id T --title TITLE --assigned-to W --room-id R [--project-room-id P] [--delegated-to-team TEAM]" >&2
    echo "  add-infinite  --task-id T --title TITLE --assigned-to W --room-id R --schedule CRON --timezone TZ --next-scheduled-at ISO" >&2
    echo "  complete      --task-id T   (removes finite task from active_tasks)" >&2
    echo "  executed      --task-id T --next-scheduled-at ISO   (updates infinite task after execution)" >&2
    echo "  set-admin-dm  --room-id R   (saves admin DM room ID for heartbeat use)" >&2
    echo "  list          Show all active tasks" >&2
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
        action_init
        ;;
    add-finite)
        _validate_required TASK_ID TITLE ASSIGNED_TO ROOM_ID
        action_add_finite
        ;;
    add-infinite)
        _validate_required TASK_ID TITLE ASSIGNED_TO ROOM_ID SCHEDULE TIMEZONE NEXT_SCHEDULED_AT
        action_add_infinite
        ;;
    complete)
        _validate_required TASK_ID
        action_complete
        ;;
    executed)
        _validate_required TASK_ID NEXT_SCHEDULED_AT
        action_executed
        ;;
    set-admin-dm)
        _validate_required ROOM_ID
        action_set_admin_dm
        ;;
    list)
        action_list
        ;;
    *)
        echo "ERROR: Unknown action '$ACTION'. Use: init, add-finite, add-infinite, complete, executed, set-admin-dm, list" >&2
        exit 1
        ;;
esac
