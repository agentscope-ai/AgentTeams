#!/bin/bash
# manage-team-state.sh - Atomic team-state.json operations for team task & project tracking
#
# Same interface as Manager's manage-state.sh but operates on ~/team-state.json.
#
# Usage:
#   manage-team-state.sh --action init
#   manage-team-state.sh --action add-finite    --task-id T --title TITLE --assigned-to W --room-id R [--source S] [--parent-task-id P] [--requester R]
#   manage-team-state.sh --action complete      --task-id T
#   manage-team-state.sh --action list
#   manage-team-state.sh --action add-project       --project-id P --title TITLE [--source S] [--parent-task-id T] [--requester R]
#   manage-team-state.sh --action complete-project  --project-id P
#   manage-team-state.sh --action list-projects

set -euo pipefail

# Resolve state file path — try working dir first, then HOME
_resolve_state_file() {
    for _candidate in "./team-state.json" "../team-state.json" "${HOME}/team-state.json"; do
        if [ -f "${_candidate}" ]; then
            echo "${_candidate}"
            return
        fi
    done
    # Default: create in current dir or parent (whichever has AGENTS.md)
    if [ -f "../AGENTS.md" ]; then
        echo "../team-state.json"
    else
        echo "./team-state.json"
    fi
}

STATE_FILE="$(_resolve_state_file)"

_ts() {
    date -u '+%Y-%m-%dT%H:%M:%SZ'
}

_ensure_state_file() {
    if [ ! -f "$STATE_FILE" ]; then
        cat > "$STATE_FILE" << EOF
{
  "team_id": null,
  "active_tasks": [],
  "active_projects": [],
  "updated_at": "$(_ts)"
}
EOF
    else
        # Migrate: add active_projects if missing (backward compat)
        if ! jq -e '.active_projects' "$STATE_FILE" > /dev/null 2>&1; then
            local tmp
            tmp=$(mktemp)
            jq '. + {active_projects: []}' "$STATE_FILE" > "$tmp" && mv "$tmp" "$STATE_FILE"
        fi
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
       --arg source "${SOURCE:-}" \
       --arg parent "${PARENT_TASK_ID:-}" \
       --arg requester "${REQUESTER:-}" \
       --arg ts "$(_ts)" \
       '.active_tasks += [{
            task_id: $id,
            title: $title,
            type: "finite",
            assigned_to: $worker,
            room_id: $room
        } + (if $source != "" then {source: $source} else {} end)
          + (if $parent != "" then {parent_task_id: $parent} else {} end)
          + (if $requester != "" then {requester: $requester} else {} end)]
        | .updated_at = $ts' \
       "$STATE_FILE" > "$tmp" && mv "$tmp" "$STATE_FILE"

    echo "OK: added sub-task $TASK_ID \"$TITLE\" (assigned to $ASSIGNED_TO, source=${SOURCE:-unset})"
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
    else
        jq -r '.active_tasks[] | [.task_id, .type, .assigned_to, (.source // "-"), (.title // "-")] | @tsv' "$STATE_FILE" | \
            while IFS=$'\t' read -r tid ttype worker src title; do
                echo "  $tid  type=$ttype  worker=$worker  source=$src  title=\"$title\""
            done
        echo "Total: $count active sub-task(s)."
    fi

    local pcount
    pcount=$(jq '.active_projects | length' "$STATE_FILE")
    if [ "$pcount" -gt 0 ]; then
        echo ""
        echo "Active projects:"
        jq -r '.active_projects[] | [.project_id, .status, (.source // "-"), (.title // "-")] | @tsv' "$STATE_FILE" | \
            while IFS=$'\t' read -r pid pstatus src title; do
                echo "  $pid  status=$pstatus  source=$src  title=\"$title\""
            done
        echo "Total: $pcount active project(s)."
    fi

    echo "Updated: $(jq -r '.updated_at' "$STATE_FILE")"
}

