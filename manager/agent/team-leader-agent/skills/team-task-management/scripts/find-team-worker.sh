#!/bin/bash
# find-team-worker.sh - Find available workers within the team
#
# Simplified version of Manager's find-worker.sh, scoped to team members only.
# Reads team worker list from team-state.json and workers-registry.json.
#
# Usage:
#   find-team-worker.sh                    # list all team workers
#   find-team-worker.sh --worker alice     # show specific worker

set -euo pipefail

REGISTRY_FILE="${HOME}/workers-registry.json"
STATE_FILE="${HOME}/team-state.json"
AGENTS_DIR="/root/hiclaw-fs/agents"

# If no local registry, try to read from shared MinIO path
if [ ! -f "$REGISTRY_FILE" ]; then
    # Team leaders get a copy of the registry via file-sync
    echo '{"error":"workers-registry.json not found — run file-sync first","workers":[]}'
    exit 0
fi

FILTER_WORKER=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --worker) FILTER_WORKER="$2"; shift 2 ;;
        *) echo "Usage: $0 [--worker <name>]" >&2; exit 1 ;;
    esac
done

# Get team_id from team-state.json
TEAM_ID=""
if [ -f "$STATE_FILE" ]; then
    TEAM_ID=$(jq -r '.team_id // ""' "$STATE_FILE" 2>/dev/null)
fi

# Get team workers: filter by team_id in registry
if [ -n "$FILTER_WORKER" ]; then
    WORKER_NAMES=$(jq -r --arg w "$FILTER_WORKER" '.workers | keys[] | select(. == $w)' "$REGISTRY_FILE")
else
    if [ -n "$TEAM_ID" ]; then
        WORKER_NAMES=$(jq -r --arg t "$TEAM_ID" '.workers | to_entries[] | select(.value.team_id == $t and .value.role != "team_leader") | .key' "$REGISTRY_FILE")
    else
        # Fallback: list all workers (shouldn't happen in normal operation)
        WORKER_NAMES=$(jq -r '.workers | keys[]' "$REGISTRY_FILE")
    fi
fi

if [ -z "$WORKER_NAMES" ]; then
    echo '{"error":"no team workers found","workers":[]}'
    exit 0
fi

_get_role() {
    local name="$1"
    local soul="${AGENTS_DIR}/${name}/SOUL.md"
    if [ ! -f "$soul" ]; then echo ""; return; fi
    awk '/^## (Role|角色)/{found=1; next} /^## /{if(found) exit} found && NF{print}' "$soul" 2>/dev/null \
        | sed 's/^- //' | paste -sd '|' - | sed 's/|/ | /g' || echo ""
}

RESULTS="[]"

for worker in $WORKER_NAMES; do
    REG_ENTRY=$(jq --arg w "$worker" '.workers[$w]' "$REGISTRY_FILE")
    SKILLS=$(echo "$REG_ENTRY" | jq -r '.skills // []')
    ROOM_ID=$(echo "$REG_ENTRY" | jq -r '.room_id // ""')
    DEPLOYMENT=$(echo "$REG_ENTRY" | jq -r '.deployment // "local"')

    # Active tasks from team-state.json
    FINITE_TASKS=0
    if [ -f "$STATE_FILE" ]; then
        FINITE_TASKS=$(jq -r --arg w "$worker" \
            '[.active_tasks[] | select(.assigned_to == $w and .type == "finite")] | length' \
            "$STATE_FILE" 2>/dev/null || echo 0)
    fi

    ROLE=$(_get_role "$worker")

    # Availability
    AVAILABILITY="idle"
    if [ "$FINITE_TASKS" -gt 0 ]; then
        AVAILABILITY="busy"
    fi

    WORKER_JSON=$(jq -n \
        --arg name "$worker" \
        --arg availability "$AVAILABILITY" \
        --arg room_id "$ROOM_ID" \
        --arg role "$ROLE" \
        --argjson skills "$SKILLS" \
        --argjson finite_tasks "$FINITE_TASKS" \
        '{
            name: $name,
            availability: $availability,
            role: (if $role == "" then null else $role end),
            skills: $skills,
            finite_tasks: $finite_tasks,
            room_id: $room_id
        }')

    RESULTS=$(echo "$RESULTS" | jq --argjson w "$WORKER_JSON" '. += [$w]')
done

TOTAL=$(echo "$RESULTS" | jq 'length')
IDLE_COUNT=$(echo "$RESULTS" | jq '[.[] | select(.availability == "idle")] | length')
BUSY_COUNT=$(echo "$RESULTS" | jq '[.[] | select(.availability == "busy")] | length')

jq -n \
    --argjson workers "$RESULTS" \
    --argjson total "$TOTAL" \
    --argjson idle "$IDLE_COUNT" \
    --argjson busy "$BUSY_COUNT" \
    '{
        summary: { total: $total, idle: $idle, busy: $busy },
        workers: $workers
    }'
