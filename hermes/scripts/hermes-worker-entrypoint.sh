#!/bin/bash
# hermes-worker-entrypoint.sh - Hermes Worker Agent container startup
# Reads config from environment variables and launches hermes-worker.
#
# Environment variables (set by controller during worker creation):
#   AGENTTEAMS_WORKER_NAME   - Worker name (required)
#   AGENTTEAMS_FS_ENDPOINT   - MinIO endpoint (required in local mode)
#   AGENTTEAMS_FS_ACCESS_KEY - MinIO access key (required in local mode)
#   AGENTTEAMS_FS_SECRET_KEY - MinIO secret key (required in local mode)
#   AGENTTEAMS_RUNTIME       - "aliyun" for cloud mode (uses RRSA/STS via hiclaw-env.sh)
#   TZ                   - Timezone (optional)

set -e

# Source shared environment bootstrap (provides ensure_mc_credentials in cloud mode)
source /opt/hiclaw/scripts/lib/hiclaw-env.sh 2>/dev/null || true

WORKER_NAME="${AGENTTEAMS_WORKER_NAME:-${HICLAW_WORKER_NAME:-}}"
[ -n "${WORKER_NAME}" ] || { echo "AGENTTEAMS_WORKER_NAME is required" >&2; exit 1; }
# Align with the openclaw worker layout: HOME == workspace == MinIO mirror root.
# The controller injects HOME=/root/hiclaw-fs/agents/<WORKER_NAME>; we anchor
# the install dir to its parent so workspace_dir == HOME and ${HERMES_HOME}
# == ${HOME}/.hermes/. This makes `cd ~ && ls` show openclaw.json / AGENTS.md /
# SOUL.md / skills / .hermes just like the openclaw worker.
INSTALL_DIR="/root/hiclaw-fs/agents"
WORKSPACE="${INSTALL_DIR}/${WORKER_NAME}"

