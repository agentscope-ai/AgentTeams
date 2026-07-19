#!/bin/bash
# session-snapshot.sh - Write operational context snapshot for rapid session recovery
#
# Called at the end of each heartbeat cycle. Writes ~/last-session-snapshot.json
# with a summary of current operational state.
#
# Usage:
#   session-snapshot.sh

set -euo pipefail

SNAPSHOT_FILE="${HOME}/last-session-snapshot.json"
STATE_FILE="${HOME}/state.json"
ESCALATIONS_FILE="${HOME}/escalations.json"

_ts() {
    date -u '+%Y-%m-%dT%H:%M:%SZ'
}

# ─── Gather data ─────────────────────────────────────────────────────────────

NOW=$(_ts)

# Active tasks from state.json
ACTIVE_TASKS="[]"
ACTIVE_COUNT=0
if [ -f "$STATE_FILE" ]; then
    ACTIVE_TASKS=$(jq -c '[.active_tasks[]? | {task_id, title, type, assigned_to, status: (.status // "active")}]' "$STATE_FILE" 2>/dev/null || echo "[]")
    ACTIVE_COUNT=$(echo "$ACTIVE_TASKS" | jq 'length')
fi

# Open escalations
ESCALATIONS_SUMMARY='{"open": 0, "highest_severity": "NONE", "items": []}'
if [ -f "$ESCALATIONS_FILE" ]; then
    ESCALATIONS_SUMMARY=$(jq -c '{
        open: [.escalations[]? | select(.status != "resolved")] | length,
        highest_severity: (
            if ([.escalations[]? | select(.status != "resolved" and .severity == "CRITICAL")] | length) > 0 then "CRITICAL"
            elif ([.escalations[]? | select(.status != "resolved" and .severity == "HIGH")] | length) > 0 then "HIGH"
            elif ([.escalations[]? | select(.status != "resolved" and .severity == "MEDIUM")] | length) > 0 then "MEDIUM"
            else "NONE"
            end
        ),
        items: [.escalations[]? | select(.status != "resolved") | {id, severity, task_id, worker, summary}]
    }' "$ESCALATIONS_FILE" 2>/dev/null || echo '{"open": 0, "highest_severity": "NONE", "items": []}')
fi

# Worker health summary (if script exists)
HEALTH_SUMMARY='{"total": 0, "working": 0, "stalled": 0, "zombie": 0, "idle": 0, "blocked": 0, "stopped": 0}'
HEALTH_SCRIPT="${HOME}/skills/worker-management/scripts/worker-health-report.sh"
if [ -f "$HEALTH_SCRIPT" ]; then
    HEALTH_SUMMARY=$(bash "$HEALTH_SCRIPT" 2>/dev/null | jq -c '.summary' 2>/dev/null || echo "$HEALTH_SUMMARY")
elif [ -f "/opt/hiclaw/agent/skills/worker-management/scripts/worker-health-report.sh" ]; then
    HEALTH_SUMMARY=$(bash /opt/hiclaw/agent/skills/worker-management/scripts/worker-health-report.sh 2>/dev/null | jq -c '.summary' 2>/dev/null || echo "$HEALTH_SUMMARY")
fi

# Deferred tasks (tasks with "deferred": true in state.json)
DEFERRED_TASKS="[]"
if [ -f "$STATE_FILE" ]; then
    DEFERRED_TASKS=$(jq -c '[.active_tasks[]? | select(.deferred == true) | {task_id, title, assigned_to}]' "$STATE_FILE" 2>/dev/null || echo "[]")
fi

# ─── Write snapshot ──────────────────────────────────────────────────────────

jq -n \
    --arg timestamp "$NOW" \
    --argjson active_tasks "$ACTIVE_TASKS" \
    --argjson active_count "$ACTIVE_COUNT" \
    --argjson escalations "$ESCALATIONS_SUMMARY" \
    --argjson health "$HEALTH_SUMMARY" \
    --argjson deferred "$DEFERRED_TASKS" \
    '{
        snapshot_version: 1,
        timestamp: $timestamp,
        active_tasks: {
            count: $active_count,
            items: $active_tasks
        },
        escalations: $escalations,
        worker_health: $health,
        deferred_tasks: $deferred
    }' > "$SNAPSHOT_FILE"

echo "OK: snapshot written to $SNAPSHOT_FILE"
