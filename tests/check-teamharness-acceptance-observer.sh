#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
eval "$(sed -n '/^_task_check_succeeded()/,/^}/p' "$ROOT/tests/test-26-qwenpaw-teamharness-plugin-mode.sh")"
TASK_ID=task-unique
PROJECT_ID=project-unique
TEAM_ROOM='!team:test'
TEST_WORKER=worker-unique
WORKER_MXID='@worker:test'
MARKER=marker-unique
submitted='{"ok":true,"effective":true,"task":{"task_id":"task-unique","project_id":"project-unique","room_id":"!team:test","assigned_to":"worker-unique","status":"submitted"},"result":{"status":"SUCCESS","summary":"marker-unique"},"validationErrors":[]}'
completed=$(jq '.effective=false | .task.status="completed" | .task.history=[{action:"accept_task_result",from:"submitted",to:"completed"}]' <<< "$submitted")
_task_check_succeeded <<< "$submitted"
_task_check_succeeded <<< "$completed"
for mutation in '.task.status="failed"' '.task.history=[]' '.validationErrors=["missing result"]' '.result.status="FAILED"' '.task.task_id="other"' '.task.project_id="other"' '.task.room_id="!other:test"' '.task.assigned_to="other"' '.result.summary="stale marker"' '.ok=false'; do
    if jq "$mutation" <<< "$completed" | _task_check_succeeded; then
        echo "FAIL: accepted $mutation"; exit 1
    fi
done
if jq '.effective=false' <<< "$submitted" | _task_check_succeeded; then
    echo 'FAIL: accepted ineffective submission'; exit 1
fi
echo 'PASS: submitted and accepted results pass; invalid or unrelated results fail'
