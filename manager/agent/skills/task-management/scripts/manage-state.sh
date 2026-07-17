#!/bin/bash
# manage-state.sh - Atomic state.json operations for task tracking
#
# Prefer the hiclaw CLI implementation when available (Phase 14 I14.4).
# The shell body below remains as a fallback for environments without hiclaw.
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
#   manage-state.sh --action mark-blocked  --task-id T --reason "..."
#   manage-state.sh --action unblock       --task-id T
#   manage-state.sh --action cancel        --task-id T --reason "..."
#   manage-state.sh --action reassign      --task-id T --assigned-to W --room-id R
#   manage-state.sh --action last-digest   get
#   manage-state.sh --action last-digest   set --at ISO
#   manage-state.sh --action verify        --task-id T   (shell-only; delegates to verify-output.sh)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VERIFY_SCRIPT="${SCRIPT_DIR}/verify-output.sh"

_pre_action=""
_prev=""
for _arg in "$@"; do
    if [ "$_prev" = "--action" ]; then
        _pre_action="$_arg"
        break
    fi
    _prev="$_arg"
done

# verify stays in shell — it does not mutate state.json and is not implemented in hiclaw manager-state.
if [ "${HICLAW_MANAGER_STATE_IMPL:-auto}" != "shell" ] && [ "$_pre_action" != "verify" ]; then
    if command -v hiclaw >/dev/null 2>&1; then
        exec hiclaw manager-state "$@"
    fi
fi

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
  "cancelled_tasks": [],
  "last_digest_sent_at": null,
  "updated_at": "$(_ts)"
}
EOF
    else
        # Backfill fields added over time for pre-existing state files
        local tmp
        tmp=$(mktemp)
        jq '(if has("admin_dm_room_id") then . else . + {admin_dm_room_id: null} end)
            | (if has("cancelled_tasks") then . else . + {cancelled_tasks: []} end)
            | (if has("last_digest_sent_at") then . else . + {last_digest_sent_at: null} end)' \
           "$STATE_FILE" > "$tmp" && mv "$tmp" "$STATE_FILE"
    fi
}

# ─── Actions ─────────────────────────────────────────────────────────────────

action_init() {
    _ensure_state_file
    echo "OK: state.json ready at $STATE_FILE"
}

action_add_finite() {
    _ensure_state_file

    # Exact duplicate (same id + same title + same assignee) is a true SKIP.
    local exact_dup
    exact_dup=$(jq -r --arg id "$TASK_ID" --arg title "$TITLE" --arg worker "$ASSIGNED_TO" \
        '[.active_tasks[] | select(.task_id == $id and .title == $title and .assigned_to == $worker)] | length' \
        "$STATE_FILE")
    if [ "$exact_dup" -gt 0 ]; then
        echo "SKIP: task $TASK_ID already in active_tasks"
        return 0
    fi

    # Same id but different title/assignee: suffix the id (-2, -3, ...) so both survive.
    local final_id="$TASK_ID"
    local existing
    existing=$(jq -r --arg id "$TASK_ID" \
        '[.active_tasks[] | select(.task_id == $id)] | length' "$STATE_FILE")
    if [ "$existing" -gt 0 ]; then
        local suffix=2
        while true; do
            final_id="${TASK_ID}-${suffix}"
            local clash
            clash=$(jq -r --arg id "$final_id" \
                '[.active_tasks[] | select(.task_id == $id)] | length' "$STATE_FILE")
            if [ "$clash" -eq 0 ]; then
                break
            fi
            suffix=$(( suffix + 1 ))
        done
    fi

    local tmp
    tmp=$(mktemp)
    jq --arg id "$final_id" \
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

    if [ "$final_id" != "$TASK_ID" ]; then
        echo "OK: added finite task $final_id \"$TITLE\" (assigned to $ASSIGNED_TO) [id suffixed from $TASK_ID due to collision]"
    else
        echo "OK: added finite task $final_id \"$TITLE\" (assigned to $ASSIGNED_TO)"
    fi
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

action_mark_blocked() {
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
    jq --arg id "$TASK_ID" \
       --arg reason "${REASON:-}" \
       --arg ts "$(_ts)" \
       '(.active_tasks[] | select(.task_id == $id))
        |= (.status = "blocked" | .blocked_since = $ts
            | if $reason != "" then .blocked_reason = $reason else . end)
        | .updated_at = $ts' \
       "$STATE_FILE" > "$tmp" && mv "$tmp" "$STATE_FILE"

    echo "OK: task $TASK_ID marked blocked (reason: ${REASON:-none})"
}

action_unblock() {
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
       '(.active_tasks[] | select(.task_id == $id))
        |= (del(.status, .blocked_since, .blocked_reason))
        | .updated_at = $ts' \
       "$STATE_FILE" > "$tmp" && mv "$tmp" "$STATE_FILE"

    echo "OK: task $TASK_ID unblocked"
}

action_cancel() {
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
    jq --arg id "$TASK_ID" \
       --arg reason "${REASON:-}" \
       --arg ts "$(_ts)" \
       '.cancelled_tasks += [
            (.active_tasks[] | select(.task_id == $id)) + {cancelled_at: $ts, cancel_reason: $reason}
        ]
        | .active_tasks = [.active_tasks[] | select(.task_id != $id)]
        | .updated_at = $ts' \
       "$STATE_FILE" > "$tmp" && mv "$tmp" "$STATE_FILE"

    echo "OK: cancelled task $TASK_ID (reason: ${REASON:-none})"
}

