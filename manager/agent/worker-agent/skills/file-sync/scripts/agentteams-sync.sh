#!/bin/sh
# agentteams-sync.sh - Pull latest config from centralized storage
# Called by the Worker agent when coordinator notifies of config updates.
# Uses /root/agentteams-fs/ layout — same absolute path as the Manager's MinIO mirror.

# Bootstrap env: provides AGENTTEAMS_STORAGE_PREFIX and ensure_mc_credentials.
# Library overrides keep the sync behavior testable without changing runtime
# defaults inside the Worker image.
AGENTTEAMS_LIB_DIR="${AGENTTEAMS_LIB_DIR:-/opt/agentteams/scripts/lib}"
if [ -f "${AGENTTEAMS_LIB_DIR}/agentteams-env.sh" ]; then
    . "${AGENTTEAMS_LIB_DIR}/agentteams-env.sh"
else
    . "${AGENTTEAMS_LIB_DIR}/oss-credentials.sh" 2>/dev/null || true
    ensure_mc_credentials 2>/dev/null || true
    AGENTTEAMS_FS_BUCKET="${AGENTTEAMS_FS_BUCKET:-agentteams-storage}"
    AGENTTEAMS_STORAGE_PREFIX="${AGENTTEAMS_STORAGE_PREFIX:-agentteams/${AGENTTEAMS_FS_BUCKET}}"
fi

# Merge helper for openclaw.json (local-first: MinIO overlays models/gateway/channels + plugins rules)
. "${AGENTTEAMS_LIB_DIR}/merge-openclaw-config.sh"
. "${AGENTTEAMS_LIB_DIR}/worker-file-sync.sh"

WORKER_NAME="${AGENTTEAMS_WORKER_NAME:?AGENTTEAMS_WORKER_NAME is required}"
AGENTTEAMS_ROOT="${AGENTTEAMS_ROOT:-/root/agentteams-fs}"
WORKSPACE="${AGENTTEAMS_ROOT}/agents/${WORKER_NAME}"

ensure_mc_credentials 2>/dev/null || true

# Fetch Manager-owned config separately. The bulk mirror must never replace
# the live file, even briefly, because OpenClaw watches it and may reconnect
# with the stale Matrix token stored in MinIO.
LOCAL_OPENCLAW="${WORKSPACE}/openclaw.json"
REMOTE_OPENCLAW="${AGENTTEAMS_REMOTE_OPENCLAW_TMP:-/tmp/openclaw-remote-sync-${WORKER_NAME}.json}"
rm -f "${REMOTE_OPENCLAW}"
mc cp "${AGENTTEAMS_STORAGE_PREFIX}/agents/${WORKER_NAME}/openclaw.json" "${REMOTE_OPENCLAW}" 2>/dev/null || true

mc mirror "${AGENTTEAMS_STORAGE_PREFIX}/agents/${WORKER_NAME}/" "${WORKSPACE}/" --overwrite \
    --exclude "openclaw.json" \
    --exclude ".openclaw/matrix/**" --exclude ".openclaw/canvas/**" 2>&1
mc mirror "${AGENTTEAMS_STORAGE_PREFIX}/shared/" "${AGENTTEAMS_ROOT}/shared/" --overwrite 2>/dev/null || true

# Update pull marker so the local→remote sync loop doesn't push back freshly-pulled files
touch "${WORKSPACE}/.last-pull"

# Merge openclaw.json after the bulk mirror: local token and Worker-owned
# settings remain authoritative while Manager-owned slices are refreshed.
if [ -f "${REMOTE_OPENCLAW}" ]; then
    if ! merge_openclaw_config "${REMOTE_OPENCLAW}" "${LOCAL_OPENCLAW}" "${LOCAL_OPENCLAW}"; then
        echo "WARNING: failed to merge remote openclaw.json; keeping local config" >&2
    fi
    rm -f "${REMOTE_OPENCLAW}"
fi

# Restore +x on scripts (MinIO does not preserve Unix permission bits)
find "${WORKSPACE}/skills" -name '*.sh' -exec chmod +x {} + 2>/dev/null || true
worker_sync_mark_remote_pull "/tmp/agentteams-worker-sync"

echo "Config sync completed at $(date)"
