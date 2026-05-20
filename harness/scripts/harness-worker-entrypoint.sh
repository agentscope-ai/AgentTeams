#!/bin/bash
# Harness Worker container entrypoint
# Mirrors hermes-worker-entrypoint.sh pattern

set -e

WORKER_NAME="${HICLAW_WORKER_NAME?}"
FS_ENDPOINT="${HICLAW_FS_ENDPOINT?}"
FS_ACCESS_KEY="${HICLAW_FS_ACCESS_KEY?}"
FS_SECRET_KEY="${HICLAW_FS_SECRET_KEY?}"
FS_BUCKET="${HICLAW_FS_BUCKET:-hiclaw-storage}"
INSTALL_DIR="${HICLAW_INSTALL_DIR:-/root/hiclaw-fs/agents}"
HARNESS_TYPE="${HICLAW_HARNESS_TYPE:-claude}"

WORKSPACE="${INSTALL_DIR}/${WORKER_NAME}"

CMD_ARGS=(
    --name "${WORKER_NAME}"
    --fs "${FS_ENDPOINT}"
    --fs-key "${FS_ACCESS_KEY}"
    --fs-secret "${FS_SECRET_KEY}"
    --fs-bucket "${FS_BUCKET}"
    --install-dir "${INSTALL_DIR}"
    --harness-type "${HARNESS_TYPE}"
)

# Readiness reporter (same pattern as hermes-worker-entrypoint.sh)
(
    READY_TIMEOUT=120
    ELAPSED=0
    while [ $ELAPSED -lt $READY_TIMEOUT ]; do
        if [ -f "${WORKSPACE}/.harness/ready" ]; then
            break
        fi
        sleep 1
        ELAPSED=$((ELAPSED + 1))
    done
    if [ $ELAPSED -lt $READY_TIMEOUT ]; then
        hiclaw worker report-ready --worker "${WORKER_NAME}" || true
    fi
) &
READY_PID=$!

exec harness-worker "${CMD_ARGS[@]}"