action_reassign() {
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
    jq --arg id "$TASK_ID" \
       --arg worker "$ASSIGNED_TO" \
       --arg room "$ROOM_ID" \
       --arg ts "$(_ts)" \
       '(.active_tasks[] | select(.task_id == $id))
        |= (.assigned_to = $worker | .room_id = $room)
        | .updated_at = $ts' \
       "$STATE_FILE" > "$tmp" && mv "$tmp" "$STATE_FILE"

    echo "OK: reassigned task $TASK_ID to $ASSIGNED_TO (room $ROOM_ID)"
}

action_last_digest() {
    _ensure_state_file

    case "${SUBACTION:-get}" in
        get)
            jq -r '.last_digest_sent_at // "null"' "$STATE_FILE"
            ;;
        set)
            if [ -z "${AT_TS:-}" ]; then
                echo "ERROR: 'last-digest set' requires --at ISO" >&2
                exit 1
            fi
            local tmp
            tmp=$(mktemp)
            jq --arg at "$AT_TS" --arg ts "$(_ts)" \
               '.last_digest_sent_at = $at | .updated_at = $ts' \
               "$STATE_FILE" > "$tmp" && mv "$tmp" "$STATE_FILE"
            echo "OK: last_digest_sent_at set to $AT_TS"
            ;;
        *)
            echo "ERROR: Unknown last-digest subaction '${SUBACTION:-}'. Use: get, set" >&2
            exit 1
            ;;
    esac
}

