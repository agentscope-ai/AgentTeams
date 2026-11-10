#!/bin/sh
# agentteams-sync.sh - Pull latest config from centralized storage
# Called by the Worker agent when coordinator notifies of config updates.
# Uses /root/agentteams-fs/ layout — same absolute path as the Manager's MinIO mirror.

set -e

# Bootstrap env: provides AGENTTEAMS_STORAGE_PREFIX and ensure_mc_credentials
if [ -f /opt/agentteams/scripts/lib/agentteams-env.sh ]; then
    . /opt/agentteams/scripts/lib/agentteams-env.sh
else
    . /opt/agentteams/scripts/lib/oss-credentials.sh 2>/dev/null || true
    ensure_mc_credentials 2>/dev/null || true
    AGENTTEAMS_FS_BUCKET="${AGENTTEAMS_FS_BUCKET:-agentteams-storage}"
    AGENTTEAMS_STORAGE_PREFIX="${AGENTTEAMS_STORAGE_PREFIX:-agentteams/${AGENTTEAMS_FS_BUCKET}}"
fi

# Merge helper for openclaw.json (local-first: MinIO overlays models/gateway/channels + plugins rules)
. /opt/agentteams/scripts/lib/merge-openclaw-config.sh
. /opt/agentteams/scripts/lib/worker-file-sync.sh

WORKER_NAME="${AGENTTEAMS_WORKER_NAME:?AGENTTEAMS_WORKER_NAME is required}"
AGENTTEAMS_ROOT="/root/agentteams-fs"
WORKSPACE="${AGENTTEAMS_ROOT}/agents/${WORKER_NAME}"

ensure_mc_credentials 2>/dev/null || true

# Download into private staging; remote credentials must never replace your live token.
LOCAL_OPENCLAW="${WORKSPACE}/openclaw.json"
# Existing Worker images already exclude credentials/** from both upload paths.
# Keep staging on your workspace filesystem so the final rename is atomic.
mkdir -p "${WORKSPACE}/credentials"
STAGING_DIR="$(mktemp -d "${WORKSPACE}/credentials/.config-pull.XXXXXX")"
trap 'rm -rf "${STAGING_DIR}"' EXIT
trap 'exit 1' HUP INT TERM
mc cp "${AGENTTEAMS_STORAGE_PREFIX}/agents/${WORKER_NAME}/openclaw.json" "${STAGING_DIR}/openclaw.json"

# Keep your live SDK databases and WAL files intact during config refresh.
mc mirror "${AGENTTEAMS_STORAGE_PREFIX}/agents/${WORKER_NAME}/" "${WORKSPACE}/" --overwrite \
    --exclude "openclaw.json" --exclude "credentials/**" \
    --exclude ".openclaw/**" 2>&1
mc mirror "${AGENTTEAMS_STORAGE_PREFIX}/shared/" "${AGENTTEAMS_ROOT}/shared/" --overwrite

# Merge against the current local config, then publish atomically on the same filesystem.
merge_openclaw_config "${STAGING_DIR}/openclaw.json" "${LOCAL_OPENCLAW}" "${STAGING_DIR}/merged.json"
mv "${STAGING_DIR}/merged.json" "${LOCAL_OPENCLAW}"

# Mark success only after all pulls and the config merge have succeeded.
touch "${WORKSPACE}/.last-pull"

# Restore +x on scripts (MinIO does not preserve Unix permission bits)
find "${WORKSPACE}/skills" -name '*.sh' -exec chmod +x {} + 2>/dev/null || true
worker_sync_mark_remote_pull "/tmp/agentteams-worker-sync"

echo "Config sync completed at $(date)"
