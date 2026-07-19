#!/bin/bash
# worker-health-report.sh - Classify worker health states from existing data sources
#
# Reads from:
#   - worker-lifecycle.json (container_status, idle_since)
#   - state.json (active tasks per worker, blocked status)
#   - hiclaw get workers -o json (phase, lastHeartbeat, lastActiveAt)
#
# Outputs JSON health classification per worker.
#
# Usage:
#   worker-health-report.sh [--stalled-threshold-min 60] [--zombie-threshold-min 15]

set -euo pipefail

LIFECYCLE_FILE="${HOME}/worker-lifecycle.json"
STATE_FILE="${HOME}/state.json"

# Default thresholds (minutes)
STALLED_THRESHOLD_MIN=60
ZOMBIE_THRESHOLD_MIN=15

# Parse args
while [[ $# -gt 0 ]]; do
    case "$1" in
        --stalled-threshold-min) STALLED_THRESHOLD_MIN="$2"; shift 2 ;;
        --zombie-threshold-min)  ZOMBIE_THRESHOLD_MIN="$2"; shift 2 ;;
        *) echo "Unknown option: $1" >&2; exit 1 ;;
    esac
done

_now_epoch() {
    date -u '+%s'
}

_iso_to_epoch() {
    local iso="$1"
    if [ -z "$iso" ] || [ "$iso" = "null" ]; then
        echo "0"
        return
    fi
    # Try GNU date first, then BSD date
    date -u -d "$iso" '+%s' 2>/dev/null || \
    date -u -j -f '%Y-%m-%dT%H:%M:%SZ' "$iso" '+%s' 2>/dev/null || \
    echo "0"
}

# ─── Gather data ─────────────────────────────────────────────────────────────

# Get workers from controller API
WORKERS_JSON="[]"
if command -v hiclaw &>/dev/null; then
    WORKERS_JSON=$(hiclaw get workers -o json 2>/dev/null | jq -c '.workers // []' 2>/dev/null || echo "[]")
fi

# Get lifecycle data
LIFECYCLE_JSON="{}"
if [ -f "$LIFECYCLE_FILE" ]; then
    LIFECYCLE_JSON=$(jq -c '.' "$LIFECYCLE_FILE" 2>/dev/null || echo "{}")
fi

# Get task state
TASKS_JSON="[]"
if [ -f "$STATE_FILE" ]; then
    TASKS_JSON=$(jq -c '.active_tasks // []' "$STATE_FILE" 2>/dev/null || echo "[]")
fi

NOW=$(_now_epoch)

# ─── Classify each worker ────────────────────────────────────────────────────

RESULT_WORKERS="{}"
SUMMARY_WORKING=0
SUMMARY_STALLED=0
SUMMARY_ZOMBIE=0
SUMMARY_IDLE=0
SUMMARY_BLOCKED=0
SUMMARY_STOPPED=0

worker_count=$(echo "$WORKERS_JSON" | jq 'length')

