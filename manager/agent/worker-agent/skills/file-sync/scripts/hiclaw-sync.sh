#!/bin/sh
# hiclaw-sync.sh - Pull latest config from centralized storage
# Called by the Worker agent when coordinator notifies of config updates.
# Uses /root/hiclaw-fs/ layout — same absolute path as the Manager's MinIO mirror.

# Bootstrap env: provides HICLAW_STORAGE_PREFIX and ensure_mc_credentials
if [ -f /opt/hiclaw/scripts/lib/hiclaw-env.sh ]; then
    . /opt/hiclaw/scripts/lib/hiclaw-env.sh
else
    . /opt/hiclaw/scripts/lib/oss-credentials.sh 2>/dev/null || true
    ensure_mc_credentials 2>/dev/null || true
    HICLAW_STORAGE_PREFIX="hiclaw/${HICLAW_OSS_BUCKET:-hiclaw-storage}"
fi

WORKER_NAME="${HICLAW_WORKER_NAME:?HICLAW_WORKER_NAME is required}"
HICLAW_ROOT="/root/hiclaw-fs"
WORKSPACE="${HICLAW_ROOT}/agents/${WORKER_NAME}"

ensure_mc_credentials 2>/dev/null || true
mc mirror "${HICLAW_STORAGE_PREFIX}/agents/${WORKER_NAME}/" "${WORKSPACE}/" --overwrite \
    --exclude ".openclaw/matrix/**" --exclude ".openclaw/canvas/**" 2>&1
mc mirror "${HICLAW_STORAGE_PREFIX}/shared/" "${HICLAW_ROOT}/shared/" --overwrite 2>/dev/null || true

# Restore +x on scripts (MinIO does not preserve Unix permission bits)
find "${WORKSPACE}/skills" -name '*.sh' -exec chmod +x {} + 2>/dev/null || true

echo "Config sync completed at $(date)"
