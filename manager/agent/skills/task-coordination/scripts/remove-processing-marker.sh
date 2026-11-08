#!/bin/bash
# Remove the processing marker from a task directory
# Usage: remove-processing-marker.sh <task-id> [run-id]

set -e

task_id="$1"
run_id="${2:-}"

if [ -z "$task_id" ]; then
    echo "Usage: $0 <task-id> [run-id]" >&2
    exit 2
fi

marker_file="/root/agentteams-fs/shared/tasks/${task_id}/.processing"

if [ -f "$marker_file" ]; then
    if [ -n "$run_id" ]; then
        marker_run_id=$(jq -r '.run_id // empty' "$marker_file" 2>/dev/null || true)
        if [ "$marker_run_id" != "$run_id" ]; then
            echo "[remove-processing-marker] Marker belongs to another run; keeping it" >&2
            exit 1
        fi
    fi
    rm -f "$marker_file"
    echo "[remove-processing-marker] Removed marker for ${task_id}"
else
    echo "[remove-processing-marker] No marker to remove for ${task_id}"
fi
