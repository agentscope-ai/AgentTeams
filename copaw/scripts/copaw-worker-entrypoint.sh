#!/bin/bash
# copaw-worker-entrypoint.sh - CoPaw Worker Agent container startup
# Reads config from environment variables and launches copaw-worker
# or lite-copaw-worker.
#
# Mode selection:
#   - AGENTTEAMS_CONSOLE_PORT set → standard mode
#   - console port unset → lite mode
#
# Environment variables (set by container_create_worker in container-api.sh):
#   AGENTTEAMS_WORKER_NAME   - Worker name (required)
#   AGENTTEAMS_FS_ENDPOINT   - MinIO endpoint (required in local mode)
#   AGENTTEAMS_FS_ACCESS_KEY - MinIO access key (required in local mode)
#   AGENTTEAMS_FS_SECRET_KEY - MinIO secret key (required in local mode)
#   AGENTTEAMS_CONSOLE_PORT  - CoPaw web console port (triggers standard mode)
#   TZ                   - Timezone (optional)

set -e

# Source shared environment bootstrap (provides worker-deps env and storage credentials)
source /opt/agentteams/scripts/lib/agentteams-env.sh

WORKER_NAME="${AGENTTEAMS_WORKER_NAME:-}"
[ -n "${WORKER_NAME}" ] || { echo "AGENTTEAMS_WORKER_NAME is required" >&2; exit 1; }
INSTALL_DIR="/root/.copaw-worker"
CONSOLE_PORT="${AGENTTEAMS_CONSOLE_PORT:-}"

log() {
    echo "[agentteams-copaw-worker $(date '+%Y-%m-%d %H:%M:%S')] $1"
}

# Set timezone from TZ env var
if [ -n "${TZ}" ] && [ -f "/usr/share/zoneinfo/${TZ}" ]; then
    ln -sf "/usr/share/zoneinfo/${TZ}" /etc/localtime
    echo "${TZ}" > /etc/timezone
    log "Timezone set to ${TZ}"
fi

# ── Credential setup ─────────────────────────────────────────────────────────
# Controller-mediated OSS: STS credentials via MC_HOST_${AGENTTEAMS_STORAGE_ALIAS:-agentteams}.
# Local MinIO: explicit FS endpoint/key/secret passed via CLI args.
if ensure_mc_credentials && agentteams_mc_host_configured; then
    log "Configuring OSS credentials via controller-issued STS..."
    # CLI requires --fs/--fs-key/--fs-secret but they are unused when the mc host is set.
    FS_ENDPOINT="https://oss-placeholder.aliyuncs.com"
    FS_ACCESS_KEY="rrsa"
    FS_SECRET_KEY="rrsa"
    FS_BUCKET="${AGENTTEAMS_FS_BUCKET:-agentteams-storage}"
    log "  OSS bucket: ${FS_BUCKET}"
else
    if [ "${AGENTTEAMS_STORAGE_PROVIDER:-minio}" = "oss" ]; then
        log "ERROR: OSS storage requires controller-issued storage credentials, but $(agentteams_mc_host_var) is not configured"
        exit 1
    fi
    FS_ENDPOINT="${AGENTTEAMS_FS_ENDPOINT:-}"
    FS_ACCESS_KEY="${AGENTTEAMS_FS_ACCESS_KEY:-}"
    FS_SECRET_KEY="${AGENTTEAMS_FS_SECRET_KEY:-}"
    FS_BUCKET="${AGENTTEAMS_FS_BUCKET:-agentteams-storage}"
    [ -n "${FS_ENDPOINT}" ] || { log "ERROR: AGENTTEAMS_FS_ENDPOINT is required"; exit 1; }
    [ -n "${FS_ACCESS_KEY}" ] || { log "ERROR: AGENTTEAMS_FS_ACCESS_KEY is required"; exit 1; }
    [ -n "${FS_SECRET_KEY}" ] || { log "ERROR: AGENTTEAMS_FS_SECRET_KEY is required"; exit 1; }
fi

# Set up skills CLI symlink: ~/.agents/skills -> worker's skills directory
# This makes `skills add -g` install skills into the worker's MinIO-synced skills/ dir
WORKER_SKILLS_DIR="${INSTALL_DIR}/${WORKER_NAME}/skills"
mkdir -p "${WORKER_SKILLS_DIR}"
mkdir -p "${HOME}/.agents"
ln -sfn "${WORKER_SKILLS_DIR}" "${HOME}/.agents/skills"

if [ -n "${CONSOLE_PORT}" ]; then
    # ---------- Standard mode: copaw-worker (PyPI CoPaw venv, with console) ----------
    VENV="/opt/venv/standard"
    log "Starting copaw-worker: ${WORKER_NAME}"
    log "  FS endpoint: ${FS_ENDPOINT}"
    log "  Install dir: ${INSTALL_DIR}"
    log "  Console port: ${CONSOLE_PORT}"
    log "  CoPaw: standard (${VENV})"

    exec "${VENV}/bin/copaw-worker" \
        --name "${WORKER_NAME}" \
        --fs "${FS_ENDPOINT}" \
        --fs-key "${FS_ACCESS_KEY}" \
        --fs-secret "${FS_SECRET_KEY}" \
        --fs-bucket "${FS_BUCKET}" \
        --install-dir "${INSTALL_DIR}" \
        --console-port "${CONSOLE_PORT}"
else
    # ---------- Lite mode: lite CoPaw venv, headless ----------
    VENV="/opt/venv/lite"
    log "Starting copaw-worker: ${WORKER_NAME}"
    log "  FS endpoint: ${FS_ENDPOINT}"
    log "  Install dir: ${INSTALL_DIR}"
    log "  CoPaw: lite (${VENV})"

    exec "${VENV}/bin/copaw-worker" \
        --name "${WORKER_NAME}" \
        --fs "${FS_ENDPOINT}" \
        --fs-key "${FS_ACCESS_KEY}" \
        --fs-secret "${FS_SECRET_KEY}" \
        --fs-bucket "${FS_BUCKET}" \
        --install-dir "${INSTALL_DIR}"
fi