action_verify() {
    if [ ! -x "$VERIFY_SCRIPT" ] && [ ! -f "$VERIFY_SCRIPT" ]; then
        echo "ERROR: verify-output.sh not found at $VERIFY_SCRIPT" >&2
        exit 1
    fi
    bash "$VERIFY_SCRIPT" --task-id "$TASK_ID"
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

    jq -r '.active_tasks[] | [.task_id, .type, .assigned_to, (.title // "-"), (.status // "-"), (.blocked_since // "-")] | @tsv' "$STATE_FILE" | \
        while IFS=$'\t' read -r tid ttype worker title status blocked_since; do
            if [ "$status" = "blocked" ]; then
                echo "  [BLOCKED since $blocked_since] $tid  type=$ttype  worker=$worker  title=\"$title\""
            else
                echo "  $tid  type=$ttype  worker=$worker  title=\"$title\""
            fi
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
TASK_TYPE=""
SCHEDULE=""
TIMEZONE=""
NEXT_SCHEDULED_AT=""
REASON=""
SUBACTION=""
AT_TS=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --action)           ACTION="$2";            shift 2 ;;
        --task-id)          TASK_ID="$2";           shift 2 ;;
        --title)            TITLE="$2";             shift 2 ;;
        --task-title)       TITLE="$2";             shift 2 ;;
        --assigned-to)      ASSIGNED_TO="$2";       shift 2 ;;
        --room-id)          ROOM_ID="$2";           shift 2 ;;
        --project-room-id)  PROJECT_ROOM_ID="$2";   shift 2 ;;
        --delegated-to-team) DELEGATED_TO_TEAM="$2"; shift 2 ;;
        --type)             TASK_TYPE="$2";         shift 2 ;;
        --schedule)         SCHEDULE="$2";          shift 2 ;;
        --timezone)         TIMEZONE="$2";          shift 2 ;;
        --next-scheduled-at) NEXT_SCHEDULED_AT="$2"; shift 2 ;;
        --reason)           REASON="$2";            shift 2 ;;
        --at)               AT_TS="$2";             shift 2 ;;
        get|set)
            # Positional subaction for `--action last-digest` (plan-specified form:
            # `last-digest get` / `last-digest set <ts>`). Only meaningful there;
            # harmless no-op token otherwise since ACTION dispatch validates it.
            SUBACTION="$1"
            shift
            if [ "$SUBACTION" = "set" ] && [ $# -gt 0 ] && [[ "$1" != --* ]]; then
                AT_TS="$1"
                shift
            fi
            ;;
        *)
            echo "Unknown argument: $1" >&2
            exit 1
            ;;
    esac
done

if [ "$ACTION" = "add" ]; then
    case "${TASK_TYPE:-finite}" in
        finite|"") ACTION="add-finite" ;;
        infinite) ACTION="add-infinite" ;;
        *)
            echo "ERROR: Unknown task type '$TASK_TYPE' for legacy add action. Use: finite, infinite" >&2
            exit 1
            ;;
    esac
fi

if [ -z "$ACTION" ]; then
    echo "Usage: $0 --action <init|add-finite|add-infinite|complete|executed|set-admin-dm|list|mark-blocked|unblock|cancel|reassign|last-digest|verify> [options]" >&2
    echo "" >&2
    echo "Actions:" >&2
    echo "  init          Ensure state.json exists (no-op if already present)" >&2
    echo "  add-finite    --task-id T --title TITLE --assigned-to W --room-id R [--project-room-id P] [--delegated-to-team TEAM]" >&2
    echo "  add-infinite  --task-id T --title TITLE --assigned-to W --room-id R --schedule CRON --timezone TZ --next-scheduled-at ISO" >&2
    echo "  complete      --task-id T   (removes finite task from active_tasks)" >&2
    echo "  executed      --task-id T --next-scheduled-at ISO   (updates infinite task after execution)" >&2
    echo "  set-admin-dm  --room-id R   (saves admin DM room ID for heartbeat use)" >&2
    echo "  list          Show all active tasks" >&2
    echo "  mark-blocked  --task-id T --reason \"...\"   (sets status=blocked + blocked_since)" >&2
    echo "  unblock       --task-id T   (clears blocked status)" >&2
    echo "  cancel        --task-id T --reason \"...\"   (removes task, records reason in cancelled_tasks)" >&2
    echo "  reassign      --task-id T --assigned-to W --room-id R   (swaps assignee/room)" >&2
    echo "  last-digest   get | set --at ISO   (reads/writes last_digest_sent_at)" >&2
    echo "  verify        --task-id T   (checks local task deliverables; JSON stdout; shell-only)" >&2
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
    mark-blocked)
        _validate_required TASK_ID
        action_mark_blocked
        ;;
    unblock)
        _validate_required TASK_ID
        action_unblock
        ;;
    cancel)
        _validate_required TASK_ID
        action_cancel
        ;;
    reassign)
        _validate_required TASK_ID ASSIGNED_TO ROOM_ID
        action_reassign
        ;;
    last-digest)
        action_last_digest
        ;;
    verify)
        _validate_required TASK_ID
        action_verify
        ;;
    *)
        echo "ERROR: Unknown action '$ACTION'. Use: init, add-finite, add-infinite, complete, executed, set-admin-dm, list, mark-blocked, unblock, cancel, reassign, last-digest, verify" >&2
        exit 1
        ;;
esac
