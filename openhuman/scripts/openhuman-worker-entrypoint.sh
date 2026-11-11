#!/bin/bash
# openhuman-worker-entrypoint.sh - OpenHuman Worker Agent container startup
# Bridges openclaw.json config into OpenHuman's TOML config,
# sets up MinIO file sync, launches openhuman-core with native Matrix support.
#
# Config bridging (in priority order):
#   1. openclaw.json  - channels.matrix.* + models.providers.agentteams-gateway
#                       (pulled from MinIO, same source as hermes/copaw)
#   2. AGENTTEAMS_* / MATRIX_* env vars  - controller-injected fallback
#
# Generated config.toml sections:
#   - [channels_config.matrix]
#       Native Matrix channel for direct human/manager interaction.
#   - LLM inference settings (via openhuman-core CLI):
#       Routes LLM traffic to the AgentTeams AI gateway (Higress); startup is
#       aborted (fail-closed) if the gateway config is missing.
#
# Environment variables (set by controller during worker creation):
#   AGENTTEAMS_WORKER_NAME         - Worker name (required)
#   AGENTTEAMS_FS_ENDPOINT         - MinIO endpoint (required in local mode)
#   AGENTTEAMS_FS_ACCESS_KEY       - MinIO access key (required in local mode)
#   AGENTTEAMS_FS_SECRET_KEY       - MinIO secret key (required in local mode)
#   AGENTTEAMS_RUNTIME             - "aliyun" for cloud mode (uses RRSA/STS)
#   AGENTTEAMS_AI_GATEWAY_URL      - AgentTeams AI gateway base URL (required)
#   AGENTTEAMS_WORKER_GATEWAY_KEY  - Higress consumer key (required)
#   AGENTTEAMS_DEFAULT_MODEL       - Default model id (default qwen-plus)
#   MATRIX_HOMESERVER_URL         - Matrix homeserver URL (fallback)
#   MATRIX_ACCESS_TOKEN           - Matrix access token (fallback)
#   MATRIX_HOME_ROOM_ID           - Matrix room ID
#   MATRIX_ALLOWED_USERS          - Comma-separated allowed user IDs (fallback)
#   MATRIX_USER_ID                - Matrix user ID (fallback)
#   MATRIX_DEVICE_ID              - Matrix device ID (optional)
#   TZ                            - Timezone (optional)

set -e

# Source shared environment bootstrap (provides ensure_mc_credentials in cloud mode)
source /opt/agentteams/scripts/lib/agentteams-env.sh 2>/dev/null || true

WORKER_NAME="${AGENTTEAMS_WORKER_NAME:?AGENTTEAMS_WORKER_NAME is required}"
WORKER_CR_NAME="${AGENTTEAMS_WORKER_CR_NAME:-${WORKER_NAME}}"
WORKSPACE="${OPENHUMAN_WORKSPACE:-/home/openhuman/.openhuman}"
if [ "${AGENTTEAMS_AGENT_ROLE:-}" = "manager" ]; then
    WORKSPACE="${AGENTTEAMS_WORKER_HOME:?AGENTTEAMS_WORKER_HOME is required}"
    export HOME="${WORKSPACE}"
    export OPENHUMAN_WORKSPACE="${WORKSPACE}"
fi

