#!/bin/bash
# openhuman-worker-entrypoint.sh - OpenHuman Worker Agent container startup
# Bridges openclaw.json config into OpenHuman's TOML config,
# sets up MinIO file sync, launches openhuman-core with native Matrix support.
#
# Config bridging (in priority order):
#   1. openclaw.json  - channels.matrix.* + models.providers.hiclaw-gateway
#                       (pulled from MinIO, same source as hermes/copaw)
#   2. MATRIX_* / AGENTTEAMS_AI_GATEWAY_URL env vars  - controller-injected fallback
#
# Generated config.toml sections:
#   - [channels_config.matrix]
#       Native Matrix channel for direct human/manager interaction.
#   - LLM inference settings (via openhuman-core CLI):
#       Routes LLM traffic to the AgentTeams AI gateway (Higress); startup is
#       aborted (fail-closed) if the gateway config is missing.
#
# Environment variables (set by controller during worker creation):
#   AGENTTEAMS_WORKER_NAME        - Worker name (required)
#   AGENTTEAMS_FS_ENDPOINT        - MinIO endpoint (required in local mode)
#   AGENTTEAMS_FS_ACCESS_KEY      - MinIO access key (required in local mode)
#   AGENTTEAMS_FS_SECRET_KEY      - MinIO secret key (required in local mode)
#   AGENTTEAMS_RUNTIME            - "aliyun" for cloud mode (uses RRSA/STS)
#   AGENTTEAMS_AI_GATEWAY_URL     - AgentTeams AI gateway base URL (required)
#   AGENTTEAMS_WORKER_GATEWAY_KEY - Higress consumer key (required)
#   AGENTTEAMS_DEFAULT_MODEL      - Default model id (default qwen-plus)
#   MATRIX_HOMESERVER_URL         - Matrix homeserver URL (fallback)
#   MATRIX_ACCESS_TOKEN           - Matrix access token (fallback)
#   MATRIX_HOME_ROOM_ID           - Matrix room ID
#   MATRIX_ALLOWED_USERS          - Comma-separated allowed user IDs (fallback)
#   MATRIX_USER_ID                - Matrix user ID (fallback)
#   MATRIX_DEVICE_ID              - Matrix device ID (optional)
#   TZ                            - Timezone (optional)

set -e

# Source shared environment bootstrap (provides ensure_mc_credentials in cloud mode)
source /opt/hiclaw/scripts/lib/hiclaw-env.sh 2>/dev/null || true

WORKER_NAME="${AGENTTEAMS_WORKER_NAME:?AGENTTEAMS_WORKER_NAME is required}"
WORKER_CR_NAME="${AGENTTEAMS_WORKER_CR_NAME:-${WORKER_NAME}}"
WORKSPACE="${OPENHUMAN_WORKSPACE:-/home/openhuman/.openhuman}"

log() {
    echo "[hiclaw-openhuman-worker $(date '+%Y-%m-%d %H:%M:%S')] $1"
}

# ============================================================
# Step 0: Set timezone from TZ env var
# ============================================================
if [ -n "${TZ:-}" ] && [ -f "/usr/share/zoneinfo/${TZ}" ]; then
    ln -sf "/usr/share/zoneinfo/${TZ}" /etc/localtime
    echo "${TZ}" > /etc/timezone
    log "Timezone set to ${TZ}"
fi

# ============================================================
# Step 1: Configure mc alias for centralized file system
# ============================================================
if [ "${AGENTTEAMS_RUNTIME:-}" = "aliyun" ]; then
    log "Configuring mc alias for cloud (RRSA OIDC)..."
    ensure_mc_credentials || { log "ERROR: Failed to obtain OSS credentials"; exit 1; }
    FS_BUCKET="${AGENTTEAMS_FS_BUCKET:-hiclaw-cloud-storage}"
else
    FS_ENDPOINT="${AGENTTEAMS_FS_ENDPOINT:?AGENTTEAMS_FS_ENDPOINT is required}"
    FS_ACCESS_KEY="${AGENTTEAMS_FS_ACCESS_KEY:?AGENTTEAMS_FS_ACCESS_KEY is required}"
    FS_SECRET_KEY="${AGENTTEAMS_FS_SECRET_KEY:?AGENTTEAMS_FS_SECRET_KEY is required}"
    FS_BUCKET="${AGENTTEAMS_FS_BUCKET:-agentteams-storage}"
    log "Configuring mc alias for local MinIO (alias=${AGENTTEAMS_STORAGE_ALIAS})..."
    mc alias set "${AGENTTEAMS_STORAGE_ALIAS}" "${FS_ENDPOINT}" "${FS_ACCESS_KEY}" "${FS_SECRET_KEY}"
