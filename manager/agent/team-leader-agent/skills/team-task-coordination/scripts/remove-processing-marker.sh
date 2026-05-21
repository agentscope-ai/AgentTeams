#!/bin/bash
# Remove the processing marker from a team task directory
# Usage: remove-processing-marker.sh <task-id>

set -e

task_id="$1"

if [ -z "$task_id" ]; then
    echo "Usage: $0 <task-id>" >&2
    exit 2
fi

# Resolve team name
if [ -z "${TEAM_NAME:-}" ]; then
    if [ -f "${HOME}/SOUL.md" ]; then
        TEAM_NAME=$(grep -oP 'Team Leader of \K[^\s]+' "${HOME}/SOUL.md" 2>/dev/null || true)
    fi
fi
if [ -z "${TEAM_NAME:-}" ]; then
    echo "ERROR: Cannot determine TEAM_NAME" >&2
    exit 2
fi

marker_file="/root/hiclaw-fs/shared/tasks/${task_id}/.processing"

if [ -f "$marker_file" ]; then
    rm -f "$marker_file"
    echo "[remove-processing-marker] Removed marker for ${task_id}"
else
    echo "[remove-processing-marker] No marker to remove for ${task_id}"
fi