action_add_project() {
    _ensure_state_file

    local existing
    existing=$(jq -r --arg id "$PROJECT_ID" \
        '[.active_projects[] | select(.project_id == $id)] | length' "$STATE_FILE")
    if [ "$existing" -gt 0 ]; then
        echo "SKIP: project $PROJECT_ID already in active_projects"
        return 0
    fi

    local tmp
    tmp=$(mktemp)
    jq --arg id "$PROJECT_ID" \
       --arg title "$TITLE" \
       --arg source "${SOURCE:-}" \
       --arg parent "${PARENT_TASK_ID:-}" \
       --arg requester "${REQUESTER:-}" \
       --arg ts "$(_ts)" \
       '.active_projects += [{
            project_id: $id,
            title: $title,
            status: "active"
        } + (if $source != "" then {source: $source} else {} end)
          + (if $parent != "" then {parent_task_id: $parent} else {} end)
          + (if $requester != "" then {requester: $requester} else {} end)]
        | .updated_at = $ts' \
       "$STATE_FILE" > "$tmp" && mv "$tmp" "$STATE_FILE"

    echo "OK: added project $PROJECT_ID \"$TITLE\" (source=${SOURCE:-unset})"
}

action_complete_project() {
    _ensure_state_file

    local existing
    existing=$(jq -r --arg id "$PROJECT_ID" \
        '[.active_projects[] | select(.project_id == $id)] | length' "$STATE_FILE")
    if [ "$existing" -eq 0 ]; then
        echo "SKIP: project $PROJECT_ID not found in active_projects"
        return 0
    fi

    local tmp
    tmp=$(mktemp)
    jq --arg id "$PROJECT_ID" --arg ts "$(_ts)" \
       '.active_projects = [.active_projects[] | select(.project_id != $id)]
        | .updated_at = $ts' \
       "$STATE_FILE" > "$tmp" && mv "$tmp" "$STATE_FILE"

    echo "OK: removed project $PROJECT_ID from active_projects"
}

action_list_projects() {
    _ensure_state_file

    local count
    count=$(jq '.active_projects | length' "$STATE_FILE")
    if [ "$count" -eq 0 ]; then
        echo "No active team projects."
        return 0
    fi

    jq -r '.active_projects[] | [.project_id, .status, (.source // "-"), (.parent_task_id // "-"), (.title // "-")] | @tsv' "$STATE_FILE" | \
        while IFS=$'\t' read -r pid pstatus src parent title; do
            echo "  $pid  status=$pstatus  source=$src  parent=$parent  title=\"$title\""
        done
    echo "Total: $count active project(s). Updated: $(jq -r '.updated_at' "$STATE_FILE")"
}

# ─── Argument parsing ─────────────────────────────────────────────────────────

ACTION=""
TASK_ID=""
TITLE=""
ASSIGNED_TO=""
ROOM_ID=""
PROJECT_ID=""
SOURCE=""
PARENT_TASK_ID=""
REQUESTER=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --action)          ACTION="$2";          shift 2 ;;
        --task-id)         TASK_ID="$2";         shift 2 ;;
        --title)           TITLE="$2";           shift 2 ;;
        --assigned-to)     ASSIGNED_TO="$2";     shift 2 ;;
        --room-id)         ROOM_ID="$2";         shift 2 ;;
        --project-id)      PROJECT_ID="$2";      shift 2 ;;
        --source)          SOURCE="$2";          shift 2 ;;
        --parent-task-id)  PARENT_TASK_ID="$2";  shift 2 ;;
        --requester)       REQUESTER="$2";       shift 2 ;;
        *)
            echo "Unknown argument: $1" >&2
            exit 1
            ;;
    esac
done

if [ -z "$ACTION" ]; then
    echo "Usage: $0 --action <init|add-finite|complete|list|add-project|complete-project|list-projects> [options]" >&2
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
    add-project)
        _validate_required PROJECT_ID TITLE
        action_add_project ;;
    complete-project)
        _validate_required PROJECT_ID
        action_complete_project ;;
    list-projects)
        action_list_projects ;;
    *)
        echo "ERROR: Unknown action '$ACTION'. Use: init, add-finite, complete, list, add-project, complete-project, list-projects" >&2
        exit 1
        ;;
esac