for ((i=0; i<worker_count; i++)); do
    worker=$(echo "$WORKERS_JSON" | jq -c ".[$i]")
    name=$(echo "$worker" | jq -r '.name // .workerName // "unknown"')
    phase=$(echo "$worker" | jq -r '.phase // "Unknown"')
    last_active=$(echo "$worker" | jq -r '.lastActiveAt // ""')
    last_heartbeat=$(echo "$worker" | jq -r '.lastHeartbeat // ""')

    # Get container status from lifecycle file
    container_status=$(echo "$LIFECYCLE_JSON" | jq -r --arg n "$name" '.[$n].container_status // "unknown"')

    # Count active tasks for this worker
    active_task_count=$(echo "$TASKS_JSON" | jq --arg w "$name" '[.[] | select(.assigned_to == $w and .type == "finite")] | length')
    blocked_task_count=$(echo "$TASKS_JSON" | jq --arg w "$name" '[.[] | select(.assigned_to == $w and .status == "blocked")] | length')

    # Calculate staleness
    last_active_epoch=$(_iso_to_epoch "$last_active")
    last_heartbeat_epoch=$(_iso_to_epoch "$last_heartbeat")

    mins_since_active=999999
    if [ "$last_active_epoch" -gt 0 ]; then
        mins_since_active=$(( (NOW - last_active_epoch) / 60 ))
    fi

    mins_since_heartbeat=999999
    if [ "$last_heartbeat_epoch" -gt 0 ]; then
        mins_since_heartbeat=$(( (NOW - last_heartbeat_epoch) / 60 ))
    fi

    # Classification logic
    health="unknown"
    detail=""
    since="$last_active"

    # Stopped: not running
    if [ "$phase" != "Running" ]; then
        health="stopped"
        detail="phase=$phase"
        SUMMARY_STOPPED=$((SUMMARY_STOPPED + 1))
    # Blocked: has blocked tasks
    elif [ "$blocked_task_count" -gt 0 ]; then
        health="blocked"
        detail="$blocked_task_count blocked task(s)"
        SUMMARY_BLOCKED=$((SUMMARY_BLOCKED + 1))
    # Working: has tasks, active recently
    elif [ "$active_task_count" -gt 0 ] && [ "$mins_since_active" -lt 30 ]; then
        health="working"
        detail="$active_task_count active task(s), active ${mins_since_active}min ago"
        SUMMARY_WORKING=$((SUMMARY_WORKING + 1))
    # Stalled: has tasks, but no recent activity
    elif [ "$active_task_count" -gt 0 ] && [ "$mins_since_active" -ge "$STALLED_THRESHOLD_MIN" ]; then
        health="stalled"
        detail="$active_task_count active task(s), no activity for ${mins_since_active}min"
        SUMMARY_STALLED=$((SUMMARY_STALLED + 1))
    # Zombie: running but no heartbeat and no tasks
    elif [ "$mins_since_heartbeat" -ge "$ZOMBIE_THRESHOLD_MIN" ] && [ "$active_task_count" -eq 0 ]; then
        health="zombie"
        detail="no heartbeat for ${mins_since_heartbeat}min, no active tasks"
        SUMMARY_ZOMBIE=$((SUMMARY_ZOMBIE + 1))
    # Idle: running, no tasks, within thresholds
    elif [ "$active_task_count" -eq 0 ]; then
        health="idle"
        detail="no active tasks"
        SUMMARY_IDLE=$((SUMMARY_IDLE + 1))
    # Default: working (has tasks, moderate activity)
    elif [ "$active_task_count" -gt 0 ]; then
        health="working"
        detail="$active_task_count active task(s), active ${mins_since_active}min ago"
        SUMMARY_WORKING=$((SUMMARY_WORKING + 1))
    fi

    # Add to result
    RESULT_WORKERS=$(echo "$RESULT_WORKERS" | jq \
        --arg name "$name" \
        --arg health "$health" \
        --arg since "$since" \
        --arg detail "$detail" \
        --argjson tasks "$active_task_count" \
        --argjson blocked "$blocked_task_count" \
        --argjson mins_active "$mins_since_active" \
        '. + {($name): {health: $health, since: $since, detail: $detail, active_tasks: $tasks, blocked_tasks: $blocked, mins_since_active: $mins_active}}')
done

# ─── Output ──────────────────────────────────────────────────────────────────

jq -n \
    --argjson workers "$RESULT_WORKERS" \
    --argjson working "$SUMMARY_WORKING" \
    --argjson stalled "$SUMMARY_STALLED" \
    --argjson zombie "$SUMMARY_ZOMBIE" \
    --argjson idle "$SUMMARY_IDLE" \
    --argjson blocked "$SUMMARY_BLOCKED" \
    --argjson stopped "$SUMMARY_STOPPED" \
    --argjson total "$worker_count" \
    '{
        workers: $workers,
        summary: {
            total: $total,
            working: $working,
            stalled: $stalled,
            zombie: $zombie,
            idle: $idle,
            blocked: $blocked,
            stopped: $stopped
        }
    }'
