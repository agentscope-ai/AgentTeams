#!/bin/bash
# manage-teams-registry.sh - CRUD operations for teams-registry.json
#
# Usage:
#   manage-teams-registry.sh --action init
#   manage-teams-registry.sh --action add --team-name T --leader L --workers w1,w2 --team-room-id R
#   manage-teams-registry.sh --action add-worker --team-name T --worker W
#   manage-teams-registry.sh --action remove-worker --team-name T --worker W
#   manage-teams-registry.sh --action remove --team-name T
#   manage-teams-registry.sh --action list
#   manage-teams-registry.sh --action get --team-name T

set -euo pipefail

REGISTRY_FILE="${HOME}/teams-registry.json"

_ts() {
    date -u '+%Y-%m-%dT%H:%M:%SZ'
}

_ensure_registry() {
    if [ ! -f "$REGISTRY_FILE" ]; then
        echo '{"version":1,"updated_at":"","teams":{}}' > "$REGISTRY_FILE"
    fi
}

action_init() {
    _ensure_registry
    echo "OK: teams-registry.json ready at $REGISTRY_FILE"
}

action_add() {
    _ensure_registry

    # Parse workers comma-separated into JSON array
    local workers_json="[]"
    IFS=',' read -ra WARR <<< "${WORKERS}"
    for w in "${WARR[@]}"; do
        w=$(echo "$w" | tr -d ' ')
        [ -z "$w" ] && continue
        workers_json=$(echo "$workers_json" | jq --arg w "$w" '. += [$w]')
    done

    local tmp
    tmp=$(mktemp)
    jq --arg name "$TEAM_NAME" \
       --arg leader "$LEADER" \
       --argjson workers "$workers_json" \
       --arg room_id "${TEAM_ROOM_ID:-}" \
       --arg admin_name "${TEAM_ADMIN:-}" \
       --arg admin_matrix_id "${TEAM_ADMIN_MATRIX_ID:-}" \
       --arg leader_dm_room_id "${LEADER_DM_ROOM_ID:-}" \
       --arg ts "$(_ts)" \
       '.teams[$name] = {
            leader: $leader,
            workers: $workers,
            team_room_id: (if $room_id == "" then null else $room_id end),
            admin: (if $admin_name == "" then null else {name: $admin_name, matrix_user_id: (if $admin_matrix_id == "" then null else $admin_matrix_id end)} end),
            leader_dm_room_id: (if $leader_dm_room_id == "" then null else $leader_dm_room_id end),
            created_at: (if .teams[$name].created_at? then .teams[$name].created_at else $ts end)
        } | .updated_at = $ts' \
       "$REGISTRY_FILE" > "$tmp" && mv "$tmp" "$REGISTRY_FILE"

    echo "OK: added team $TEAM_NAME (leader=$LEADER, workers=${WORKERS}, admin=${TEAM_ADMIN:-none})"
}

action_add_worker() {
    _ensure_registry

    local exists
    exists=$(jq -r --arg t "$TEAM_NAME" '.teams[$t] // empty' "$REGISTRY_FILE")
    if [ -z "$exists" ]; then
        echo "ERROR: team $TEAM_NAME not found" >&2
        exit 1
    fi

    local tmp
    tmp=$(mktemp)
    jq --arg t "$TEAM_NAME" --arg w "$WORKER" --arg ts "$(_ts)" \
       'if (.teams[$t].workers | index($w)) then .
        else .teams[$t].workers += [$w] | .updated_at = $ts
        end' \
       "$REGISTRY_FILE" > "$tmp" && mv "$tmp" "$REGISTRY_FILE"

    echo "OK: added worker $WORKER to team $TEAM_NAME"
}

action_remove_worker() {
    _ensure_registry

    local tmp
    tmp=$(mktemp)
    jq --arg t "$TEAM_NAME" --arg w "$WORKER" --arg ts "$(_ts)" \
       '.teams[$t].workers = [.teams[$t].workers[] | select(. != $w)]
        | .updated_at = $ts' \
       "$REGISTRY_FILE" > "$tmp" && mv "$tmp" "$REGISTRY_FILE"

    echo "OK: removed worker $WORKER from team $TEAM_NAME"
}

action_remove() {
    _ensure_registry

    local tmp
    tmp=$(mktemp)
    jq --arg t "$TEAM_NAME" --arg ts "$(_ts)" \
       'del(.teams[$t]) | .updated_at = $ts' \
       "$REGISTRY_FILE" > "$tmp" && mv "$tmp" "$REGISTRY_FILE"

    echo "OK: removed team $TEAM_NAME"
}

action_list() {
    _ensure_registry
    jq -r '.teams | to_entries[] | "\(.key)  leader=\(.value.leader)  workers=\(.value.workers | join(","))  room=\(.value.team_room_id // "none")"' "$REGISTRY_FILE"
    local count
    count=$(jq '.teams | length' "$REGISTRY_FILE")
    echo "Total: $count team(s)"
}

action_get() {
    _ensure_registry
    jq --arg t "$TEAM_NAME" '.teams[$t] // "not found"' "$REGISTRY_FILE"
}

# ─── Argument parsing ─────────────────────────────────────────────────────────

ACTION=""
TEAM_NAME=""
LEADER=""
WORKERS=""
WORKER=""
TEAM_ROOM_ID=""
TEAM_ADMIN=""
TEAM_ADMIN_MATRIX_ID=""
LEADER_DM_ROOM_ID=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --action)              ACTION="$2";              shift 2 ;;
        --team-name)           TEAM_NAME="$2";           shift 2 ;;
        --leader)              LEADER="$2";              shift 2 ;;
        --workers)             WORKERS="$2";             shift 2 ;;
        --worker)              WORKER="$2";              shift 2 ;;
        --team-room-id)        TEAM_ROOM_ID="$2";        shift 2 ;;
        --team-admin)          TEAM_ADMIN="$2";          shift 2 ;;
        --team-admin-matrix-id) TEAM_ADMIN_MATRIX_ID="$2"; shift 2 ;;
        --leader-dm-room-id)   LEADER_DM_ROOM_ID="$2";  shift 2 ;;
        *)
            echo "Unknown argument: $1" >&2
            exit 1
            ;;
    esac
done

if [ -z "$ACTION" ]; then
    echo "Usage: $0 --action <init|add|add-worker|remove-worker|remove|list|get> [options]" >&2
    exit 1
fi

case "$ACTION" in
    init)           action_init ;;
    add)            action_add ;;
    add-worker)     action_add_worker ;;
    remove-worker)  action_remove_worker ;;
    remove)         action_remove ;;
    list)           action_list ;;
    get)            action_get ;;
    *)
        echo "ERROR: Unknown action '$ACTION'" >&2
        exit 1
        ;;
esac
