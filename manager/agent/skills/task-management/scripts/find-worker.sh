#!/bin/bash
# find-worker.sh - Find suitable Workers for a task
#
# Consolidates information from multiple sources into a single view:
#   - ~/workers-registry.json   (skills, runtime, deployment, room_id)
#   - ~/state.json              (active tasks per worker)
#   - ~/worker-lifecycle.json   (container status, idle time)
#   - /root/hiclaw-fs/agents/<name>/SOUL.md  (role description)
#
# Usage:
#   find-worker.sh                          # list all workers with availability
#   find-worker.sh --skills github-operations,git-delegation   # filter by skills
#   find-worker.sh --worker alice           # show details for a specific worker
#   find-worker.sh --team alpha-team        # filter by team membership
#
# Output: JSON with worker availability, workload, container status, and role.
# The output is designed for the Manager Agent's Step 0 decision.

set -euo pipefail

REGISTRY_FILE="${HOME}/workers-registry.json"
STATE_FILE="${HOME}/state.json"
LIFECYCLE_FILE="${HOME}/worker-lifecycle.json"
AGENTS_DIR="/root/hiclaw-fs/agents"

# ─── Parse arguments ──────────────────────────────────────────────────────────

FILTER_SKILLS=""
FILTER_WORKER=""
FILTER_TEAM=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --skills)       FILTER_SKILLS="$2"; shift 2 ;;
        --worker)       FILTER_WORKER="$2"; shift 2 ;;
        --team)         FILTER_TEAM="$2"; shift 2 ;;
        *)
            echo "Usage: $0 [--skills s1,s2] [--worker <name>] [--team <team-name>]" >&2
            exit 1
            ;;
    esac
done

# ─── Validate data sources ───────────────────────────────────────────────────

if [ ! -f "$REGISTRY_FILE" ]; then
    echo '{"error":"workers-registry.json not found — no workers registered yet","workers":[]}'
    exit 0
fi

WORKER_COUNT=$(jq '.workers | length' "$REGISTRY_FILE" 2>/dev/null || echo 0)
if [ "$WORKER_COUNT" -eq 0 ]; then
    echo '{"error":"no workers in registry","workers":[]}'
    exit 0
fi

# ─── Build worker list ───────────────────────────────────────────────────────

# Collect all worker names (optionally filtered)
if [ -n "$FILTER_WORKER" ]; then
    WORKER_NAMES=$(jq -r --arg w "$FILTER_WORKER" '.workers | keys[] | select(. == $w)' "$REGISTRY_FILE")
    if [ -z "$WORKER_NAMES" ]; then
        echo '{"error":"worker '"$FILTER_WORKER"' not found in registry","workers":[]}'
        exit 0
    fi
else
    WORKER_NAMES=$(jq -r '.workers | keys[]' "$REGISTRY_FILE")
fi

# ─── Helper: extract role from SOUL.md ────────────────────────────────────────
# Supports both English "## Role" and Chinese "## 角色" headings.
# Returns all non-empty lines between the heading and the next "##", joined by " | ".

_get_role() {
    local name="$1"
    local soul="${AGENTS_DIR}/${name}/SOUL.md"
    if [ ! -f "$soul" ]; then
        echo ""
        return
    fi
    awk '/^## (Role|角色)/{found=1; next} /^## /{if(found) exit} found && NF{print}' "$soul" 2>/dev/null \
        | sed 's/^- //' | paste -sd '|' - | sed 's/|/ | /g' || echo ""
}

# ─── Build JSON output ───────────────────────────────────────────────────────

RESULTS="[]"