log() {
    echo "[hiclaw-hermes-worker $(date '+%Y-%m-%d %H:%M:%S')] $1"
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
if [ "${AGENTTEAMS_RUNTIME:-${HICLAW_RUNTIME:-}}" = "aliyun" ]; then
    log "Cloud mode: configuring OSS credentials via RRSA..."
    ensure_mc_credentials || { log "ERROR: Failed to obtain OSS credentials"; exit 1; }
    FS_ENDPOINT="https://oss-placeholder.aliyuncs.com"
    FS_ACCESS_KEY="rrsa"
    FS_SECRET_KEY="rrsa"
    FS_BUCKET="${AGENTTEAMS_FS_BUCKET:-${HICLAW_FS_BUCKET:-hiclaw-cloud-storage}}"
    log "  OSS bucket: ${FS_BUCKET}"
else
    FS_ENDPOINT="${AGENTTEAMS_FS_ENDPOINT:-${HICLAW_FS_ENDPOINT:-}}"
    FS_ACCESS_KEY="${AGENTTEAMS_FS_ACCESS_KEY:-${HICLAW_FS_ACCESS_KEY:-}}"
    FS_SECRET_KEY="${AGENTTEAMS_FS_SECRET_KEY:-${HICLAW_FS_SECRET_KEY:-}}"
    FS_BUCKET="${AGENTTEAMS_FS_BUCKET:-${HICLAW_FS_BUCKET:-hiclaw-storage}}"
    [ -n "${FS_ENDPOINT}" ] || { log "ERROR: AGENTTEAMS_FS_ENDPOINT is required"; exit 1; }
    [ -n "${FS_ACCESS_KEY}" ] || { log "ERROR: AGENTTEAMS_FS_ACCESS_KEY is required"; exit 1; }
    [ -n "${FS_SECRET_KEY}" ] || { log "ERROR: AGENTTEAMS_FS_SECRET_KEY is required"; exit 1; }
fi
log "  FS bucket: ${FS_BUCKET}"

# Workspace == HOME, so ~/skills is the real directory hermes_worker syncs from
# MinIO. Mirror the openclaw convention of also exposing it as ~/.agents/skills
# for any tool that walks that legacy path.
mkdir -p "${WORKSPACE}/skills" "${HOME}/.agents"
# Use --no-dereference so we replace any pre-existing symlink-to-directory
# instead of nesting ~/.agents/skills/skills inside it.
ln -sfn "${WORKSPACE}/skills" "${HOME}/.agents/skills"

# Background readiness reporter — report ready once the bridge has produced
# the gateway's config.yaml (i.e. the worker can actually serve traffic).
_start_readiness_reporter() {
    [ -z "${HICLAW_CONTROLLER_URL:-}" ] && return 0

    (
        TIMEOUT=120; ELAPSED=0
        CONFIG_FILE="${WORKSPACE}/.hermes/config.yaml"
        while [ "${ELAPSED}" -lt "${TIMEOUT}" ]; do
            if [ -f "${CONFIG_FILE}" ] && grep -q '^matrix:' "${CONFIG_FILE}" 2>/dev/null; then
                break
            fi
            sleep 5; ELAPSED=$((ELAPSED + 5))
        done

        if [ "${ELAPSED}" -ge "${TIMEOUT}" ]; then
            log "WARNING: readiness reporter timed out waiting for config after ${TIMEOUT}s"
            exit 1
        fi

        hiclaw worker report-ready
    ) &
    log "Background readiness reporter started (PID: $!)"
}

VENV="/opt/venv/hermes"
log "Starting hermes-worker: ${WORKER_NAME}"
log "  FS endpoint: ${FS_ENDPOINT}"
log "  Install dir: ${INSTALL_DIR}"
log "  Hermes venv: ${VENV}"

# Hermes-agent reads its workspace from HERMES_HOME at process start.
export HERMES_HOME="${WORKSPACE}/.hermes"
mkdir -p "${HERMES_HOME}"

# ── Hermes runtime knobs for autonomous Worker operation ─────────────────────
# These match hermes-agent's own "container deployment" intent but it does not
# auto-detect that we're inside the worker pod (hermes only auto-bypasses when
# its terminal sandbox is set to "docker"; we run the local sandbox inside an
# already-isolated container, so it sees env_type=local and prompts for human
# approval on every dangerous command).
#
# 1) HERMES_YOLO_MODE=1 — bypass the dangerous-command approval gate.
#    The worker container is itself the security boundary; pausing on every
#    `rm -rf` would deadlock multi-Agent collaboration (the gate posts a
#    "/approve …" prompt to the room and waits indefinitely for a human or
#    coordinator that has no way to actually approve via Matrix).
# 2) MATRIX_HOME_CHANNEL=disabled — suppress the per-session "📬 No home
#    channel is set …" onboarding reminder. Workers don't deliver cron job
#    output and Manager doesn't have a UI to /sethome on the worker's behalf,
#    so the reminder is pure noise that fires every time a new room session
#    starts. Any non-empty value satisfies hermes-agent's check
#    (gateway/run.py: `if not os.getenv("MATRIX_HOME_CHANNEL")`).
#
# Both can still be overridden by the user via the openclaw env block.
: "${HERMES_YOLO_MODE:=1}"
: "${MATRIX_HOME_CHANNEL:=disabled}"
export HERMES_YOLO_MODE MATRIX_HOME_CHANNEL

# Hermes does not expose a dedicated Matrix trace env knob like OpenClaw's
# OPENCLAW_MATRIX_DEBUG. When HiClaw asks for Matrix debug logs, the bridge
# upgrades Hermes' config.yaml logging.level to DEBUG before the gateway starts.
if [ "${HICLAW_MATRIX_DEBUG:-}" = "1" ]; then
    log "HICLAW_MATRIX_DEBUG=1 detected; Hermes bridge will set logging.level=DEBUG for Matrix/gateway tracing"
fi

# ── Hermes CMS Plugin Configuration ──────────────────────────────────────────
# Pass observability env through to hermes-agent. Hermes uses standard OTel
# environment variables, so no per-app bootstrap file is required.
CMS_TRACES_ENABLED="$(echo "${HICLAW_CMS_TRACES_ENABLED:-false}" | tr '[:upper:]' '[:lower:]')"
if [ "${CMS_TRACES_ENABLED}" = "true" ]; then
    export OTEL_EXPORTER_OTLP_ENDPOINT="${HICLAW_CMS_ENDPOINT}"
    export OTEL_EXPORTER_OTLP_PROTOCOL="http/protobuf"
    export OTEL_EXPORTER_OTLP_HEADERS="x-arms-license-key=${HICLAW_CMS_LICENSE_KEY},x-arms-project=${HICLAW_CMS_PROJECT},x-cms-workspace=${HICLAW_CMS_WORKSPACE}"
    export OTEL_SERVICE_NAME="${HICLAW_CMS_SERVICE_NAME:-hiclaw-worker-${WORKER_NAME}}"
    export OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT="true"
    log "OTel exporter configured (endpoint=${HICLAW_CMS_ENDPOINT})"
fi

CMD_ARGS=(
    --name "${WORKER_NAME}"
    --fs "${FS_ENDPOINT}"
    --fs-key "${FS_ACCESS_KEY}"
    --fs-secret "${FS_SECRET_KEY}"
    --fs-bucket "${FS_BUCKET}"
    --install-dir "${INSTALL_DIR}"
)

_start_readiness_reporter

exec "${VENV}/bin/hermes-worker" "${CMD_ARGS[@]}"
