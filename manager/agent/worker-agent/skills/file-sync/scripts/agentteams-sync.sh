#!/bin/sh
# agentteams-sync.sh - Pull latest config from centralized storage
# Called by the Worker agent when coordinator notifies of config updates.
# Uses /root/agentteams-fs/ layout — same absolute path as the Manager's MinIO mirror.

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

WORKER_NAME="${AGENTTEAMS_WORKER_NAME:?AGENTTEAMS_WORKER_NAME is required}"
AGENTTEAMS_ROOT="/root/agentteams-fs"
WORKSPACE="${AGENTTEAMS_ROOT}/agents/${WORKER_NAME}"

ensure_mc_credentials 2>/dev/null || true

# Save local openclaw.json before mirror overwrites it
LOCAL_OPENCLAW="${WORKSPACE}/openclaw.json"
SAVED_LOCAL="/tmp/openclaw-local-sync.json"
if [ -f "${LOCAL_OPENCLAW}" ]; then
    cp "${LOCAL_OPENCLAW}" "${SAVED_LOCAL}"
fi

mc mirror "${AGENTTEAMS_STORAGE_PREFIX}/agents/${WORKER_NAME}/" "${WORKSPACE}/" --overwrite \
    --exclude ".openclaw/matrix/**" --exclude ".openclaw/canvas/**" 2>&1
mc mirror "${AGENTTEAMS_STORAGE_PREFIX}/shared/" "${AGENTTEAMS_ROOT}/shared/" --overwrite 2>/dev/null || true

# Update pull marker so the local→remote sync loop doesn't push back freshly-pulled files
touch "${WORKSPACE}/.last-pull"

# Merge openclaw.json: local-first (pre-mirror copy) with MinIO overlay (arg1=remote, arg2=local, arg3=out)
if [ -f "${SAVED_LOCAL}" ] && [ -f "${LOCAL_OPENCLAW}" ]; then
    merge_openclaw_config "${LOCAL_OPENCLAW}" "${SAVED_LOCAL}" "${LOCAL_OPENCLAW}"
    rm -f "${SAVED_LOCAL}"
fi

# Restore +x on scripts (MinIO does not preserve Unix permission bits)
find "${WORKSPACE}/skills" -name '*.sh' -exec chmod +x {} + 2>/dev/null || true

echo "Config sync completed at $(date)"
