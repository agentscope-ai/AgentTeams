#!/bin/bash
# Check if a task directory has an active processing marker
# Usage: check-processing-marker.sh <task-id>
# Exit codes:
#   0 - No marker or marker expired (safe to proceed)
#   1 - Valid marker exists (do not modify)

set -e

task_id="$1"

if [ -z "$task_id" ]; then
    echo "Usage: $0 <task-id>" >&2
    exit 2
fi

marker_file="/root/agentteams-fs/shared/tasks/${task_id}/.processing"

# No marker file = safe to proceed
if [ ! -f "$marker_file" ]; then
    echo "[check-processing-marker] No marker found for ${task_id}"
    exit 0
fi

# Read marker file
if ! marker_content=$(cat "$marker_file" 2>/dev/null); then
    echo "[check-processing-marker] Failed to read marker, assuming expired"
    rm -f "$marker_file"
    exit 0
fi

# Extract expiration time
expires_at=$(echo "$marker_content" | jq -r '.expires_at // empty' 2>/dev/null || true)

if [ -z "$expires_at" ]; then
    echo "[check-processing-marker] Invalid marker format, removing"
    rm -f "$marker_file"
    exit 0
fi

# Check if expired (compare ISO 8601 timestamps)
now=$(date -u +%Y-%m-%dT%H:%M:%SZ)

# Convert to seconds since epoch for comparison
if expires_epoch=$(date -d "$expires_at" +%s 2>/dev/null); then
    now_epoch=$(date -d "$now" +%s 2>/dev/null)

    if [ "$now_epoch" -ge "$expires_epoch" ]; then
        processor=$(echo "$marker_content" | jq -r '.processor // "unknown"')
        echo "[check-processing-marker] Marker expired (was held by ${processor}), removing"
        rm -f "$marker_file"
        exit 0
    fi
fi

# Active marker exists
processor=$(echo "$marker_content" | jq -r '.processor // "unknown"')
operation=$(echo "$marker_content" | jq -r '.operation // "unknown"')
started=$(echo "$marker_content" | jq -r '.started_at // "unknown"')

echo "[check-processing-marker] ACTIVE marker found:"
echo "  Processor: ${processor}"
echo "  Operation: ${operation}"
echo "  Started: ${started}"
echo "  Expires: ${expires_at}"
exit 1
