#!/bin/bash
# openhuman-worker-entrypoint.sh - OpenHuman Worker Agent container startup
# Bridges openclaw.json config into OpenHuman's TOML config,
# sets up MinIO file sync, launches openhuman-core with native Matrix support.
#
# Config bridging (in priority order):
#   1. openclaw.json  - channels.matrix.* + models.providers.hiclaw-gateway
#                       (pulled from MinIO, same source as hermes/copaw)
#   2. MATRIX_* / HICLAW_AI_GATEWAY_URL env vars  - controller-injected fallback
#
# Generated config.toml sections:
#   - [channels_config.matrix]
#       Native Matrix channel for direct human/manager interaction.
#   - LLM inference settings (via openhuman-core CLI):
#       Routes LLM traffic to the HiClaw AI gateway (Higress); startup is
#       aborted (fail-closed) if the gateway config is missing.
#
# Environment variables (set by controller during worker creation):
#   HICLAW_WORKER_NAME            - Worker name (required)
#   HICLAW_FS_ENDPOINT            - MinIO endpoint (required in local mode)
#   HICLAW_FS_ACCESS_KEY          - MinIO access key (required in local mode)
#   HICLAW_FS_SECRET_KEY          - MinIO secret key (required in local mode)
#   HICLAW_RUNTIME                - "aliyun" for cloud mode (uses RRSA/STS)
#   HICLAW_AI_GATEWAY_URL         - HiClaw AI gateway base URL (required)
#   HICLAW_WORKER_GATEWAY_KEY     - Higress consumer key (required)
#   HICLAW_DEFAULT_MODEL            - Default model id (default qwen-plus)
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

WORKER_NAME="${HICLAW_WORKER_NAME:?HICLAW_WORKER_NAME is required}"
WORKER_CR_NAME="${HICLAW_WORKER_CR_NAME:-${WORKER_NAME}}"
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
if [ "${HICLAW_RUNTIME:-}" = "aliyun" ]; then
    log "Configuring mc alias for cloud (RRSA OIDC)..."
    ensure_mc_credentials || { log "ERROR: Failed to obtain OSS credentials"; exit 1; }
    FS_BUCKET="${HICLAW_FS_BUCKET:-hiclaw-cloud-storage}"
else
    FS_ENDPOINT="${HICLAW_FS_ENDPOINT:?HICLAW_FS_ENDPOINT is required}"
    FS_ACCESS_KEY="${HICLAW_FS_ACCESS_KEY:?HICLAW_FS_ACCESS_KEY is required}"
    FS_SECRET_KEY="${HICLAW_FS_SECRET_KEY:?HICLAW_FS_SECRET_KEY is required}"
    FS_BUCKET="${HICLAW_FS_BUCKET:-hiclaw-storage}"
    log "Configuring mc alias for local MinIO..."
    mc alias set hiclaw "${FS_ENDPOINT}" "${FS_ACCESS_KEY}" "${FS_SECRET_KEY}"
fi
log "  FS bucket: ${FS_BUCKET}"

# ============================================================
# Step 2: Pull Worker config from centralized storage
# ============================================================
mkdir -p "${WORKSPACE}" "${WORKSPACE}/shared" "${WORKSPACE}/memory" \
         "${WORKSPACE}/skills" "${WORKSPACE}/config"

log "Pulling Worker config from centralized storage..."
ensure_mc_credentials 2>/dev/null || true
mc mirror "${HICLAW_STORAGE_PREFIX}/agents/${WORKER_NAME}/" "${WORKSPACE}/agent-config/" \
    --overwrite 2>/dev/null || true
mc mirror "${HICLAW_STORAGE_PREFIX}/shared/" "${WORKSPACE}/shared/" \
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
    mc mirror "${HICLAW_STORAGE_PREFIX}/agents/${WORKER_NAME}/" "${WORKSPACE}/agent-config/" \
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
# Step 3: Generate config.toml — bridge from openclaw.json
# ============================================================
# Primary source: openclaw.json (channels.matrix.*) pulled from MinIO in Step 2.
# Fallback: MATRIX_* environment variables injected by the controller.
# This keeps OpenHuman aligned with how hermes / copaw / openclaw runtimes
# consume Matrix configuration — via a single config artifact rather than
# per-field env vars.
log "Generating OpenHuman config.toml..."

OPENCLAW_JSON="${WORKSPACE}/agent-config/openclaw.json"

