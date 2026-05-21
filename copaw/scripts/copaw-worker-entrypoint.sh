#!/bin/bash
# copaw-worker-entrypoint.sh - CoPaw Worker Agent container startup
# Reads config from environment variables and launches copaw-worker.
#
# Environment variables (set by controller during worker creation):
#   HICLAW_WORKER_NAME   - Worker name (required)
#   HICLAW_FS_ENDPOINT   - MinIO endpoint (required in local mode)
#   HICLAW_FS_ACCESS_KEY - MinIO access key (required in local mode)
#   HICLAW_FS_SECRET_KEY - MinIO secret key (required in local mode)
#   HICLAW_RUNTIME       - "aliyun" for cloud mode (uses RRSA/STS via hiclaw-env.sh)
#   TZ                   - Timezone (optional)

set -e

# Source shared environment bootstrap (provides ensure_mc_credentials in cloud mode)
source /opt/hiclaw/scripts/lib/hiclaw-env.sh 2>/dev/null || true

WORKER_NAME="${HICLAW_WORKER_NAME:?HICLAW_WORKER_NAME is required}"
WORKER_CR_NAME="${HICLAW_WORKER_CR_NAME:-${WORKER_NAME}}"
# Align with openclaw/hermes: HOME == workspace == MinIO mirror root.
# The controller injects HOME=/root/hiclaw-fs/agents/<WORKER_NAME>; we set
# install_dir to its parent so that install_dir/<name> == HOME.
INSTALL_DIR="/root/hiclaw-fs/agents"

log() {
    echo "[hiclaw-copaw-worker $(date '+%Y-%m-%d %H:%M:%S')] $1"
}

# Set timezone from TZ env var
if [ -n "${TZ}" ] && [ -f "/usr/share/zoneinfo/${TZ}" ]; then
    ln -sf "/usr/share/zoneinfo/${TZ}" /etc/localtime
    echo "${TZ}" > /etc/timezone
    log "Timezone set to ${TZ}"
fi

# ── Credential setup ─────────────────────────────────────────────────────────
# Cloud mode: RRSA/STS credentials via MC_HOST_hiclaw (set by ensure_mc_credentials).
# FileSync._ensure_alias() detects MC_HOST_hiclaw and skips mc alias set.
# Local mode: explicit FS endpoint/key/secret passed via CLI args.
if [ "${HICLAW_RUNTIME:-}" = "aliyun" ]; then
    log "Cloud mode: configuring OSS credentials via RRSA..."
    ensure_mc_credentials || { log "ERROR: Failed to obtain OSS credentials"; exit 1; }
    # CLI requires --fs/--fs-key/--fs-secret but they are unused when MC_HOST_hiclaw is set
    FS_ENDPOINT="https://oss-placeholder.aliyuncs.com"
    FS_ACCESS_KEY="rrsa"
    FS_SECRET_KEY="rrsa"
    FS_BUCKET="${HICLAW_FS_BUCKET:-hiclaw-cloud-storage}"
    log "  OSS bucket: ${FS_BUCKET}"
else
    FS_ENDPOINT="${HICLAW_FS_ENDPOINT:?HICLAW_FS_ENDPOINT is required}"
    FS_ACCESS_KEY="${HICLAW_FS_ACCESS_KEY:?HICLAW_FS_ACCESS_KEY is required}"
    FS_SECRET_KEY="${HICLAW_FS_SECRET_KEY:?HICLAW_FS_SECRET_KEY is required}"
    FS_BUCKET="${HICLAW_FS_BUCKET:-hiclaw-storage}"
fi
log "  FS bucket: ${FS_BUCKET}"

# Workspace == HOME, so ~/skills is the real directory synced from MinIO.
# Expose it as ~/.agents/skills for tools that walk that legacy path.
mkdir -p "${INSTALL_DIR}/${WORKER_NAME}/skills" "${HOME}/.agents"
ln -sfn "${INSTALL_DIR}/${WORKER_NAME}/skills" "${HOME}/.agents/skills"

