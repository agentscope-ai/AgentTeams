#!/bin/bash
# start-mc-mirror.sh - Initialize MinIO storage and start periodic Remote->Local sync
#
# Manager's own workspace (/root/manager-workspace/) is LOCAL ONLY and not synced to MinIO.
# MinIO only stores shared data and worker configs (/root/hiclaw-fs/).
#
# ── File Sync Design Principle ──────────────────────────────────────────────
#
#   Local -> Remote (push):
#     The party that writes a file is responsible for pushing it to MinIO
#     immediately via explicit mc cp/mirror. No background Local->Remote sync.
#
#   Remote -> Local (pull):
#     The party that modifies files in MinIO is responsible for notifying the
#     other side via Matrix @mention, so the receiver can pull on demand.
#     Examples:
#       - Manager pushes task spec → @mentions Worker → Worker runs file-sync
#       - Worker pushes task result → @mentions Manager → Manager runs mc mirror
#       - Manager pushes skill update → push-worker-skills.sh notifies Worker
#
#   This script only provides a 5-minute fallback pull as a safety net, in case
#   an on-demand pull was missed (e.g., agent didn't follow SKILL.md exactly).
#   Normal operation should NOT rely on this fallback.
#
# ────────────────────────────────────────────────────────────────────────────

source /opt/hiclaw/scripts/lib/hiclaw-env.sh
waitForService "MinIO" "127.0.0.1" 9000

# Configure mc alias (local access, not through Higress)
mc alias set hiclaw http://127.0.0.1:9000 "${HICLAW_MINIO_USER:-${HICLAW_ADMIN_USER:-admin}}" "${HICLAW_MINIO_PASSWORD:-${HICLAW_ADMIN_PASSWORD:-admin}}"

# Create default bucket
mc mb "${HICLAW_STORAGE_PREFIX}" --ignore-existing

# Initialize placeholder directories for shared data and worker artifacts
for dir in shared/knowledge shared/tasks workers; do
    echo "" | mc pipe "${HICLAW_STORAGE_PREFIX}/${dir}/.gitkeep" 2>/dev/null || true
done

# Initialize hiclaw-config directory for declarative CRD-style resources
for dir in hiclaw-config/workers hiclaw-config/teams hiclaw-config/humans; do
    echo "" | mc pipe "${HICLAW_STORAGE_PREFIX}/${dir}/.gitkeep" 2>/dev/null || true
done

# Create local mirror directory (for shared + worker data only)
# Use absolute path because HOME may point to manager-workspace
HICLAW_FS_ROOT="/root/hiclaw-fs"
mkdir -p "${HICLAW_FS_ROOT}"
mkdir -p "${HICLAW_FS_ROOT}/hiclaw-config"

# Initial full sync to local (workers + shared)
mc mirror "${HICLAW_STORAGE_PREFIX}/" "${HICLAW_FS_ROOT}/" --overwrite

# Signal that initialization is complete
touch "${HICLAW_FS_ROOT}/.initialized"

log "MinIO storage initialized and synced to ${HICLAW_FS_ROOT}/"

# hiclaw-config mirror: 10-second interval for control plane config (CRD YAML files).
# hiclaw-controller watches this directory via fsnotify to trigger reconcile.
(
    while true; do
        sleep 10
        mc mirror "${HICLAW_STORAGE_PREFIX}/hiclaw-config/" "${HICLAW_FS_ROOT}/hiclaw-config/" --overwrite --newer-than "15s" 2>/dev/null || true
    done
) &

# Fallback: periodic Remote->Local pull every 5 minutes.
# Normal operation relies on on-demand pulls triggered by Matrix notifications.
# This loop is a safety net only — see design principle above.
while true; do
    sleep 300
    mc mirror "${HICLAW_STORAGE_PREFIX}/" "${HICLAW_FS_ROOT}/" --overwrite --newer-than "5m" 2>/dev/null || true
done