fi
log "  FS bucket: ${FS_BUCKET}"

# ============================================================
# Step 2: Pull Worker config from centralized storage
# ============================================================
mkdir -p "${WORKSPACE}" "${WORKSPACE}/shared" "${WORKSPACE}/memory" \
         "${WORKSPACE}/skills" "${WORKSPACE}/config"

log "Pulling Worker config from centralized storage..."
ensure_mc_credentials 2>/dev/null || true
mc mirror "${AGENTTEAMS_STORAGE_PREFIX}/agents/${WORKER_NAME}/" "${WORKSPACE}/agent-config/" \
    --overwrite 2>/dev/null || true
mc mirror "${AGENTTEAMS_STORAGE_PREFIX}/shared/" "${WORKSPACE}/shared/" \
    --overwrite 2>/dev/null || true

# Copy essential files from agent-config to workspace root
for _f in SOUL.md AGENTS.md MEMORY.md; do
    if [ -f "${WORKSPACE}/agent-config/${_f}" ]; then
        cp -f "${WORKSPACE}/agent-config/${_f}" "${WORKSPACE}/${_f}"
    fi
done

# Copy skills from agent-config
if [ -d "${WORKSPACE}/agent-config/skills" ]; then
    cp -rf "${WORKSPACE}/agent-config/skills/"* "${WORKSPACE}/skills/" 2>/dev/null || true
    find "${WORKSPACE}/skills" -name '*.sh' -exec chmod +x {} + 2>/dev/null || true
fi

# Copy memory files
if [ -d "${WORKSPACE}/agent-config/memory" ]; then
    cp -rf "${WORKSPACE}/agent-config/memory/"* "${WORKSPACE}/memory/" 2>/dev/null || true
fi

# Mark pull completion for sync loop
PULL_MARKER="${WORKSPACE}/.last-pull"
touch "${PULL_MARKER}"

# Verify essential files
RETRY=0
while [ ! -f "${WORKSPACE}/SOUL.md" ] || [ ! -f "${WORKSPACE}/AGENTS.md" ]; do
    RETRY=$((RETRY + 1))
    if [ "${RETRY}" -gt 6 ]; then
        log "WARNING: SOUL.md or AGENTS.md not found after retries. Continuing without them."
        break
    fi
    log "Waiting for config files to appear in MinIO (attempt ${RETRY}/6)..."
    sleep 5
    mc mirror "${AGENTTEAMS_STORAGE_PREFIX}/agents/${WORKER_NAME}/" "${WORKSPACE}/agent-config/" \
        --overwrite 2>/dev/null || true
    for _f in SOUL.md AGENTS.md; do
        [ -f "${WORKSPACE}/agent-config/${_f}" ] && cp -f "${WORKSPACE}/agent-config/${_f}" "${WORKSPACE}/${_f}"
    done
    touch "${PULL_MARKER}"
done

# Create symlink for skills CLI
mkdir -p "${HOME}/.agents"
ln -sfn "${WORKSPACE}/skills" "${HOME}/.agents/skills"

log "Worker config pulled successfully"

# ============================================================
# Step 3: Generate config.toml — Python bridge (openclaw.json → TOML)
# ============================================================
# Primary source: openclaw.json (channels.matrix.*) pulled from MinIO in Step 2.
# Fallback: MATRIX_* / AGENTTEAMS_* environment variables injected by controller.
log "Generating OpenHuman config.toml via openhuman-worker bridge..."

OPENCLAW_JSON="${WORKSPACE}/agent-config/openclaw.json"
openhuman-worker bridge \
    --workspace "${WORKSPACE}" \
    --openclaw-json "${OPENCLAW_JSON}" || {
    log "FATAL: openhuman-worker bridge failed"
    exit 1
}
export OPENHUMAN_CONFIG="${WORKSPACE}/config.toml"
log "config.toml generated at ${WORKSPACE}/config.toml"

# Generate a random core token if not set
export OPENHUMAN_CORE_TOKEN="${OPENHUMAN_CORE_TOKEN:-$(openssl rand -hex 32 2>/dev/null || head -c 64 /dev/urandom | od -A n -t x1 | tr -d ' \n')}"

# ============================================================
# Step 4: Start file sync loops
# ============================================================
# Semantics: design/sync-contract.md (OPENHUMAN preset).
# Push: memory/ → agents/{name}/memory/; shared/ → global shared/ with
# spec.md + base/ excludes (Manager-owned read-only paths).
# Pull: skills + shared fallback every 300s.

