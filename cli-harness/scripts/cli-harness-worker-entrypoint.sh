#!/bin/bash
# cli-harness-worker-entrypoint.sh - CLI-harness Worker container startup
# Reads config from environment variables and launches cli-harness-worker.
#
# Environment variables (set by controller during worker creation):
#   AGENTTEAMS_WORKER_NAME    - Worker name (required)
#   AGENTTEAMS_WORKER_RUNTIME - CLI runtime: atomcode|codex|claude-code|kimi-code|pi|dsh
#                               (injected by the controller backends; optional)
#   AGENTTEAMS_CLI_ADAPTER    - runtime override for backends that cannot pass
#                               env (e.g. SandboxClaim); wins over WORKER_RUNTIME
#   AGENTTEAMS_FS_ENDPOINT    - MinIO endpoint (required in local mode)
#   AGENTTEAMS_FS_ACCESS_KEY  - MinIO access key (required in local mode)
#   AGENTTEAMS_FS_SECRET_KEY  - MinIO secret key (required in local mode)
#   AGENTTEAMS_RUNTIME        - "aliyun" for cloud mode (RRSA/STS via agentteams-env.sh)
#   TZ                        - Timezone (optional)

set -e

# Source shared environment bootstrap (provides ensure_mc_credentials in cloud mode)
source /opt/agentteams/scripts/lib/agentteams-env.sh 2>/dev/null || true

WORKER_NAME="${AGENTTEAMS_WORKER_NAME:?AGENTTEAMS_WORKER_NAME is required}"
# Align with the openclaw/hermes layout: HOME == workspace == MinIO mirror root.
INSTALL_DIR="/root/agentteams-fs/agents"
WORKSPACE="${INSTALL_DIR}/${WORKER_NAME}"

log() {
    echo "[agentteams-cli-harness $(date '+%Y-%m-%d %H:%M:%S')] $1"
}

# Set timezone from TZ env var
if [ -n "${TZ}" ] && [ -f "/usr/share/zoneinfo/${TZ}" ]; then
    ln -sf "/usr/share/zoneinfo/${TZ}" /etc/localtime
    echo "${TZ}" > /etc/timezone
    log "Timezone set to ${TZ}"
fi

# ── Credential setup ─────────────────────────────────────────────────────────
if [ "${AGENTTEAMS_RUNTIME:-}" = "aliyun" ]; then
    log "Cloud mode: configuring OSS credentials via RRSA..."
    ensure_mc_credentials || { log "ERROR: Failed to obtain OSS credentials"; exit 1; }
    FS_ENDPOINT="https://oss-placeholder.aliyuncs.com"
    FS_ACCESS_KEY="rrsa"
    FS_SECRET_KEY="rrsa"
    FS_BUCKET="${AGENTTEAMS_FS_BUCKET:-agentteams-cloud-storage}"
else
    FS_ENDPOINT="${AGENTTEAMS_FS_ENDPOINT:?AGENTTEAMS_FS_ENDPOINT is required}"
    FS_ACCESS_KEY="${AGENTTEAMS_FS_ACCESS_KEY:?AGENTTEAMS_FS_ACCESS_KEY is required}"
    FS_SECRET_KEY="${AGENTTEAMS_FS_SECRET_KEY:?AGENTTEAMS_FS_SECRET_KEY is required}"
    FS_BUCKET="${AGENTTEAMS_FS_BUCKET:-agentteams-storage}"
fi
log "  FS bucket: ${FS_BUCKET}"

# Workspace == HOME; expose skills at the legacy ~/.agents/skills path too.
mkdir -p "${WORKSPACE}/skills" "${HOME}/.agents"
ln -sfn "${WORKSPACE}/skills" "${HOME}/.agents/skills"

# ── Runtime selection ────────────────────────────────────────────────────────
# Resolution order (must match cli_harness_worker.adapters.normalize_runtime):
#   AGENTTEAMS_CLI_ADAPTER > AGENTTEAMS_WORKER_RUNTIME > default in Python.
RUNTIME="${AGENTTEAMS_CLI_ADAPTER:-${AGENTTEAMS_WORKER_RUNTIME:-}}"
if [ -n "${RUNTIME}" ]; then
    log "  CLI runtime: ${RUNTIME}"
else
    log "  CLI runtime: (unset — Python default applies)"
fi

# Background readiness reporter — report ready once the Python worker has
# written its ready marker (Matrix login done, adapter selected).
_start_readiness_reporter() {
    [ -z "${AGENTTEAMS_CONTROLLER_URL:-}" ] && return 0

    (
        TIMEOUT=180; ELAPSED=0
        READY_MARKER="${WORKSPACE}/.cli-harness/ready"
        while [ "${ELAPSED}" -lt "${TIMEOUT}" ]; do
            if [ -f "${READY_MARKER}" ]; then
                break
            fi
            sleep 5; ELAPSED=$((ELAPSED + 5))
        done

        if [ "${ELAPSED}" -ge "${TIMEOUT}" ]; then
            log "WARNING: readiness reporter timed out waiting for marker after ${TIMEOUT}s"
            exit 1
        fi

        agt worker report-ready
    ) &
    log "Background readiness reporter started (PID: $!)"
}

VENV="/opt/venv/cli-harness"
log "Starting cli-harness-worker: ${WORKER_NAME}"
log "  FS endpoint: ${FS_ENDPOINT}"
log "  Install dir: ${INSTALL_DIR}"

CMD_ARGS=(
    --name "${WORKER_NAME}"
    --runtime "${RUNTIME}"
    --fs "${FS_ENDPOINT}"
    --fs-key "${FS_ACCESS_KEY}"
    --fs-secret "${FS_SECRET_KEY}"
    --fs-bucket "${FS_BUCKET}"
    --install-dir "${INSTALL_DIR}"
)

_start_readiness_reporter

exec "${VENV}/bin/cli-harness-worker" "${CMD_ARGS[@]}"