for worker in $WORKER_NAMES; do
    # --- Registry info ---
    REG_ENTRY=$(jq --arg w "$worker" '.workers[$w]' "$REGISTRY_FILE")
    SKILLS=$(echo "$REG_ENTRY" | jq -r '.skills // []')
    RUNTIME=$(echo "$REG_ENTRY" | jq -r '.runtime // "openclaw"')
    DEPLOYMENT=$(echo "$REG_ENTRY" | jq -r '.deployment // "local"')
    ROOM_ID=$(echo "$REG_ENTRY" | jq -r '.room_id // ""')
    WORKER_ROLE=$(echo "$REG_ENTRY" | jq -r '.role // "worker"')
    WORKER_TEAM_ID=$(echo "$REG_ENTRY" | jq -r '.team_id // ""')

    # --- Team filter ---
    if [ -n "$FILTER_TEAM" ] && [ "$WORKER_TEAM_ID" != "$FILTER_TEAM" ]; then
        continue
    fi

    # --- Skills filter ---
    if [ -n "$FILTER_SKILLS" ]; then
        MATCH=true
        IFS=',' read -ra REQUIRED <<< "$FILTER_SKILLS"
        for req in "${REQUIRED[@]}"; do
            req=$(echo "$req" | tr -d ' ')
            [ -z "$req" ] && continue
            HAS=$(echo "$SKILLS" | jq -r --arg s "$req" 'map(select(. == $s)) | length')
            if [ "$HAS" -eq 0 ]; then
                MATCH=false
                break
            fi
        done
        if [ "$MATCH" = false ]; then
            continue
        fi
    fi

    # --- Active tasks from state.json ---
    FINITE_TASKS=0
    INFINITE_TASKS=0
    TASK_TITLES="[]"
    if [ -f "$STATE_FILE" ]; then
        FINITE_TASKS=$(jq -r --arg w "$worker" \
            '[.active_tasks[] | select(.assigned_to == $w and .type == "finite")] | length' \
            "$STATE_FILE" 2>/dev/null || echo 0)
        INFINITE_TASKS=$(jq -r --arg w "$worker" \
            '[.active_tasks[] | select(.assigned_to == $w and .type == "infinite")] | length' \
            "$STATE_FILE" 2>/dev/null || echo 0)
        # Support both "task_id" and "id" field names for compatibility
        TASK_TITLES=$(jq --arg w "$worker" \
            '[.active_tasks[] | select(.assigned_to == $w) | {task_id: (.task_id // .id), type, title}]' \
            "$STATE_FILE" 2>/dev/null || echo "[]")
    fi

    # --- Container status from lifecycle ---
    CONTAINER_STATUS="unknown"
    IDLE_SINCE="null"
    if [ -f "$LIFECYCLE_FILE" ]; then
        CONTAINER_STATUS=$(jq -r --arg w "$worker" \
            '.workers[$w].container_status // "unknown"' "$LIFECYCLE_FILE" 2>/dev/null || echo "unknown")
        IDLE_SINCE=$(jq -r --arg w "$worker" \
            '.workers[$w].idle_since // "null"' "$LIFECYCLE_FILE" 2>/dev/null || echo "null")
    fi
    # For remote workers, override from registry
    if [ "$DEPLOYMENT" = "remote" ]; then
        CONTAINER_STATUS="remote"
    fi

    # --- Role from SOUL.md ---
    ROLE=$(_get_role "$worker")

    # --- Availability verdict ---
    # idle = running + no finite tasks
    # busy = has finite tasks
    # stopped = container stopped (needs wake-up before assign)
    # unavailable = not_found (needs recreate)
    # remote = remotely deployed (always assumed available)
    AVAILABILITY="idle"
    if [ "$CONTAINER_STATUS" = "remote" ]; then
        if [ "$FINITE_TASKS" -gt 0 ]; then
            AVAILABILITY="busy"
        else
            AVAILABILITY="idle"
        fi
    elif [ "$CONTAINER_STATUS" = "not_found" ]; then
        AVAILABILITY="unavailable"
    elif [ "$CONTAINER_STATUS" = "stopped" ] || [ "$CONTAINER_STATUS" = "exited" ]; then
        AVAILABILITY="stopped"
    elif [ "$FINITE_TASKS" -gt 0 ]; then
        AVAILABILITY="busy"
    fi

    # --- Assemble worker JSON ---
    WORKER_JSON=$(jq -n \
        --arg name "$worker" \
        --arg availability "$AVAILABILITY" \
        --arg container_status "$CONTAINER_STATUS" \
        --arg runtime "$RUNTIME" \
        --arg deployment "$DEPLOYMENT" \
        --arg room_id "$ROOM_ID" \
        --arg role "$ROLE" \
        --arg worker_role "$WORKER_ROLE" \
        --arg team_id "$WORKER_TEAM_ID" \
        --arg idle_since "$IDLE_SINCE" \
        --argjson skills "$SKILLS" \
        --argjson finite_tasks "$FINITE_TASKS" \
        --argjson infinite_tasks "$INFINITE_TASKS" \
        --argjson active_tasks "$TASK_TITLES" \
        '{
            name: $name,
            availability: $availability,
            role: (if $role == "" then null else $role end),
            worker_role: $worker_role,
            team_id: (if $team_id == "" then null else $team_id end),
            skills: $skills,
            runtime: $runtime,
            deployment: $deployment,
            container_status: $container_status,
            finite_tasks: $finite_tasks,
            infinite_tasks: $infinite_tasks,
            active_tasks: $active_tasks,
            idle_since: (if $idle_since == "null" then null else $idle_since end),
            room_id: $room_id
        }')

    RESULTS=$(echo "$RESULTS" | jq --argjson w "$WORKER_JSON" '. += [$w]')
done

# ─── Summary ─────────────────────────────────────────────────────────────────

TOTAL=$(echo "$RESULTS" | jq 'length')
IDLE_COUNT=$(echo "$RESULTS" | jq '[.[] | select(.availability == "idle")] | length')
BUSY_COUNT=$(echo "$RESULTS" | jq '[.[] | select(.availability == "busy")] | length')
STOPPED_COUNT=$(echo "$RESULTS" | jq '[.[] | select(.availability == "stopped")] | length')
UNAVAILABLE_COUNT=$(echo "$RESULTS" | jq '[.[] | select(.availability == "unavailable")] | length')

jq -n \
    --argjson workers "$RESULTS" \
    --argjson total "$TOTAL" \
    --argjson idle "$IDLE_COUNT" \
    --argjson busy "$BUSY_COUNT" \
    --argjson stopped "$STOPPED_COUNT" \
    --argjson unavailable "$UNAVAILABLE_COUNT" \
    '{
        summary: {
            total: $total,
            idle: $idle,
            busy: $busy,
            stopped: $stopped,
            unavailable: $unavailable
        },
        workers: $workers
    }'
