#!/bin/bash
# Check if a team task directory has an active processing marker
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

# Check if expired
now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
expires_epoch=$(date -d "$expires_at" +%s 2>/dev/null || echo "0")
now_epoch=$(date -d "$now" +%s 2>/dev/null || echo "0")

if [ "$now_epoch" -ge "$expires_epoch" ] && [ "$expires_epoch" -gt 0 ]; then
    processor=$(echo "$marker_content" | jq -r '.processor // "unknown"')
    echo "[check-processing-marker] Marker expired (was held by ${processor}), removing"
    rm -f "$marker_file"
    exit 0
fi

# Active marker exists
processor=$(echo "$marker_content" | jq -r '.processor // "unknown"')
started=$(echo "$marker_content" | jq -r '.started_at // "unknown"')

echo "[check-processing-marker] ACTIVE marker found:"
echo "  Processor: ${processor}"
echo "  Started: ${started}"
echo "  Expires: ${expires_at}"
exit 1