if [ -f "${OPENCLAW_JSON}" ] && command -v jq >/dev/null 2>&1; then
    log "Reading config from openclaw.json (bridge mode)"

    # --- Matrix config ---
    MATRIX_CFG=$(jq -r '.channels.matrix // empty' "${OPENCLAW_JSON}")
    if [ -n "${MATRIX_CFG}" ]; then
        _HS=$(jq -r '.channels.matrix.homeserver // empty' "${OPENCLAW_JSON}")
        _AT=$(jq -r '.channels.matrix.accessToken // empty' "${OPENCLAW_JSON}")
        _UID=$(jq -r '.channels.matrix.userId // empty' "${OPENCLAW_JSON}")

        BRIDGE_HOMESERVER="${_HS:-${MATRIX_HOMESERVER_URL:-}}"
        BRIDGE_ACCESS_TOKEN="${_AT:-${MATRIX_ACCESS_TOKEN:-}}"
        BRIDGE_USER_ID="${_UID:-${MATRIX_USER_ID:-}}"
        BRIDGE_ROOM_ID="${MATRIX_HOME_ROOM_ID:-}"  # room_id is not in openclaw.json; always from env

        # Allowed users — merge dm.allowFrom + groupAllowFrom (deduplicated)
        BRIDGE_ALLOWED_USERS=$(
            jq -r '[
                (.channels.matrix.dm.allowFrom // [])[] ,
                (.channels.matrix.groupAllowFrom // [])[]
            ] | unique | .[]' "${OPENCLAW_JSON}" 2>/dev/null
        )
    fi

    # --- LLM provider config (HiClaw AI gateway via Higress) ---
    # Maps openclaw.json's models.providers["hiclaw-gateway"] +
    # agents.defaults.model.primary into OpenHuman's [[cloud_providers]]
    # and [model_routes] sections so that the worker routes LLM traffic
    # through Higress instead of falling back to api.openhuman.ai.
    BRIDGE_LLM_BASE_URL=$(jq -r '.models.providers["hiclaw-gateway"].baseUrl // empty' "${OPENCLAW_JSON}")
    BRIDGE_LLM_API_KEY=$(jq -r '.models.providers["hiclaw-gateway"].apiKey // empty' "${OPENCLAW_JSON}")
    # primary is "hiclaw-gateway/<model>" — strip the provider prefix.
    BRIDGE_LLM_PRIMARY=$(jq -r '.agents.defaults.model.primary // empty | sub("^hiclaw-gateway/"; "")' "${OPENCLAW_JSON}")
fi

# Apply fallback from env vars when openclaw.json was absent or incomplete.
BRIDGE_HOMESERVER="${BRIDGE_HOMESERVER:-${MATRIX_HOMESERVER_URL:-}}"
BRIDGE_ACCESS_TOKEN="${BRIDGE_ACCESS_TOKEN:-${MATRIX_ACCESS_TOKEN:-}}"
BRIDGE_ROOM_ID="${BRIDGE_ROOM_ID:-${MATRIX_HOME_ROOM_ID:-}}"
BRIDGE_USER_ID="${BRIDGE_USER_ID:-${MATRIX_USER_ID:-}}"

# LLM fallback: HICLAW_AI_GATEWAY_URL is the base host (no /v1 suffix);
# HICLAW_WORKER_GATEWAY_KEY is the Higress consumer key for this worker.
if [ -z "${BRIDGE_LLM_BASE_URL:-}" ] && [ -n "${HICLAW_AI_GATEWAY_URL:-}" ]; then
    BRIDGE_LLM_BASE_URL="${HICLAW_AI_GATEWAY_URL%/}/v1"
fi
BRIDGE_LLM_API_KEY="${BRIDGE_LLM_API_KEY:-${HICLAW_WORKER_GATEWAY_KEY:-}}"
BRIDGE_LLM_PRIMARY="${BRIDGE_LLM_PRIMARY:-${HICLAW_DEFAULT_MODEL:-qwen-plus}}"

# If bridge didn't yield allowed users, fall back to MATRIX_ALLOWED_USERS env var.
if [ -z "${BRIDGE_ALLOWED_USERS:-}" ] && [ -n "${MATRIX_ALLOWED_USERS:-}" ]; then
    BRIDGE_ALLOWED_USERS=$(echo "${MATRIX_ALLOWED_USERS}" | tr ',' '\n')
fi

# Convert newline-separated user list to TOML array entries.
ALLOWED_USERS_TOML=""
if [ -n "${BRIDGE_ALLOWED_USERS:-}" ]; then
    ALLOWED_USERS_TOML=$(echo "${BRIDGE_ALLOWED_USERS}" | sed '/^$/d' | sed 's/.*/ "&",/' | sed '$ s/,$//')
fi

# Write Matrix-only config.toml first; LLM settings are applied below via
# openhuman-core CLI (which is the supported, schema-stable path).
cat > "${WORKSPACE}/config.toml" <<EOF
# Auto-generated by openhuman-worker-entrypoint.sh
# Do not edit manually — changes will be overwritten on container restart.

[channels_config]

[channels_config.matrix]
homeserver = "${BRIDGE_HOMESERVER}"
access_token = "${BRIDGE_ACCESS_TOKEN}"
room_id = "${BRIDGE_ROOM_ID}"
allowed_users = [
${ALLOWED_USERS_TOML}
]
$([ -n "${BRIDGE_USER_ID}" ] && echo "user_id = \"${BRIDGE_USER_ID}\"")
$([ -n "${MATRIX_DEVICE_ID:-}" ] && echo "device_id = \"${MATRIX_DEVICE_ID}\"")
EOF

log "config.toml generated at ${WORKSPACE}/config.toml"

# --- LLM routing through HiClaw AI gateway (Higress) ---
# Use openhuman-core's CLI to register the HiClaw gateway as an
# OpenAI-compatible inference endpoint. This is REQUIRED for HiClaw-managed
# workers; if not configured, the entrypoint aborts (fail-closed) to
# prevent silent routing of workloads to external services.
export OPENHUMAN_CONFIG="${WORKSPACE}/config.toml"
if [ -n "${BRIDGE_LLM_BASE_URL}" ] && [ -n "${BRIDGE_LLM_API_KEY}" ]; then
    log "Configuring LLM: endpoint=${BRIDGE_LLM_BASE_URL} model=${BRIDGE_LLM_PRIMARY}"
    openhuman-core config update_model_settings \
        --inference_url "${BRIDGE_LLM_BASE_URL}" \
        --api_key "${BRIDGE_LLM_API_KEY}" \
        --default_model "${BRIDGE_LLM_PRIMARY}" \
        >/dev/null 2>&1 || log "WARNING: openhuman-core config update_model_settings failed"
else
    log "FATAL: LLM gateway not configured (HICLAW_AI_GATEWAY_URL or HICLAW_WORKER_GATEWAY_KEY missing). HiClaw-managed workers must route through the platform AI gateway; refusing to start to prevent silent fallback to external services."
    exit 1
fi

# Generate a random core token if not set
export OPENHUMAN_CORE_TOKEN="${OPENHUMAN_CORE_TOKEN:-$(openssl rand -hex 32 2>/dev/null || head -c 64 /dev/urandom | od -A n -t x1 | tr -d ' \n')}"

# ============================================================
# Step 4: Start file sync loops
# ============================================================

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
                "${HICLAW_STORAGE_PREFIX}/agents/${WORKER_NAME}/memory/" \
                --overwrite 2>/dev/null || true
            mc mirror "${WORKSPACE}/shared/" \
                "${HICLAW_STORAGE_PREFIX}/shared/" \
                --overwrite --exclude "spec.md" --exclude "base/" 2>/dev/null || true
            # Push SOUL.md/AGENTS.md only if agent modified them
            for _mf in SOUL.md AGENTS.md MEMORY.md; do
                if [ -f "${WORKSPACE}/${_mf}" ] && [ "${WORKSPACE}/${_mf}" -nt "${PULL_MARKER}" ]; then
                    mc cp "${WORKSPACE}/${_mf}" \
                        "${HICLAW_STORAGE_PREFIX}/agents/${WORKER_NAME}/${_mf}" 2>/dev/null || true
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
        mc mirror "${HICLAW_STORAGE_PREFIX}/agents/${WORKER_NAME}/skills/" \
            "${WORKSPACE}/skills/" --overwrite 2>/dev/null || true
        find "${WORKSPACE}/skills" -name '*.sh' -exec chmod +x {} + 2>/dev/null || true
        mc mirror "${HICLAW_STORAGE_PREFIX}/shared/" "${WORKSPACE}/shared/" \
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
    if [ -n "${HICLAW_CONTROLLER_URL:-}" ]; then
        hiclaw worker report-ready --name "${WORKER_CR_NAME}" 2>/dev/null || \
            curl -sf -X POST "${HICLAW_CONTROLLER_URL}/api/v1/workers/${WORKER_CR_NAME}/ready" \
                -H "Content-Type: application/json" \
                -H "Authorization: Bearer $(cat ${HICLAW_AUTH_TOKEN_FILE:-/var/run/secrets/hiclaw/token} 2>/dev/null)" 2>/dev/null || \
            log "WARNING: Failed to report ready to controller"
    fi
) &

# Wait for the main process
wait ${CORE_PID}