log() {
    echo "[agentteams-openhuman-worker $(date '+%Y-%m-%d %H:%M:%S')] $1"
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
    FS_BUCKET="${AGENTTEAMS_FS_BUCKET:-agentteams-storage}"
else
    FS_ENDPOINT="${AGENTTEAMS_FS_ENDPOINT:?AGENTTEAMS_FS_ENDPOINT is required}"
    FS_ACCESS_KEY="${AGENTTEAMS_FS_ACCESS_KEY:?AGENTTEAMS_FS_ACCESS_KEY is required}"
    FS_SECRET_KEY="${AGENTTEAMS_FS_SECRET_KEY:?AGENTTEAMS_FS_SECRET_KEY is required}"
    FS_BUCKET="${AGENTTEAMS_FS_BUCKET:-agentteams-storage}"
    log "Configuring mc alias for local MinIO..."
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

        BRIDGE_HOMESERVER="${_HS:-${AGENTTEAMS_MATRIX_URL:-${MATRIX_HOMESERVER_URL:-}}}"
        BRIDGE_ACCESS_TOKEN="${_AT:-${AGENTTEAMS_WORKER_MATRIX_TOKEN:-${MATRIX_ACCESS_TOKEN:-}}}"
        BRIDGE_USER_ID="${_UID:-${AGENTTEAMS_MATRIX_USER_ID:-${MATRIX_USER_ID:-}}}"
        BRIDGE_ROOM_ID="${AGENTTEAMS_WORKER_ROOM_ID:-${MATRIX_HOME_ROOM_ID:-}}"  # room_id is not in openclaw.json; always from env

        # Allowed users — merge dm.allowFrom + groupAllowFrom (deduplicated)
        BRIDGE_ALLOWED_USERS=$(
            jq -r '[
                (.channels.matrix.dm.allowFrom // [])[] ,
                (.channels.matrix.groupAllowFrom // [])[]
            ] | unique | .[]' "${OPENCLAW_JSON}" 2>/dev/null
        )
    fi

    # --- LLM provider config (AgentTeams AI gateway via Higress) ---
    # Maps openclaw.json's models.providers["agentteams-gateway"] +
    # agents.defaults.model.primary into OpenHuman's [[cloud_providers]]
    # and [model_routes] sections so that the worker routes LLM traffic
    # through Higress instead of falling back to api.openhuman.ai.
    BRIDGE_LLM_BASE_URL=$(jq -r '.models.providers["agentteams-gateway"].baseUrl // empty' "${OPENCLAW_JSON}")
    BRIDGE_LLM_API_KEY=$(jq -r '.models.providers["agentteams-gateway"].apiKey // empty' "${OPENCLAW_JSON}")
    # primary is "agentteams-gateway/<model>" — strip the provider prefix.
    BRIDGE_LLM_PRIMARY=$(jq -r '.agents.defaults.model.primary // empty | sub("^agentteams-gateway/"; "")' "${OPENCLAW_JSON}")
fi

# Apply fallback from env vars when openclaw.json was absent or incomplete.
BRIDGE_HOMESERVER="${BRIDGE_HOMESERVER:-${AGENTTEAMS_MATRIX_URL:-${MATRIX_HOMESERVER_URL:-}}}"
BRIDGE_ACCESS_TOKEN="${BRIDGE_ACCESS_TOKEN:-${AGENTTEAMS_WORKER_MATRIX_TOKEN:-${MATRIX_ACCESS_TOKEN:-}}}"
BRIDGE_ROOM_ID="${BRIDGE_ROOM_ID:-${AGENTTEAMS_WORKER_ROOM_ID:-${MATRIX_HOME_ROOM_ID:-}}}"
BRIDGE_USER_ID="${BRIDGE_USER_ID:-${AGENTTEAMS_MATRIX_USER_ID:-${MATRIX_USER_ID:-}}}"

# LLM fallback: AGENTTEAMS_AI_GATEWAY_URL is the base host (no /v1 suffix);
# AGENTTEAMS_WORKER_GATEWAY_KEY is the Higress consumer key for this worker.
if [ -z "${BRIDGE_LLM_BASE_URL:-}" ] && [ -n "${AGENTTEAMS_AI_GATEWAY_URL:-}" ]; then
    BRIDGE_LLM_BASE_URL="${AGENTTEAMS_AI_GATEWAY_URL%/}/v1"
fi
BRIDGE_LLM_API_KEY="${BRIDGE_LLM_API_KEY:-${AGENTTEAMS_WORKER_GATEWAY_KEY:-}}"
BRIDGE_LLM_PRIMARY="${BRIDGE_LLM_PRIMARY:-${AGENTTEAMS_DEFAULT_MODEL:-qwen-plus}}"

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

if [ -n "${WORKER_NAME}" ]; then
    # Native channel turns route through orchestrator, whose builtin definition
    # omits workspace identity and shell tools. Bind your assigned role explicitly.
    # OpenHuman resolves <config-dir>/workspace as its native workspace.
    NATIVE_ROLE="${AGENTTEAMS_AGENT_ROLE:-${AGENTTEAMS_WORKER_ROLE:-worker}}"
    NATIVE_WORKSPACE="${WORKSPACE}/workspace"
    mkdir -p "${NATIVE_WORKSPACE}/agents" "${NATIVE_WORKSPACE}/skills"
    write_member_prompt() {
        cat "${WORKSPACE}/AGENTS.md"
        printf '\nController member context:\nmember.name: %s\nmember.role: %s\nmember.personalRoomId: %s\nmember.matrixUserId: %s\n' \
            "${WORKER_CR_NAME}" "${NATIVE_ROLE}" "${BRIDGE_ROOM_ID}" "${BRIDGE_USER_ID}"
        printf '\nYour existing personality:\n'
        cat "${WORKSPACE}/SOUL.md"
        if [ -n "${AGENTTEAMS_STORAGE_PREFIX:-}" ]; then
            printf '\nYour shared storage prefix: %s\n' "${AGENTTEAMS_STORAGE_PREFIX}"
        fi
        printf '\nUse file_write to create files. Your shell policy blocks environment-variable expansion and output redirection; use literal paths with mc for uploads.\n'
    }
    # Channel turns load SOUL.md directly; the definition supplies tool scope.
    write_member_prompt > "${NATIVE_WORKSPACE}/SOUL.md"
    cp -a "${WORKSPACE}/skills/." "${NATIVE_WORKSPACE}/skills/"
    cat > "${NATIVE_WORKSPACE}/agents/orchestrator.toml" <<EOF
id = "orchestrator"
display_name = "${WORKER_NAME}"
when_to_use = "Complete tasks using your assigned AgentTeams role and credentials."
agent_tier = "chat"
max_iterations = 15
omit_identity = true
omit_skills_catalog = true
omit_memory_md = false

[system_prompt]
inline = "You are the AgentTeams ${NATIVE_ROLE} named ${WORKER_NAME}. Follow your assigned role and credentials, and use your available tools to complete tasks."

[tools]
named = ["shell", "file_read", "file_write", "query_memory", "memory_store", "memory_forget"]
EOF
    cat >> "${WORKSPACE}/config.toml" <<'EOF'

[autonomy]
allowed_commands = ["agt", "mc", "ls", "cat", "grep", "find", "echo", "pwd", "head", "tail"]
EOF
    if [ "${AGENTTEAMS_YOLO:-0}" = "1" ]; then
        printf '\nlevel = "full"\nrequire_approval_for_medium_risk = false\n' >> "${WORKSPACE}/config.toml"
    fi
fi

# --- LLM routing through AgentTeams AI gateway (Higress) ---
# Use openhuman-core's CLI to register the AgentTeams gateway as an
# OpenAI-compatible inference endpoint. This is REQUIRED for AgentTeams-managed
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
    log "FATAL: LLM gateway not configured (AGENTTEAMS_AI_GATEWAY_URL or AGENTTEAMS_WORKER_GATEWAY_KEY missing). AgentTeams-managed workers must route through the platform AI gateway; refusing to start to prevent silent fallback to external services."
    exit 1
fi

# Preserve CLI context outside the native shell's cleared environment. Keep
# credentials out of prompts and argv; the wrapper reads this private file.
umask 077
{
    printf 'export AGENTTEAMS_CONTROLLER_URL=%q\n' "${AGENTTEAMS_CONTROLLER_URL:-}"
    printf 'export AGENTTEAMS_AUTH_TOKEN_FILE=%q\n' "${AGENTTEAMS_AUTH_TOKEN_FILE:-}"
    if [ -n "${AGENTTEAMS_AUTH_TOKEN:-}" ]; then
        printf 'export AGENTTEAMS_AUTH_TOKEN=%q\n' "${AGENTTEAMS_AUTH_TOKEN}"
    fi
    printf 'export AGENTTEAMS_AGENT_ROLE=%q\n' "${AGENTTEAMS_AGENT_ROLE:-standalone}"
    printf 'export AGENTTEAMS_MATRIX_URL=%q\n' "${BRIDGE_HOMESERVER}"
    printf 'export AGENTTEAMS_WORKER_MATRIX_TOKEN=%q\n' "${BRIDGE_ACCESS_TOKEN}"
    if [ "${AGENTTEAMS_AGENT_ROLE:-}" = "manager" ]; then
        printf 'export AGENTTEAMS_MANAGER_MATRIX_TOKEN=%q\n' "${BRIDGE_ACCESS_TOKEN}"
    fi
} > "${HOME}/.agentteams-cli.env"

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
                "${AGENTTEAMS_STORAGE_PREFIX}/agents/${WORKER_NAME}/memory/" \
                --overwrite 2>/dev/null || true
            mc mirror "${WORKSPACE}/shared/" \
                "${AGENTTEAMS_STORAGE_PREFIX}/shared/" \
                --overwrite --exclude "spec.md" --exclude "base/" 2>/dev/null || true
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
        if [ "${AGENTTEAMS_AGENT_ROLE:-}" = "manager" ]; then
            sleep 5
            if mc cp "${AGENTTEAMS_STORAGE_PREFIX}/agents/${WORKER_NAME}/openclaw.json" "${WORKSPACE}/agent-config/openclaw.json.next" 2>/dev/null; then
                mv "${WORKSPACE}/agent-config/openclaw.json.next" "${WORKSPACE}/agent-config/openclaw.json"
            fi
            mc cp "${AGENTTEAMS_STORAGE_PREFIX}/agents/${WORKER_NAME}/AGENTS.md" "${WORKSPACE}/AGENTS.md" 2>/dev/null || true
            write_manager_prompt > "${NATIVE_WORKSPACE}/SOUL.md"
            cp -a "${WORKSPACE}/skills/." "${NATIVE_WORKSPACE}/skills/"
        else
            sleep 300
        fi
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
    # Your Manager Matrix adapter reports readiness after joining and syncing.
    if [ -n "${AGENTTEAMS_CONTROLLER_URL:-}" ] && [ "${AGENTTEAMS_AGENT_ROLE:-}" != "manager" ]; then
        agt worker report-ready --name "${WORKER_CR_NAME}" 2>/dev/null || \
            log "WARNING: Failed to report ready to controller"
    fi
) &

# Wait for the main process
wait ${CORE_PID}