# Background readiness reporter — report ready to controller when CoPaw bridge completes
_start_readiness_reporter() {
    [ -z "${HICLAW_CONTROLLER_URL:-}" ] && return 0

    (
        # Phase 1: Wait for CoPaw config to be ready (with timeout)
        TIMEOUT=120; ELAPSED=0
        CONFIG_FILE="${INSTALL_DIR}/${WORKER_NAME}/.copaw/config.json"
        while [ "${ELAPSED}" -lt "${TIMEOUT}" ]; do
            if [ -f "${CONFIG_FILE}" ] && grep -q '"channels"' "${CONFIG_FILE}" 2>/dev/null; then
                break
            fi
            sleep 5; ELAPSED=$((ELAPSED + 5))
        done

        if [ "${ELAPSED}" -ge "${TIMEOUT}" ]; then
            log "WARNING: readiness reporter timed out waiting for config after ${TIMEOUT}s"
            exit 1
        fi

        # Report ready to controller via hiclaw CLI
        hiclaw worker report-ready --name "${WORKER_CR_NAME}"
    ) &
    log "Background readiness reporter started (PID: $!)"
}

VENV="/opt/venv/copaw"
log "Starting copaw-worker: ${WORKER_NAME}"
log "  Worker CR name: ${WORKER_CR_NAME}"
log "  FS endpoint: ${FS_ENDPOINT}"
log "  Install dir: ${INSTALL_DIR}"
log "  CoPaw venv: ${VENV}"

# Set COPAW_WORKING_DIR before starting (read by copaw.constant at import time)
export COPAW_WORKING_DIR="${INSTALL_DIR}/${WORKER_NAME}/.copaw"

# Enable debug logging for troubleshooting
export COPAW_LOG_LEVEL="${COPAW_LOG_LEVEL:-info}"

# ── CoPaw CMS Plugin Configuration ───────────────────────────────────────────
# Configure LoongSuite observability plugin if tracing is enabled
CMS_TRACES_ENABLED="$(echo "${HICLAW_CMS_TRACES_ENABLED:-false}" | tr '[:upper:]' '[:lower:]')"
if [ "${CMS_TRACES_ENABLED}" = "true" ]; then
    log "Configuring CoPaw CMS plugin..."
    LOONGSUITE_DIR="${HOME}/.loongsuite"
    mkdir -p "${LOONGSUITE_DIR}"

    cat > "${LOONGSUITE_DIR}/bootstrap-config.json" <<EOF
{
  "OTEL_EXPORTER_OTLP_ENDPOINT": "${HICLAW_CMS_ENDPOINT}",
  "OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
  "OTEL_EXPORTER_OTLP_HEADERS": "x-arms-license-key=${HICLAW_CMS_LICENSE_KEY},x-arms-project=${HICLAW_CMS_PROJECT},x-cms-workspace=${HICLAW_CMS_WORKSPACE}",
  "OTEL_SERVICE_NAME": "${HICLAW_CMS_SERVICE_NAME:-hiclaw-worker-${WORKER_NAME}}",
  "OTEL_SEMCONV_STABILITY_OPT_IN": "http,gen_ai_latest_experimental",
  "OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT": "SPAN_AND_EVENT",
  "LOONGSUITE_PYTHON_SITE_BOOTSTRAP": "true"
}
EOF
    log "CoPaw CMS plugin configured at ${LOONGSUITE_DIR}/bootstrap-config.json"
    export LOONGSUITE_PYTHON_SITE_BOOTSTRAP=true
fi

# Console port (default 8088, can be overridden via HICLAW_CONSOLE_PORT)
CONSOLE_PORT="${HICLAW_CONSOLE_PORT:-8088}"

# Build command
CMD_ARGS=(
    --name "${WORKER_NAME}"
    --cr-name "${WORKER_CR_NAME}"
    --fs "${FS_ENDPOINT}"
    --fs-key "${FS_ACCESS_KEY}"
    --fs-secret "${FS_SECRET_KEY}"
    --fs-bucket "${FS_BUCKET}"
    --install-dir "${INSTALL_DIR}"
    --console-port "${CONSOLE_PORT}"
)

log "  Console port: ${CONSOLE_PORT}"

_start_readiness_reporter

exec "${VENV}/bin/copaw-worker" "${CMD_ARGS[@]}"
