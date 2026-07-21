#!/bin/bash
# start-mc-mirror.sh - Initialize MinIO storage and start periodic Remote->Local sync
#
# Manager's own workspace (/root/manager-workspace/) is LOCAL ONLY and not synced to MinIO.
# MinIO only stores shared data and worker configs (/root/agentteams-fs/).
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

source /opt/agentteams/scripts/lib/agentteams-env.sh

# MinIO S3: use explicit URL, or cluster FS endpoint, or in-process minio (embedded controller)
# (Port 8080 is Higress, not the S3 API; never use it for mc.)
MINIO_S3_URL="${AGENTTEAMS_MINIO_S3_URL:-${AGENTTEAMS_FS_ENDPOINT:-${AGENTTEAMS_MINIO_ENDPOINT:-http://127.0.0.1:9000}}}"
MINIO_S3_URL="${MINIO_S3_URL//:8080/:9000}"
_HP="${MINIO_S3_URL#*://}"
_HP="${_HP%%/*}"
_MINIO_HOST="${_HP%%:*}"
_MINIO_PORT="${_HP##*:}"
if [ "${_MINIO_PORT}" = "${_HP}" ] || [ -z "${_MINIO_PORT}" ]; then
    _MINIO_PORT=9000
fi
waitForService "MinIO" "${_MINIO_HOST}" "${_MINIO_PORT}"

# Configure mc alias (direct S3, not Higress HTTP)
mc alias set agentteams "${MINIO_S3_URL}" \
    "${AGENTTEAMS_MINIO_USER:-${AGENTTEAMS_ADMIN_USER:-admin}}" \
    "${AGENTTEAMS_MINIO_PASSWORD:-${AGENTTEAMS_ADMIN_PASSWORD:-admin}}"

# Create default bucket
mc mb "${AGENTTEAMS_STORAGE_PREFIX}" --ignore-existing

if ! mc ls "${AGENTTEAMS_STORAGE_PREFIX}/" > /dev/null 2>&1; then
    log "ERROR: MinIO S3 is not usable at ${MINIO_S3_URL} (AGENTTEAMS_MINIO_S3_URL / AGENTTEAMS_FS_ENDPOINT must be the S3 port, e.g. :9000, not the Higress gateway 8080)."
    exit 1
fi

# Initialize placeholder directories for shared data and worker artifacts
for dir in shared/knowledge shared/tasks workers; do
    echo "" | mc pipe "${AGENTTEAMS_STORAGE_PREFIX}/${dir}/.gitkeep" 2>/dev/null || true
done

# Initialize agentteams-config directory for declarative CRD-style resources
for dir in agentteams-config/workers agentteams-config/teams agentteams-config/humans; do
    echo "" | mc pipe "${AGENTTEAMS_STORAGE_PREFIX}/${dir}/.gitkeep" 2>/dev/null || true
done

# Create local mirror directory (for shared + worker data only)
# Use absolute path because HOME may point to manager-workspace
AGENTTEAMS_FS_ROOT="/root/agentteams-fs"
mkdir -p "${AGENTTEAMS_FS_ROOT}"
mkdir -p "${AGENTTEAMS_FS_ROOT}/agentteams-config"

# Initial full sync to local (workers + shared)
mc mirror "${AGENTTEAMS_STORAGE_PREFIX}/" "${AGENTTEAMS_FS_ROOT}/" --overwrite

# Signal that initialization is complete
touch "${AGENTTEAMS_FS_ROOT}/.initialized"

log "MinIO storage initialized and synced to ${AGENTTEAMS_FS_ROOT}/"

# agentteams-config mirror: 10-second interval for control plane config (CRD YAML files).
# agentteams-controller watches this directory via fsnotify to trigger reconcile.
(
    while true; do
        sleep 10
        mc mirror "${AGENTTEAMS_STORAGE_PREFIX}/agentteams-config/" "${AGENTTEAMS_FS_ROOT}/agentteams-config/" --overwrite --remove --newer-than "15s" 2>/dev/null || true
    done
) &

# Fallback: periodic Remote->Local pull every 5 minutes.
# Normal operation relies on on-demand pulls triggered by Matrix notifications.
# This loop is a safety net only — see design principle above.
while true; do
    sleep 300
    mc mirror "${AGENTTEAMS_STORAGE_PREFIX}/" "${AGENTTEAMS_FS_ROOT}/" --overwrite --newer-than "5m" 2>/dev/null || true
done