# Local → Remote: push changed files every 30 seconds
(
    while true; do
        sleep 30
        CHANGED=$(find "${WORKSPACE}/" -type f -newer "${PULL_MARKER}" \
            -not -path "*/config.toml" \
            -not -path "*/.last-pull" \
            -not -path "*/agent-config/*" \
            2>/dev/null | head -1)
        if [ -n "${CHANGED}" ]; then
            ensure_mc_credentials 2>/dev/null || true
            mc mirror "${WORKSPACE}/memory/" \
                "${AGENTTEAMS_STORAGE_PREFIX}/agents/${WORKER_NAME}/memory/" \
                --overwrite 2>/dev/null || true
            mc mirror "${WORKSPACE}/shared/" \
                "${AGENTTEAMS_STORAGE_PREFIX}/shared/" \
                --overwrite --exclude "*/spec.md" --exclude "*/base/*" 2>/dev/null || true
            # Push SOUL.md/AGENTS.md only if agent modified them
            for _mf in SOUL.md AGENTS.md MEMORY.md; do
                if [ -f "${WORKSPACE}/${_mf}" ] && [ "${WORKSPACE}/${_mf}" -nt "${PULL_MARKER}" ]; then
                    mc cp "${WORKSPACE}/${_mf}" \
                        "${AGENTTEAMS_STORAGE_PREFIX}/agents/${WORKER_NAME}/${_mf}" 2>/dev/null || true
                fi
            done
        fi
    done
) &
SYNC_LOCAL_PID=$!
log "Local->Remote sync started (PID: ${SYNC_LOCAL_PID})"

# Remote → Local: pull Manager-managed files every 5 minutes
(
    while true; do
        sleep 300
        ensure_mc_credentials 2>/dev/null || true
        mc mirror "${AGENTTEAMS_STORAGE_PREFIX}/agents/${WORKER_NAME}/skills/" \
            "${WORKSPACE}/skills/" --overwrite 2>/dev/null || true
        find "${WORKSPACE}/skills" -name '*.sh' -exec chmod +x {} + 2>/dev/null || true
        mc mirror "${AGENTTEAMS_STORAGE_PREFIX}/shared/" "${WORKSPACE}/shared/" \
            --overwrite --newer-than "5m" 2>/dev/null || true
        touch "${PULL_MARKER}"
    done
) &
SYNC_REMOTE_PID=$!
log "Remote->Local fallback sync started (PID: ${SYNC_REMOTE_PID})"

# ============================================================
# Step 5: Launch OpenHuman Core
# ============================================================

# Graceful shutdown handler
cleanup() {
    log "Shutting down..."
    kill ${CORE_PID} ${SYNC_LOCAL_PID} ${SYNC_REMOTE_PID} 2>/dev/null || true
    wait ${CORE_PID} 2>/dev/null || true
    log "Shutdown complete"
}
trap cleanup SIGTERM SIGINT

log "Starting OpenHuman Core: ${WORKER_NAME}"
export OPENHUMAN_CORE_HOST="${OPENHUMAN_CORE_HOST:-0.0.0.0}"
export OPENHUMAN_CORE_PORT="${OPENHUMAN_CORE_PORT:-7788}"
export OPENHUMAN_CONFIG="${WORKSPACE}/config.toml"

cd "${WORKSPACE}"
openhuman-core serve &
CORE_PID=$!

# ============================================================
# Step 6: Wait for health + report ready to controller
# ============================================================
(
    # Wait for openhuman-core to become healthy
    TIMEOUT=120; ELAPSED=0
    while [ "${ELAPSED}" -lt "${TIMEOUT}" ]; do
        if curl -sf "http://localhost:${OPENHUMAN_CORE_PORT}/health" >/dev/null 2>&1; then
            break
        fi
        sleep 3; ELAPSED=$((ELAPSED + 3))
    done

    if [ "${ELAPSED}" -ge "${TIMEOUT}" ]; then
        log "WARNING: readiness reporter timed out waiting for health after ${TIMEOUT}s"
        exit 1
    fi

    log "OpenHuman Core is healthy"

    # Report ready to controller
    if [ -n "${AGENTTEAMS_CONTROLLER_URL:-}" ]; then
        hiclaw worker report-ready --name "${WORKER_CR_NAME}" 2>/dev/null || \
            curl -sf -X POST "${AGENTTEAMS_CONTROLLER_URL}/api/v1/workers/${WORKER_CR_NAME}/ready" \
                -H "Content-Type: application/json" \
                -H "Authorization: Bearer $(cat ${AGENTTEAMS_AUTH_TOKEN_FILE:-/var/run/secrets/agentteams/token} 2>/dev/null)" 2>/dev/null || \
            log "WARNING: Failed to report ready to controller"
    fi
) &

# Wait for the main process
wait ${CORE_PID}
