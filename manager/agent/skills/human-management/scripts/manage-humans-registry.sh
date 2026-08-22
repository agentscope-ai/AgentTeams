#!/bin/bash
# manage-humans-registry.sh - CRUD operations for humans-registry.json
#
# Usage:
#   manage-humans-registry.sh --action init
#   manage-humans-registry.sh --action add --name N --matrix-id M --display-name D --level L [--teams t1,t2] [--workers w1,w2] [--note NOTE]
#   manage-humans-registry.sh --action update --name N [--level L] [--teams t1,t2] [--workers w1,w2]
#   manage-humans-registry.sh --action remove --name N
#   manage-humans-registry.sh --action list
#   manage-humans-registry.sh --action get --name N

set -euo pipefail

REGISTRY_FILE="${HOME}/humans-registry.json"

_ts() {
    date -u '+%Y-%m-%dT%H:%M:%SZ'
}

_ensure_registry() {
    if [ ! -f "$REGISTRY_FILE" ]; then
        echo '{"version":1,"updated_at":"","humans":{}}' > "$REGISTRY_FILE"
    fi
}

_csv_to_json_array() {
    local csv="$1"
    if [ -z "$csv" ]; then
        echo "[]"
        return
    fi
    local arr="[]"
    IFS=',' read -ra ITEMS <<< "$csv"
    for item in "${ITEMS[@]}"; do
        item=$(echo "$item" | tr -d ' ')
        [ -z "$item" ] && continue
        arr=$(echo "$arr" | jq --arg v "$item" '. += [$v]')
    done
    echo "$arr"
}

action_init() {
    _ensure_registry
    echo "OK: humans-registry.json ready at $REGISTRY_FILE"
}

action_add() {
    _ensure_registry

    local teams_json
    teams_json=$(_csv_to_json_array "${TEAMS:-}")
    local workers_json
    workers_json=$(_csv_to_json_array "${WORKERS:-}")

    local tmp
    tmp=$(mktemp)
    jq --arg name "$NAME" \
       --arg mid "$MATRIX_ID" \
       --arg dname "${DISPLAY_NAME:-$NAME}" \
       --argjson level "${LEVEL}" \
       --argjson teams "$teams_json" \
       --argjson workers "$workers_json" \
       --arg note "${NOTE:-}" \
       --arg ts "$(_ts)" \
       '.humans[$name] = {
            matrix_user_id: $mid,
            display_name: $dname,
            permission_level: $level,
            accessible_teams: $teams,
            accessible_workers: $workers,
            rooms: [],
            created_at: (if .humans[$name].created_at? then .humans[$name].created_at else $ts end),
            note: (if $note == "" then null else $note end)
        } | .updated_at = $ts' \
       "$REGISTRY_FILE" > "$tmp" && mv "$tmp" "$REGISTRY_FILE"

    echo "OK: added human $NAME (level=$LEVEL)"
}

action_update() {
    _ensure_registry

    local exists
    exists=$(jq -r --arg n "$NAME" '.humans[$n] // empty' "$REGISTRY_FILE")
    if [ -z "$exists" ]; then
        echo "ERROR: human $NAME not found" >&2
        exit 1
    fi

    local tmp
    tmp=$(mktemp)
    local filter=". "

    if [ -n "${LEVEL:-}" ]; then
        filter="${filter} | .humans[\"$NAME\"].permission_level = ${LEVEL}"
    fi
    if [ -n "${TEAMS:-}" ]; then
        local teams_json
        teams_json=$(_csv_to_json_array "$TEAMS")
        filter="${filter} | .humans[\"$NAME\"].accessible_teams = ${teams_json}"
    fi
    if [ -n "${WORKERS:-}" ]; then
        local workers_json
        workers_json=$(_csv_to_json_array "$WORKERS")
        filter="${filter} | .humans[\"$NAME\"].accessible_workers = ${workers_json}"
    fi

    filter="${filter} | .updated_at = \"$(_ts)\""

    jq "$filter" "$REGISTRY_FILE" > "$tmp" && mv "$tmp" "$REGISTRY_FILE"
    echo "OK: updated human $NAME"
}

action_remove() {
    _ensure_registry

    local tmp
    tmp=$(mktemp)
    jq --arg n "$NAME" --arg ts "$(_ts)" \
       'del(.humans[$n]) | .updated_at = $ts' \
       "$REGISTRY_FILE" > "$tmp" && mv "$tmp" "$REGISTRY_FILE"

    echo "OK: removed human $NAME"
}

action_list() {
    _ensure_registry
    jq -r '.humans | to_entries[] | "\(.key)  level=\(.value.permission_level)  display=\(.value.display_name)  teams=\(.value.accessible_teams // [] | join(","))  workers=\(.value.accessible_workers // [] | join(","))"' "$REGISTRY_FILE"
    local count
    count=$(jq '.humans | length' "$REGISTRY_FILE")
    echo "Total: $count human(s)"
}

action_get() {
    _ensure_registry
    jq --arg n "$NAME" '.humans[$n] // "not found"' "$REGISTRY_FILE"
}

# ─── Argument parsing ─────────────────────────────────────────────────────────

ACTION=""
NAME=""
MATRIX_ID=""
DISPLAY_NAME=""
LEVEL=""
TEAMS=""
WORKERS=""
NOTE=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --action)        ACTION="$2";        shift 2 ;;
        --name)          NAME="$2";          shift 2 ;;
        --matrix-id)     MATRIX_ID="$2";     shift 2 ;;
        --display-name)  DISPLAY_NAME="$2";  shift 2 ;;
        --level)         LEVEL="$2";         shift 2 ;;
        --teams)         TEAMS="$2";         shift 2 ;;
        --workers)       WORKERS="$2";       shift 2 ;;
        --note)          NOTE="$2";          shift 2 ;;
        *)
            echo "Unknown argument: $1" >&2
            exit 1
            ;;
    esac
done

if [ -z "$ACTION" ]; then
    echo "Usage: $0 --action <init|add|update|remove|list|get> [options]" >&2
    exit 1
fi

case "$ACTION" in
    init)     action_init ;;
    add)      action_add ;;
    update)   action_update ;;
    remove)   action_remove ;;
    list)     action_list ;;
    get)      action_get ;;
    *)
        echo "ERROR: Unknown action '$ACTION'" >&2
        exit 1
        ;;
esac
