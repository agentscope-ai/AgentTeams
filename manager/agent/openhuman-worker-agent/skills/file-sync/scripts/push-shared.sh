#!/bin/bash
# push-shared.sh - Push local shared/ files to the correct MinIO path
#
# Automatically detects team membership and pushes to:
#   - teams/{team}/shared/{path} for team workers
#   - shared/{path} for non-team workers
#
# Usage:
#   push-shared.sh tasks/{task-id}/
#   push-shared.sh tasks/{task-id}/ --exclude "spec.md" --exclude "base/"

set -euo pipefail

SUBPATH="${1:?Usage: push-shared.sh <subpath> [--exclude ...]}"
shift

# Resolve local shared dir
LOCAL_SHARED=""
for _candidate in "./shared" "../shared" "/root/hiclaw-fs/shared"; do
    if [ -d "${_candidate}" ]; then
        LOCAL_SHARED="${_candidate}"
        break
    fi
done
if [ -z "${LOCAL_SHARED}" ]; then
    echo "ERROR: shared/ directory not found" >&2
    exit 1
fi

# Detect team from AGENTS.md
TEAM_NAME=""
for _agents in "./AGENTS.md" "../AGENTS.md"; do
    if [ -f "${_agents}" ]; then
        TEAM_NAME=$(grep -oP '\*\*Team\*\*:\s*\K\S+' "${_agents}" 2>/dev/null || true)
        break
    fi
done

# Build MinIO destination
HICLAW_STORAGE_PREFIX="${HICLAW_STORAGE_PREFIX:-hiclaw/hiclaw-storage}"
if [ -n "${TEAM_NAME}" ]; then
    MINIO_DEST="${HICLAW_STORAGE_PREFIX}/teams/${TEAM_NAME}/shared/${SUBPATH}"
else
    MINIO_DEST="${HICLAW_STORAGE_PREFIX}/shared/${SUBPATH}"
fi

LOCAL_SRC="${LOCAL_SHARED}/${SUBPATH}"
if [ ! -d "${LOCAL_SRC}" ] && [ ! -f "${LOCAL_SRC}" ]; then
    echo "ERROR: ${LOCAL_SRC} does not exist" >&2
    exit 1
fi

# Build mc mirror args
MC_ARGS=("mirror" "${LOCAL_SRC}" "${MINIO_DEST}" "--overwrite")
for arg in "$@"; do
    MC_ARGS+=("$arg")
done

mc "${MC_ARGS[@]}" 2>&1
echo "OK: Pushed ${SUBPATH} to ${MINIO_DEST}"
