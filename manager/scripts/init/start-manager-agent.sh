#!/bin/bash
# start-manager-agent.sh - Initialize and start the Manager Agent
# Supports both local (supervisord) and cloud (SAE single-process) deployments.
# In local mode this is the last supervisord component to start (priority 800).
# In cloud mode (HICLAW_RUNTIME=aliyun) this is the container entrypoint.

source /opt/hiclaw/scripts/lib/hiclaw-env.sh

# ============================================================
# Set timezone from TZ env var
# ============================================================
if [ -n "${TZ}" ] && [ -f "/usr/share/zoneinfo/${TZ}" ]; then
    ln -sf "/usr/share/zoneinfo/${TZ}" /etc/localtime
    echo "${TZ}" > /etc/timezone
    log "Timezone set to ${TZ}"
fi

MATRIX_DOMAIN="${HICLAW_MATRIX_DOMAIN:-matrix-local.hiclaw.io:8080}"
AI_GATEWAY_DOMAIN="${HICLAW_AI_GATEWAY_DOMAIN:-aigw-local.hiclaw.io}"

# ============================================================
# Cloud mode: validate required environment variables + initial credentials
# ============================================================
if [ "${HICLAW_RUNTIME}" = "aliyun" ]; then
    : "${HICLAW_MATRIX_URL:?HICLAW_MATRIX_URL is required}"
    : "${HICLAW_MATRIX_DOMAIN:?HICLAW_MATRIX_DOMAIN is required}"
    : "${HICLAW_AI_GATEWAY_URL:?HICLAW_AI_GATEWAY_URL is required}"
    : "${HICLAW_MANAGER_GATEWAY_KEY:?HICLAW_MANAGER_GATEWAY_KEY is required}"
    : "${HICLAW_MANAGER_PASSWORD:?HICLAW_MANAGER_PASSWORD is required (cloud containers are stateless, password must be injected)}"
    : "${HICLAW_REGISTRATION_TOKEN:?HICLAW_REGISTRATION_TOKEN is required}"
    : "${HICLAW_ADMIN_USER:?HICLAW_ADMIN_USER is required}"
    : "${HICLAW_ADMIN_PASSWORD:?HICLAW_ADMIN_PASSWORD is required}"
    log "Cloud mode: validating environment... OK"
    log "  Matrix: ${HICLAW_MATRIX_SERVER}, AI Gateway: ${HICLAW_AI_GATEWAY_URL}, OSS: ${HICLAW_STORAGE_BUCKET}"
    ensure_mc_credentials || { log "FATAL: Initial STS credential fetch failed"; exit 1; }
fi

# ============================================================
# Local mode: host symlinks, /etc/hosts, wait for local services
# ============================================================
if [ "${HICLAW_RUNTIME}" != "aliyun" ]; then
    # Create symlink for host directory access
    if [ -d "/host-share" ]; then
        ORIGINAL_HOST_HOME="${HOST_ORIGINAL_HOME:-$HOME}"
        if [ ! -e "${ORIGINAL_HOST_HOME}" ] && [ "${ORIGINAL_HOST_HOME}" != "/" ] && [ "${ORIGINAL_HOST_HOME}" != "/root" ] && [ "${ORIGINAL_HOST_HOME}" != "/data" ] && [ "${ORIGINAL_HOST_HOME}" != "/host-share" ]; then
            mkdir -p "$(dirname "${ORIGINAL_HOST_HOME}")"
            ln -sfn /host-share "${ORIGINAL_HOST_HOME}"
            log "Created symlink: ${ORIGINAL_HOST_HOME} -> /host-share"
        else
            ln -sfn /host-share /root/host-home
            log "Created fallback symlink: /root/host-home -> /host-share"
        fi
    fi

    # Add local domains to /etc/hosts
    HOSTS_DOMAINS="${MATRIX_DOMAIN%%:*} ${HICLAW_MATRIX_CLIENT_DOMAIN:-matrix-client-local.hiclaw.io} ${AI_GATEWAY_DOMAIN} ${HICLAW_FS_DOMAIN:-fs-local.hiclaw.io}"
    if ! grep -q "${AI_GATEWAY_DOMAIN}" /etc/hosts 2>/dev/null; then
        echo "127.0.0.1 ${HOSTS_DOMAINS}" >> /etc/hosts
        log "Added local domains to /etc/hosts"
    fi

    # Wait for local infrastructure
    waitForService "Higress Gateway" "127.0.0.1" 8080 180
    waitForService "Higress Console" "127.0.0.1" 8001 180
    waitForService "Tuwunel" "127.0.0.1" 6167 120
    waitForHTTP "Tuwunel Matrix API" "${HICLAW_MATRIX_SERVER}/_tuwunel/server_version" 120
    waitForService "MinIO" "127.0.0.1" 9000 120
else
    # Cloud mode: wait for external Tuwunel
    log "Waiting for Tuwunel Matrix server at ${HICLAW_MATRIX_SERVER}..."
    _retry=0
    while [ "${_retry}" -lt 30 ]; do
        if curl -sf "${HICLAW_MATRIX_SERVER}/_matrix/client/versions" > /dev/null 2>&1; then
            log "Tuwunel is ready"
            break
        fi
        _retry=$((_retry + 1))
        log "  Waiting for Tuwunel (attempt ${_retry}/30)..."
        sleep 5
    done
    if [ "${_retry}" -ge 30 ]; then
        log "ERROR: Tuwunel not reachable at ${HICLAW_MATRIX_SERVER}"
        exit 1
    fi
fi

# ============================================================
# Auto-generate secrets if not provided via environment
# Persisted to /data so they survive container restart
# ============================================================
SECRETS_FILE="/data/hiclaw-secrets.env"
if [ -f "${SECRETS_FILE}" ]; then
    source "${SECRETS_FILE}"
    log "Loaded persisted secrets from ${SECRETS_FILE}"
fi

if [ -z "${HICLAW_MANAGER_GATEWAY_KEY}" ]; then
    export HICLAW_MANAGER_GATEWAY_KEY="$(generateKey 32)"
    log "Auto-generated HICLAW_MANAGER_GATEWAY_KEY"
fi
if [ -z "${HICLAW_MANAGER_PASSWORD}" ]; then
    export HICLAW_MANAGER_PASSWORD="$(generateKey 16)"
    log "Auto-generated HICLAW_MANAGER_PASSWORD"
fi

# Persist secrets so they survive supervisord restart
mkdir -p /data
cat > "${SECRETS_FILE}" <<EOF
export HICLAW_MANAGER_GATEWAY_KEY="${HICLAW_MANAGER_GATEWAY_KEY}"
export HICLAW_MANAGER_PASSWORD="${HICLAW_MANAGER_PASSWORD}"
EOF
chmod 600 "${SECRETS_FILE}"

# Cloud mode: pull workspace from OSS before initialization
if [ "${HICLAW_RUNTIME}" = "aliyun" ]; then
    HICLAW_FS="/root/hiclaw-fs"
    mkdir -p "${HICLAW_FS}/shared" "${HICLAW_FS}/agents"
    log "Pulling workspace from OSS..."
    ensure_mc_credentials
    mc mirror "${HICLAW_STORAGE_PREFIX}/manager/" /root/manager-workspace/ --overwrite 2>/dev/null || true
    mc mirror "${HICLAW_STORAGE_PREFIX}/shared/" "${HICLAW_FS}/shared/" --overwrite 2>/dev/null || true
    mc mirror "${HICLAW_STORAGE_PREFIX}/agents/" "${HICLAW_FS}/agents/" --overwrite 2>/dev/null || true
    # Symlink hiclaw-fs into workspace for agent access
    ln -sfn "${HICLAW_FS}" /root/manager-workspace/hiclaw-fs
fi

# ============================================================
# Initialize / upgrade Manager workspace
# First boot: full init via upgrade-builtins.sh
# Subsequent boots: compare image version; upgrade only if changed
# ============================================================
mkdir -p /root/manager-workspace

IMAGE_VERSION=$(cat /opt/hiclaw/agent/.builtin-version 2>/dev/null || echo "unknown")
INSTALLED_VERSION=$(cat /root/manager-workspace/.builtin-version 2>/dev/null || echo "")

if [ ! -f /root/manager-workspace/.initialized ]; then
    log "First boot: initializing manager workspace..."
    bash /opt/hiclaw/scripts/init/upgrade-builtins.sh
    touch /root/manager-workspace/.initialized
    log "Manager workspace initialized (version: ${IMAGE_VERSION})"
elif [ "${IMAGE_VERSION}" != "${INSTALLED_VERSION}" ] || [ "${IMAGE_VERSION}" = "latest" ]; then
    log "Upgrade detected: ${INSTALLED_VERSION} -> ${IMAGE_VERSION}${IMAGE_VERSION:+ (latest: always upgrade)}"
    bash /opt/hiclaw/scripts/init/upgrade-builtins.sh
    log "Manager workspace upgraded to version: ${IMAGE_VERSION}"
else
    log "Workspace up to date (version: ${IMAGE_VERSION})"
fi

# Local mode: wait for mc mirror initialization (shared + worker data in /root/hiclaw-fs/)
if [ "${HICLAW_RUNTIME}" != "aliyun" ]; then
    log "Waiting for MinIO storage initialization..."
    _minio_wait=0
    while [ ! -f /root/hiclaw-fs/.initialized ]; do
        sleep 2
        _minio_wait=$(( _minio_wait + 1 ))
        if [ "${_minio_wait}" -ge 60 ]; then
            log "ERROR: MinIO storage initialization timed out after 120s"
            exit 1
        fi
    done
    log "MinIO storage initialized"
fi

# ============================================================
# Register Matrix users via Registration API (single-step, no UIAA)
# ============================================================
log "Registering human admin Matrix account..."
curl -sf -X POST ${HICLAW_MATRIX_SERVER}/_matrix/client/v3/register \
    -H 'Content-Type: application/json' \
    -d '{
        "username": "'"${HICLAW_ADMIN_USER}"'",
        "password": "'"${HICLAW_ADMIN_PASSWORD}"'",
        "auth": {
            "type": "m.login.registration_token",
            "token": "'"${HICLAW_REGISTRATION_TOKEN}"'"
        }
    }' > /dev/null 2>&1 || log "Admin account may already exist"

log "Registering Manager Agent Matrix account..."
curl -sf -X POST ${HICLAW_MATRIX_SERVER}/_matrix/client/v3/register \
    -H 'Content-Type: application/json' \
    -d '{
        "username": "manager",
        "password": "'"${HICLAW_MANAGER_PASSWORD}"'",
        "auth": {
            "type": "m.login.registration_token",
            "token": "'"${HICLAW_REGISTRATION_TOKEN}"'"
        }
    }' > /dev/null 2>&1 || log "Manager account may already exist"

# Get Manager Agent's Matrix access token
log "Obtaining Manager Matrix access token..."
_LOGIN_RESPONSE=$(curl -sf -X POST ${HICLAW_MATRIX_SERVER}/_matrix/client/v3/login \
    -H 'Content-Type: application/json' \
    -d '{
        "type": "m.login.password",
        "identifier": {"type": "m.id.user", "user": "manager"},
        "password": "'"${HICLAW_MANAGER_PASSWORD}"'"
    }' 2>&1)
_LOGIN_EXIT=$?
log "Matrix login HTTP exit code: ${_LOGIN_EXIT}"
log "Matrix login response: ${_LOGIN_RESPONSE}"

MANAGER_TOKEN=$(echo "${_LOGIN_RESPONSE}" | jq -r '.access_token' 2>/dev/null)

if [ -z "${MANAGER_TOKEN}" ] || [ "${MANAGER_TOKEN}" = "null" ]; then
    log "ERROR: Failed to obtain Manager Matrix token (exit=${_LOGIN_EXIT})"
    log "ERROR: Login response was: ${_LOGIN_RESPONSE}"
    exit 1
fi
log "Manager Matrix token obtained (token prefix: ${MANAGER_TOKEN:0:10}...)"

# ============================================================
# Local mode: Initialize Higress Console + configure routes
# Cloud mode: Create admin DM room + schedule welcome message
# ============================================================
if [ "${HICLAW_RUNTIME}" != "aliyun" ]; then
    COOKIE_FILE="/tmp/higress-session-cookie"

    log "Waiting for Higress Console to be fully ready and initializing admin..."
    INIT_DONE=false
    for i in $(seq 1 90); do
        INIT_RESULT=$(curl -s -X POST http://127.0.0.1:8001/system/init \
            -H 'Content-Type: application/json' \
            -d '{"adminUser":{"name":"'"${HICLAW_ADMIN_USER}"'","password":"'"${HICLAW_ADMIN_PASSWORD}"'","displayName":"'"${HICLAW_ADMIN_USER}"'"}}' 2>/dev/null) || true
        if echo "${INIT_RESULT}" | grep -qE '"success":true|already.?init' 2>/dev/null; then
            INIT_DONE=true
            break
        fi
        if echo "${INIT_RESULT}" | grep -q '"name"' 2>/dev/null; then
            INIT_DONE=true
            break
        fi
        sleep 2
    done

    if [ "${INIT_DONE}" != "true" ]; then
        log "ERROR: Higress Console did not become ready within 180s"
        exit 1
    fi
    log "Higress Console init done"

    log "Logging into Higress Console..."
    LOGIN_OK=false
    for i in $(seq 1 10); do
        HTTP_CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST http://127.0.0.1:8001/session/login \
            -H 'Content-Type: application/json' \
            -c "${COOKIE_FILE}" \
            -d '{"username":"'"${HICLAW_ADMIN_USER}"'","password":"'"${HICLAW_ADMIN_PASSWORD}"'"}' 2>/dev/null) || true
        if { [ "${HTTP_CODE}" = "200" ] || [ "${HTTP_CODE}" = "201" ]; } && [ -f "${COOKIE_FILE}" ] && [ -s "${COOKIE_FILE}" ]; then
            LOGIN_OK=true
            break
        fi
        log "Login attempt $i (HTTP ${HTTP_CODE}), retrying in 3s..."
        sleep 3
    done

    if [ "${LOGIN_OK}" != "true" ]; then
        log "ERROR: Could not login to Higress Console after retries"
        exit 1
    fi
    log "Higress Console login successful"

    VERIFY_CODE=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8001/v1/consumers -b "${COOKIE_FILE}" 2>/dev/null) || true
    if [ "${VERIFY_CODE}" = "200" ]; then
        log "Console session verified (cookie valid)"
    else
        log "WARNING: Console session may be invalid (verify returned HTTP ${VERIFY_CODE})"
        rm -f "${COOKIE_FILE}"
        for i in $(seq 1 5); do
            curl -s -o /dev/null -w '%{http_code}' -X POST http://127.0.0.1:8001/session/login \
                -H 'Content-Type: application/json' \
                -c "${COOKIE_FILE}" \
                -d '{"username":"'"${HICLAW_ADMIN_USER}"'","password":"'"${HICLAW_ADMIN_PASSWORD}"'"}' 2>/dev/null
            VERIFY2=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8001/v1/consumers -b "${COOKIE_FILE}" 2>/dev/null) || true
            if [ "${VERIFY2}" = "200" ]; then
                log "Re-login successful, session verified"
                break
            fi
            sleep 2
        done
    fi

    export HIGRESS_COOKIE_FILE="${COOKIE_FILE}"

    # Configure Higress routes, consumers, MCP servers
    /opt/hiclaw/scripts/init/setup-higress.sh
fi

# ============================================================
# Create admin DM room, persist to state.json, send welcome message
# Runs in both local and cloud modes (idempotent)
# ============================================================
MANAGER_FULL_ID="@manager:${MATRIX_DOMAIN}"
ADMIN_FULL_ID="@${HICLAW_ADMIN_USER}:${MATRIX_DOMAIN}"

log "Logging in as admin to create DM room..."
_ADMIN_LOGIN=$(curl -sf -X POST "${HICLAW_MATRIX_SERVER}/_matrix/client/v3/login" \
    -H 'Content-Type: application/json' \
    -d '{
        "type": "m.login.password",
        "identifier": {"type": "m.id.user", "user": "'"${HICLAW_ADMIN_USER}"'"},
        "password": "'"${HICLAW_ADMIN_PASSWORD}"'"
    }' 2>&1) || true

ADMIN_MATRIX_TOKEN=$(echo "${_ADMIN_LOGIN}" | jq -r '.access_token // empty' 2>/dev/null)
if [ -z "${ADMIN_MATRIX_TOKEN}" ]; then
    log "WARNING: Failed to login as admin, skipping DM room creation"
else
    # Search for existing DM room with Manager (idempotent)
    DM_ROOM_ID=""
    _JOINED_ROOMS=$(curl -sf "${HICLAW_MATRIX_SERVER}/_matrix/client/v3/joined_rooms" \
        -H "Authorization: Bearer ${ADMIN_MATRIX_TOKEN}" 2>/dev/null \
        | jq -r '.joined_rooms[]' 2>/dev/null) || true
    for _rid in ${_JOINED_ROOMS}; do
        _members=$(curl -sf "${HICLAW_MATRIX_SERVER}/_matrix/client/v3/rooms/${_rid}/members" \
            -H "Authorization: Bearer ${ADMIN_MATRIX_TOKEN}" 2>/dev/null \
            | jq -r '.chunk[].state_key' 2>/dev/null) || continue
        _count=$(echo "${_members}" | wc -l | xargs)
        if [ "${_count}" = "2" ] && echo "${_members}" | grep -q "@manager:"; then
            DM_ROOM_ID="${_rid}"
            break
        fi
    done

    if [ -n "${DM_ROOM_ID}" ]; then
        log "Existing DM room found: ${DM_ROOM_ID}"
    else
        log "Creating DM room with Manager..."
        _RAW=$(curl -s -w '\nHTTP_CODE:%{http_code}' -X POST "${HICLAW_MATRIX_SERVER}/_matrix/client/v3/createRoom" \
            -H "Authorization: Bearer ${ADMIN_MATRIX_TOKEN}" \
            -H 'Content-Type: application/json' \
            -d "{\"is_direct\":true,\"invite\":[\"${MANAGER_FULL_ID}\"],\"preset\":\"trusted_private_chat\"}" 2>&1) || true
        _HTTP_CODE=$(echo "${_RAW}" | tail -1 | sed 's/HTTP_CODE://')
        _CREATE_RESP=$(echo "${_RAW}" | sed '$d')
        DM_ROOM_ID=$(echo "${_CREATE_RESP}" | jq -r '.room_id // empty' 2>/dev/null)
        if [ -n "${DM_ROOM_ID}" ]; then
            log "DM room created: ${DM_ROOM_ID}"
        else
            log "WARNING: Failed to create DM room (HTTP ${_HTTP_CODE}): ${_CREATE_RESP}"
        fi
    fi

    # Persist admin DM room ID to state.json
    if [ -n "${DM_ROOM_ID}" ]; then
        STATE_SCRIPT="/opt/hiclaw/agent/skills/task-management/scripts/manage-state.sh"
        if [ -f "${STATE_SCRIPT}" ]; then
            bash "${STATE_SCRIPT}" --action init 2>/dev/null || true
            bash "${STATE_SCRIPT}" --action set-admin-dm --room-id "${DM_ROOM_ID}" 2>/dev/null || true
            log "Admin DM room persisted to state.json: ${DM_ROOM_ID}"
        fi
    fi

    # Schedule welcome message in background (only on first boot)
    if [ -n "${DM_ROOM_ID}" ] && [ ! -f "/root/manager-workspace/soul-configured" ]; then
        log "Scheduling welcome message (background, waiting for OpenClaw to start)..."
        (
            _HICLAW_LANGUAGE="${HICLAW_LANGUAGE:-zh}"
            _HICLAW_TIMEZONE="${TZ:-Asia/Shanghai}"
            _wait=0
            _ready=false
            while [ "${_wait}" -lt 300 ]; do
                if curl -sf http://127.0.0.1:18799/ > /dev/null 2>&1; then
                    _ready=true
                    break
                fi
                sleep 3
                _wait=$((_wait + 3))
            done
            if [ "${_ready}" != "true" ]; then
                echo "[manager] WARNING: OpenClaw gateway not ready within 300s, skipping welcome message"
                exit 0
            fi
            # Ensure Manager has joined the DM room before sending the welcome
            # message.  Without this, there is a race between OpenClaw's Matrix
            # auto-join and the message send — the message may land before Manager
            # joins, so OpenClaw's /sync never picks it up.
            _join_ok=false
            for _join_attempt in 1 2 3; do
                if curl -sf -X POST "${HICLAW_MATRIX_SERVER}/_matrix/client/v3/rooms/${DM_ROOM_ID}/join" \
                    -H "Authorization: Bearer ${MANAGER_TOKEN}" \
                    -H 'Content-Type: application/json' \
                    -d '{}' > /dev/null 2>&1; then
                    echo "[manager] Manager joined DM room before welcome message"
                    _join_ok=true
                    break
                fi
                sleep 2
            done
            if [ "${_join_ok}" != "true" ]; then
                echo "[manager] WARNING: Manager join request failed after 3 attempts (may already be joined)"
            fi
            _welcome_msg="This is an automated message from the HiClaw setup. This is a fresh installation.

--- Installation Context ---
User Language: ${_HICLAW_LANGUAGE}  (zh = Chinese, en = English)
User Timezone: ${_HICLAW_TIMEZONE}  (IANA timezone identifier)
---

You are an AI agent that manages a team of worker agents. Your identity and personality have not been configured yet — the human admin is about to meet you for the first time.

Please begin the onboarding conversation:

1. Greet the admin warmly and briefly describe what you can do (coordinate workers, manage tasks, run multi-agent projects)
2. The user has selected \"${_HICLAW_LANGUAGE}\" as their preferred language during installation. Use this language for your greeting and all subsequent communication.
3. The user's timezone is ${_HICLAW_TIMEZONE}. Based on this timezone, you may infer their likely region and suggest additional language options.
4. Ask them: a) What would they like to call you? b) Communication style preference? c) Any behavior guidelines? d) Confirm default language
5. After they reply, write their preferences to ~/SOUL.md
6. Confirm what you wrote, and ask if they would like to adjust anything
7. Once confirmed, run: touch ~/soul-configured

The human admin will start chatting shortly."
            _txn_id="welcome-$(date +%s)"
            _payload=$(jq -nc --arg body "${_welcome_msg}" '{"msgtype":"m.text","body":$body}')
            _raw=$(curl -s -w '\nHTTP_CODE:%{http_code}' -X PUT "${HICLAW_MATRIX_SERVER}/_matrix/client/v3/rooms/${DM_ROOM_ID}/send/m.room.message/${_txn_id}" \
                -H "Authorization: Bearer ${ADMIN_MATRIX_TOKEN}" \
                -H 'Content-Type: application/json' \
                -d "${_payload}" 2>&1) || true
            _http_code=$(echo "${_raw}" | tail -1 | sed 's/HTTP_CODE://')
            _send_resp=$(echo "${_raw}" | sed '$d')
            if echo "${_send_resp}" | jq -e '.event_id' > /dev/null 2>&1; then
                echo "[manager] Welcome message sent to DM room"
            else
                echo "[manager] WARNING: Failed to send welcome message (HTTP ${_http_code}): ${_send_resp}"
            fi
        ) &
        log "Welcome message background process started (PID: $!)"
    fi
fi

# ============================================================
# Generate Manager Agent openclaw.json from template
# ============================================================
log "Generating Manager openclaw.json..."
export MANAGER_MATRIX_TOKEN="${MANAGER_TOKEN}"
export MANAGER_GATEWAY_KEY="${HICLAW_MANAGER_GATEWAY_KEY}"
# Resolve model parameters based on model name
MODEL_NAME="${HICLAW_DEFAULT_MODEL:-qwen3.5-plus}"
case "${MODEL_NAME}" in
    gpt-5.3-codex|gpt-5-mini|gpt-5-nano)
        export MODEL_CONTEXT_WINDOW=400000 MODEL_MAX_TOKENS=128000 ;;
    claude-opus-4-6)
        export MODEL_CONTEXT_WINDOW=1000000 MODEL_MAX_TOKENS=128000 ;;
    claude-sonnet-4-6)
        export MODEL_CONTEXT_WINDOW=1000000 MODEL_MAX_TOKENS=64000 ;;
    claude-haiku-4-5)
        export MODEL_CONTEXT_WINDOW=200000 MODEL_MAX_TOKENS=64000 ;;
    qwen3.5-plus)
        export MODEL_CONTEXT_WINDOW=200000 MODEL_MAX_TOKENS=64000 ;;
    deepseek-chat|deepseek-reasoner|kimi-k2.5)
        export MODEL_CONTEXT_WINDOW=256000 MODEL_MAX_TOKENS=128000 ;;
    glm-5|MiniMax-M2.7|MiniMax-M2.7-highspeed|MiniMax-M2.5)
        export MODEL_CONTEXT_WINDOW=200000 MODEL_MAX_TOKENS=128000 ;;
    *)
        export MODEL_CONTEXT_WINDOW=150000 MODEL_MAX_TOKENS=128000 ;;
esac
export MODEL_REASONING=true

# Override with user-supplied custom model parameters from env (set during install)
[ -n "${HICLAW_MODEL_CONTEXT_WINDOW:-}" ] && export MODEL_CONTEXT_WINDOW="${HICLAW_MODEL_CONTEXT_WINDOW}"
[ -n "${HICLAW_MODEL_MAX_TOKENS:-}" ] && export MODEL_MAX_TOKENS="${HICLAW_MODEL_MAX_TOKENS}"
[ -n "${HICLAW_MODEL_REASONING:-}" ] && export MODEL_REASONING="${HICLAW_MODEL_REASONING}"

# E2EE: convert HICLAW_MATRIX_E2EE to JSON boolean for template substitution
if [ "${HICLAW_MATRIX_E2EE:-0}" = "1" ] || [ "${HICLAW_MATRIX_E2EE:-}" = "true" ]; then
    export MATRIX_E2EE_ENABLED=true
else
    export MATRIX_E2EE_ENABLED=false
fi
log "Matrix E2EE: ${MATRIX_E2EE_ENABLED}"

# Resolve input modalities: only vision-capable models get "image"
case "${MODEL_NAME}" in
    gpt-5.4|gpt-5.3-codex|gpt-5-mini|gpt-5-nano|claude-opus-4-6|claude-sonnet-4-6|claude-haiku-4-5|qwen3.5-plus|kimi-k2.5)
        export MODEL_INPUT='["text", "image"]' ;;
    *)
        export MODEL_INPUT='["text"]' ;;
esac
# Override with user-supplied vision setting from env
if [ "${HICLAW_MODEL_VISION:-}" = "true" ]; then
    export MODEL_INPUT='["text", "image"]'
elif [ "${HICLAW_MODEL_VISION:-}" = "false" ]; then
    export MODEL_INPUT='["text"]'
fi

log "Model: ${MODEL_NAME} (context=${MODEL_CONTEXT_WINDOW}, maxTokens=${MODEL_MAX_TOKENS}, reasoning=${MODEL_REASONING}, input=${MODEL_INPUT})"

if [ -f /root/manager-workspace/openclaw.json ]; then
    log "Manager openclaw.json already exists, updating dynamic fields only (preserving user customizations)..."
    # Merge known models into existing config (add missing, preserve user-added)
    # Use known-models.json (valid JSON) instead of template (contains ${VAR} placeholders)
    KNOWN_MODELS=$(cat /opt/hiclaw/configs/known-models.json 2>/dev/null || echo '[]')
    jq --arg token "${MANAGER_TOKEN}" \
       --arg key "${HICLAW_MANAGER_GATEWAY_KEY}" \
       --arg model "${MODEL_NAME}" \
       --argjson e2ee "${MATRIX_E2EE_ENABLED}" \
       --argjson known_models "${KNOWN_MODELS}" \
       --argjson ctx "${MODEL_CONTEXT_WINDOW}" \
       --argjson max "${MODEL_MAX_TOKENS}" \
       --argjson reasoning "${MODEL_REASONING}" \
       --argjson input "${MODEL_INPUT}" \
       '
        # Merge known models: add any model id not already present
        .models.providers["hiclaw-gateway"].models as $existing
        | ($existing | map(.id)) as $existing_ids
        | ($known_models | map(select(.id as $id | $existing_ids | index($id) | not))) as $new
        | .models.providers["hiclaw-gateway"].models = ($existing + $new)
        # Ensure the user-chosen default model is in the list (custom model support)
        | if (.models.providers["hiclaw-gateway"].models | map(.id) | index($model) | not) then
            .models.providers["hiclaw-gateway"].models += [{"id": $model, "name": $model, "reasoning": $reasoning, "contextWindow": $ctx, "maxTokens": $max, "input": $input}]
          else . end
        # Rebuild model aliases from the full models list
        | (.models.providers["hiclaw-gateway"].models | map({ ("hiclaw-gateway/" + .id): { "alias": .id } }) | add // {}) as $aliases
        | .agents.defaults.models = ((.agents.defaults.models // {}) + $aliases)
        | .channels.matrix.accessToken = $token | .models.providers["hiclaw-gateway"].apiKey = $key
        | ((.hooks.token // "") as $ht | if $ht == $key or $ht == ($key + "-hooks" | @base64) then del(.hooks) else . end)
        | .agents.defaults.model.primary = ("hiclaw-gateway/" + $model)
        | .commands.restart = true
        | .gateway.controlUi.dangerouslyDisableDeviceAuth = true
        | .channels.matrix.encryption = $e2ee
       ' \
       /root/manager-workspace/openclaw.json > /tmp/openclaw.json.tmp && \
        mv /tmp/openclaw.json.tmp /root/manager-workspace/openclaw.json
    # Verify the token was written correctly
    _written_token=$(jq -r '.channels.matrix.accessToken' /root/manager-workspace/openclaw.json 2>/dev/null)
    if [ -z "${_written_token}" ] || [ "${_written_token}" = "null" ]; then
        log "ERROR: Matrix token was not written correctly to openclaw.json (got: ${_written_token})"
    else
        log "Matrix token written to openclaw.json (prefix: ${_written_token:0:10}...)"
    fi
else
    log "Manager openclaw.json not found, generating from template..."
    envsubst < /opt/hiclaw/configs/manager-openclaw.json.tmpl > /root/manager-workspace/openclaw.json
    # Inject custom model if not in the built-in list
    if ! jq -e --arg model "${MODEL_NAME}" '.models.providers["hiclaw-gateway"].models | map(.id) | index($model)' /root/manager-workspace/openclaw.json > /dev/null 2>&1; then
        log "Custom model '${MODEL_NAME}' not in built-in list, injecting into config..."
        jq --arg model "${MODEL_NAME}" \
           --argjson ctx "${MODEL_CONTEXT_WINDOW}" \
           --argjson max "${MODEL_MAX_TOKENS}" \
           --argjson reasoning "${MODEL_REASONING}" \
           --argjson input "${MODEL_INPUT}" \
           '
            .models.providers["hiclaw-gateway"].models += [{"id": $model, "name": $model, "reasoning": $reasoning, "contextWindow": $ctx, "maxTokens": $max, "input": $input}]
            | .agents.defaults.models += {("hiclaw-gateway/" + $model): {"alias": $model}}
           ' /root/manager-workspace/openclaw.json > /tmp/openclaw.json.tmp && \
            mv /tmp/openclaw.json.tmp /root/manager-workspace/openclaw.json
    fi
    _written_token=$(jq -r '.channels.matrix.accessToken' /root/manager-workspace/openclaw.json 2>/dev/null)
    log "Matrix token written from template (prefix: ${_written_token:0:10}...)"
fi

# Cloud mode: overlay cloud-specific settings onto generated config
if [ "${HICLAW_RUNTIME}" = "aliyun" ]; then
    log "Applying cloud overlay to openclaw.json..."
    jq --arg homeserver "${HICLAW_MATRIX_SERVER}" \
       --arg gateway "${HICLAW_AI_GATEWAY_URL}/v1" \
       --arg key "${HICLAW_MANAGER_GATEWAY_KEY}" \
       '.channels.matrix.homeserver = $homeserver
        | .models.providers["hiclaw-gateway"].baseUrl = $gateway
        | .models.providers["hiclaw-gateway"].apiKey = $key
        | ((.hooks.token // "") as $ht | if $ht == $key or $ht == ($key + "-hooks" | @base64) then del(.hooks) else . end)
        | .commands.restart = false' \
       /root/manager-workspace/openclaw.json > /tmp/openclaw-cloud.json && \
        mv /tmp/openclaw-cloud.json /root/manager-workspace/openclaw.json
    log "Cloud overlay applied"
fi

# ============================================================
# Optional: enable openclaw-cms-plugin observability
# Config is applied at runtime so secrets stay out of image layers.
# ============================================================
CMS_TRACES_ENABLED="$(echo "${HICLAW_CMS_TRACES_ENABLED:-false}" | tr '[:upper:]' '[:lower:]')"
if [ "${CMS_TRACES_ENABLED}" = "true" ]; then
    CMS_PLUGIN_NAME="openclaw-cms-plugin"
    CMS_PLUGIN_DIR="${OPENCLAW_CMS_PLUGIN_DIR:-/opt/openclaw/extensions/openclaw-cms-plugin}"
    CMS_PLUGIN_MANIFEST="${CMS_PLUGIN_DIR}/openclaw.plugin.json"
    DIAG_PLUGIN_NAME="diagnostics-otel"
    DIAG_PLUGIN_DIR="/opt/openclaw/extensions/diagnostics-otel"
    CMS_LICENSE_KEY="${HICLAW_CMS_LICENSE_KEY:-}"
    CMS_PROJECT="${HICLAW_CMS_PROJECT:-}"
    CMS_METRICS_ENABLED="${HICLAW_CMS_METRICS_ENABLED:-false}"

    if [ ! -f "${CMS_PLUGIN_MANIFEST}" ]; then
        log "WARNING: ${CMS_PLUGIN_NAME} manifest not found at ${CMS_PLUGIN_MANIFEST}, skipping plugin config"
    else
        _missing=0
        [ -z "${HICLAW_CMS_ENDPOINT:-}" ] && log "WARNING: HICLAW_CMS_ENDPOINT is required when HICLAW_CMS_TRACES_ENABLED=true" && _missing=1
        [ -z "${CMS_LICENSE_KEY:-}" ] && log "WARNING: HICLAW_CMS_LICENSE_KEY is required when HICLAW_CMS_TRACES_ENABLED=true" && _missing=1
        [ -z "${HICLAW_CMS_WORKSPACE:-}" ] && log "WARNING: HICLAW_CMS_WORKSPACE is required when HICLAW_CMS_TRACES_ENABLED=true" && _missing=1

        if [ "${_missing}" = "0" ]; then
            CMS_SERVICE_NAME="${HICLAW_CMS_SERVICE_NAME:-hiclaw-manager}"
            CMS_ENABLE_METRICS="${CMS_METRICS_ENABLED}"
            DIAG_AVAILABLE="0"
            _metrics_lc="$(echo "${CMS_ENABLE_METRICS}" | tr '[:upper:]' '[:lower:]')"
            if [ "${_metrics_lc}" = "true" ]; then
                if [ -f "${DIAG_PLUGIN_DIR}/package.json" ]; then
                    DIAG_AVAILABLE="1"
                    if [ ! -d "${DIAG_PLUGIN_DIR}/node_modules" ]; then
                        log "diagnostics-otel dependencies missing, installing..."
                        if (cd "${DIAG_PLUGIN_DIR}" && npm install --omit=dev --ignore-scripts >/tmp/hiclaw-diag-install.log 2>&1); then
                            log "diagnostics-otel dependencies installed"
                        else
                            log "WARNING: diagnostics-otel npm install failed, metrics plugin may not load"
                        fi
                    else
                        log "diagnostics-otel dependencies already present"
                    fi
                else
                    log "WARNING: diagnostics-otel package.json not found at ${DIAG_PLUGIN_DIR}, metrics plugin may not load"
                fi
            fi

            log "Applying ${CMS_PLUGIN_NAME} config to openclaw.json..."
            jq --arg pluginName "${CMS_PLUGIN_NAME}" \
               --arg pluginDir "${CMS_PLUGIN_DIR}" \
               --arg endpoint "${HICLAW_CMS_ENDPOINT}" \
               --arg licenseKey "${CMS_LICENSE_KEY}" \
               --arg armsProject "${CMS_PROJECT}" \
               --arg cmsWorkspace "${HICLAW_CMS_WORKSPACE}" \
               --arg serviceName "${CMS_SERVICE_NAME}" \
               --arg diagPluginName "${DIAG_PLUGIN_NAME}" \
               --arg diagPluginDir "${DIAG_PLUGIN_DIR}" \
               --arg metricsRaw "${CMS_ENABLE_METRICS}" \
               --arg diagAvailableRaw "${DIAG_AVAILABLE}" \
               '
                .plugins = (.plugins // {})
                | .plugins.load = (.plugins.load // {})
                | .plugins.entries = (.plugins.entries // {})
                | if (.plugins.allow | type) != "array" then .plugins.allow = [] else . end
                | if (.plugins.allow | index($pluginName)) == null then .plugins.allow += [$pluginName] else . end
                | if (.plugins.load.paths | type) != "array" then .plugins.load.paths = [] else . end
                | if (.plugins.load.paths | index($pluginDir)) == null then .plugins.load.paths += [$pluginDir] else . end
                | .plugins.entries[$pluginName] = {
                    "enabled": true,
                    "config": {
                        "endpoint": $endpoint,
                        "headers": {
                            "x-arms-license-key": $licenseKey,
                            "x-arms-project": $armsProject,
                            "x-cms-workspace": $cmsWorkspace
                        },
                        "serviceName": $serviceName
                    }
                }

                # diagnostics-otel metrics (optional)
                | ($metricsRaw | ascii_downcase) as $m
                | ($diagAvailableRaw == "1") as $diagAvailable
                | (($m == "true") and $diagAvailable) as $metricsEnabled
                | if $metricsEnabled then
                    (if (.plugins.allow | index($diagPluginName)) == null then .plugins.allow += [$diagPluginName] else . end)
                    | (if (.plugins.load.paths | index($diagPluginDir)) == null then .plugins.load.paths += [$diagPluginDir] else . end)
                    | .plugins.entries[$diagPluginName].enabled = true
                    | .diagnostics = (.diagnostics // {})
                    | .diagnostics.otel = (.diagnostics.otel // {})
                    | .diagnostics.enabled = true
                    | .diagnostics.otel.enabled = true
                    | .diagnostics.otel.endpoint = $endpoint
                    | .diagnostics.otel.protocol = (.diagnostics.otel.protocol // "http/protobuf")
                    | .diagnostics.otel.headers = {
                        "x-arms-license-key": $licenseKey,
                        "x-arms-project": $armsProject,
                        "x-cms-workspace": $cmsWorkspace
                    }
                    | .diagnostics.otel.serviceName = $serviceName
                    | .diagnostics.otel.metrics = true
                    | .diagnostics.otel.traces = (.diagnostics.otel.traces // false)
                    | .diagnostics.otel.logs = (.diagnostics.otel.logs // false)
                  else
                    .
                  end
               ' /root/manager-workspace/openclaw.json > /tmp/openclaw-cms.json && \
                mv /tmp/openclaw-cms.json /root/manager-workspace/openclaw.json
            log "${CMS_PLUGIN_NAME} config applied (metrics=${CMS_ENABLE_METRICS}, service=${CMS_SERVICE_NAME})"
        else
            log "Skipping ${CMS_PLUGIN_NAME} config due to missing required env vars"
        fi
    fi
fi

# ============================================================
# Detect container runtime (for Worker creation)
# ============================================================
source /opt/hiclaw/scripts/lib/container-api.sh
if container_api_available; then
    log "Container runtime socket detected at ${CONTAINER_SOCKET} — direct Worker creation enabled"
    export HICLAW_CONTAINER_RUNTIME="socket"
elif [ "${HICLAW_RUNTIME}" = "aliyun" ]; then
    log "Cloud mode — Workers created via SAE API"
    export HICLAW_CONTAINER_RUNTIME="cloud"
else
    log "No container runtime found — Worker creation will output install commands"
    export HICLAW_CONTAINER_RUNTIME="none"
fi

# ============================================================
# Upgrade Worker openclaw.json: merge known models + E2EE flag into existing configs
# Existing workers in MinIO may have old single-model configs or missing encryption field.
# Merge template models so they can hot-switch without restart.
# ============================================================
REGISTRY_FILE="/root/manager-workspace/workers-registry.json"
if [ -f "${REGISTRY_FILE}" ]; then
    # Use known-models.json (valid JSON) instead of template (contains ${VAR} placeholders)
    KNOWN_MODELS_FILE="/opt/hiclaw/configs/known-models.json"
    if [ -f "${KNOWN_MODELS_FILE}" ]; then
        _KNOWN_MODELS=$(cat "${KNOWN_MODELS_FILE}")
        for _wname in $(jq -r '.workers | keys[]' "${REGISTRY_FILE}" 2>/dev/null); do
            [ -z "${_wname}" ] && continue
            _minio_path="${HICLAW_STORAGE_PREFIX}/agents/${_wname}/openclaw.json"
            _tmp_in="/tmp/openclaw-${_wname}-models-upgrade-in.json"
            if mc cp "${_minio_path}" "${_tmp_in}" 2>/dev/null; then
                _tmp_out="/tmp/openclaw-${_wname}-models-upgrade-out.json"
                # Idempotent merge: add missing known models, rebuild aliases, set e2ee.
                # Always runs — jq deduplicates by model id, so re-runs are safe.
                jq --argjson known_models "${_KNOWN_MODELS}" \
                   --argjson e2ee "${MATRIX_E2EE_ENABLED}" '
                    .models.providers["hiclaw-gateway"].models as $existing
                    | ($existing | map(.id)) as $existing_ids
                    | ($known_models | map(select(.id as $id | $existing_ids | index($id) | not))) as $new
                    | .models.providers["hiclaw-gateway"].models = ($existing + $new)
                    | (.models.providers["hiclaw-gateway"].models | map({ ("hiclaw-gateway/" + .id): { "alias": .id } }) | add // {}) as $aliases
                    | .agents.defaults.models = ((.agents.defaults.models // {}) + $aliases)
                    | .channels.matrix.encryption = $e2ee
                ' "${_tmp_in}" > "${_tmp_out}" 2>/dev/null
                if ! diff -q "${_tmp_in}" "${_tmp_out}" > /dev/null 2>&1; then
                    if mc cp "${_tmp_out}" "${_minio_path}" 2>/dev/null; then
                        _new_count=$(jq '.models.providers["hiclaw-gateway"].models | length' "${_tmp_out}" 2>/dev/null)
                        log "Worker ${_wname}: upgraded openclaw.json (models: ${_new_count}, e2ee: ${MATRIX_E2EE_ENABLED})"
                    fi
                fi
                rm -f "${_tmp_in}" "${_tmp_out}"
            fi
        done
    fi
fi

# ============================================================
# Ensure Worker Matrix password files exist in MinIO (E2EE fix)
# Workers need to re-login on restart to get a fresh device_id.
# Older workers created before this fix won't have the password file.
# ============================================================
if [ -f "${REGISTRY_FILE}" ]; then
    for _wname in $(jq -r '.workers | keys[]' "${REGISTRY_FILE}" 2>/dev/null); do
        [ -z "${_wname}" ] && continue
        _creds_file="/data/worker-creds/${_wname}.env"
        if [ -f "${_creds_file}" ]; then
            # Check if password file already exists in MinIO
            if ! mc stat "${HICLAW_STORAGE_PREFIX}/agents/${_wname}/credentials/matrix/password" > /dev/null 2>&1; then
                source "${_creds_file}"
                if [ -n "${WORKER_PASSWORD}" ]; then
                    _tmp_pw="/tmp/matrix-pw-${_wname}"
                    echo -n "${WORKER_PASSWORD}" > "${_tmp_pw}"
                    mc cp "${_tmp_pw}" "${HICLAW_STORAGE_PREFIX}/agents/${_wname}/credentials/matrix/password" 2>/dev/null \
                        && log "Worker ${_wname}: wrote Matrix password to MinIO (E2EE re-login fix)" \
                        || log "Worker ${_wname}: WARNING: failed to write Matrix password to MinIO"
                    rm -f "${_tmp_pw}"
                fi
            fi
        fi
    done
fi

# ============================================================
# Recreate Worker containers as needed after Manager restart.
# Workers are on hiclaw-net; Docker DNS resolves *-local.hiclaw.io via
# the Manager's network aliases, so IP changes don't require worker recreation.
# Only recreate stopped/missing workers.
# ============================================================
if container_api_available; then
    REGISTRY_FILE="/root/manager-workspace/workers-registry.json"
    if [ -f "${REGISTRY_FILE}" ]; then
        for _worker_name in $(jq -r '.workers | keys[]' "${REGISTRY_FILE}" 2>/dev/null); do
            [ -z "${_worker_name}" ] && continue

            # Skip remote workers — they are not Manager-managed containers.
            _deployment=$(jq -r --arg w "${_worker_name}" '.workers[$w].deployment // "local"' "${REGISTRY_FILE}" 2>/dev/null)
            if [ "${_deployment}" = "remote" ]; then
                log "Worker ${_worker_name} is remote, skipping container recreate"
                continue
            fi

            _status=$(container_status_worker "${_worker_name}")
            if [ "${_status}" = "running" ]; then
                log "Worker running: ${_worker_name}, skipping"
                continue
            fi
            # Container missing or stopped — recreate.
            log "Worker container ${_status}: ${_worker_name}, recreating..."
            _creds_file="/data/worker-creds/${_worker_name}.env"
            if [ -f "${_creds_file}" ]; then
                source "${_creds_file}"
                _runtime=$(jq -r --arg w "${_worker_name}" '.workers[$w].runtime // "openclaw"' "${REGISTRY_FILE}" 2>/dev/null)
                _recreated=false
                for _attempt in 1 2 3; do
                    if [ "${_runtime}" = "copaw" ]; then
                        container_create_copaw_worker "${_worker_name}" "${_worker_name}" "${WORKER_MINIO_PASSWORD}" 2>&1 && _recreated=true && break
                    else
                        container_create_worker "${_worker_name}" "${_worker_name}" "${WORKER_MINIO_PASSWORD}" 2>&1 && _recreated=true && break
                    fi
                    log "  Attempt ${_attempt}/3 failed for ${_worker_name}, retrying in $((5 * _attempt))s..."
                    sleep $((5 * _attempt))
                done
                if [ "${_recreated}" = true ]; then
                    log "  Recreated ${_runtime} worker: ${_worker_name}"
                else
                    log "  ERROR: Failed to recreate ${_worker_name} after 3 attempts"
                fi
            else
                log "  WARNING: No credentials found for ${_worker_name} (${_creds_file} missing), skipping"
            fi
        done
    fi
fi

# ============================================================
# Notify workers of builtin updates if upgrade happened
# Builtin files (AGENTS.md, skills) are already synced by upgrade-builtins.sh
#
# Cooldown: skip notification if the last successful notify was within
# NOTIFY_COOLDOWN_SECS (default 3600s / 1 hour). This prevents repeated
# notifications when the Manager crash-loops and re-runs upgrade-builtins
# on every restart (e.g. IMAGE_VERSION=latest always triggers upgrade).
# ============================================================
NOTIFY_COOLDOWN_SECS="${HICLAW_NOTIFY_COOLDOWN_SECS:-3600}"
NOTIFY_TS_FILE="/root/manager-workspace/.last-worker-notify-ts"

if [ -f /root/manager-workspace/.upgrade-pending-worker-notify ]; then
    _now=$(date +%s)
    _last_notify=$(cat "${NOTIFY_TS_FILE}" 2>/dev/null || echo "0")
    _elapsed=$(( _now - _last_notify ))

    if [ "${_elapsed}" -lt "${NOTIFY_COOLDOWN_SECS}" ]; then
        log "Skipping worker builtin notification (last notify ${_elapsed}s ago, cooldown ${NOTIFY_COOLDOWN_SECS}s)"
        rm -f /root/manager-workspace/.upgrade-pending-worker-notify
    else
        log "Notifying workers about builtin updates..."
        REGISTRY_FILE="/root/manager-workspace/workers-registry.json"
        _notify_ok=false
        if [ -f "${REGISTRY_FILE}" ]; then
            for _worker_name in $(jq -r '.workers | keys[]' "${REGISTRY_FILE}" 2>/dev/null); do
                [ -z "${_worker_name}" ] && continue
                _room_id=$(jq -r --arg w "${_worker_name}" '.workers[$w].room_id // empty' "${REGISTRY_FILE}" 2>/dev/null)
                if [ -n "${_room_id}" ]; then
                    _worker_id="@${_worker_name}:${MATRIX_DOMAIN}"
                    _txn_id="upgrade-$(date +%s%N)"
                    _msg="@${_worker_name}:${MATRIX_DOMAIN} Manager upgraded builtin files (AGENTS.md, skills). Please use your file-sync skill to sync the latest config."
                    _raw=$(curl -s -w '\nHTTP_CODE:%{http_code}' -X PUT \
                        "${HICLAW_MATRIX_SERVER}/_matrix/client/v3/rooms/${_room_id}/send/m.room.message/${_txn_id}" \
                        -H "Authorization: Bearer ${MANAGER_TOKEN}" \
                        -H 'Content-Type: application/json' \
                        -d "{\"msgtype\":\"m.text\",\"body\":\"${_msg}\",\"m.mentions\":{\"user_ids\":[\"${_worker_id}\"]}}" \
                        2>&1) || true
                    _http_code=$(echo "${_raw}" | tail -1 | sed 's/HTTP_CODE://')
                    _notify_resp=$(echo "${_raw}" | sed '$d')
                    if echo "${_notify_resp}" | jq -e '.event_id' > /dev/null 2>&1; then
                        log "  Notified ${_worker_name}"; _notify_ok=true
                    else
                        log "  WARNING: Failed to notify ${_worker_name} (HTTP ${_http_code}): ${_notify_resp}"
                    fi
                fi
            done
        fi
        # Record timestamp only if at least one notification succeeded
        if [ "${_notify_ok}" = true ]; then
            echo "${_now}" > "${NOTIFY_TS_FILE}"
        fi
        rm -f /root/manager-workspace/.upgrade-pending-worker-notify
    fi
fi

# ============================================================
# Start OpenClaw Manager Agent
# ============================================================
log "Starting Manager Agent (OpenClaw)..."

# HOME is already set to /root/manager-workspace via docker run -e HOME=...
export OPENCLAW_CONFIG_PATH="/root/manager-workspace/openclaw.json"

# Symlink to default OpenClaw config path so CLI commands find the config
mkdir -p "${HOME}/.openclaw"
ln -sf "/root/manager-workspace/openclaw.json" "${HOME}/.openclaw/openclaw.json"

# Ensure host credential symlinks exist under HOME
if [ -d "/host-share" ]; then
    [ -f "/host-share/.gitconfig" ] && ln -sf "/host-share/.gitconfig" "${HOME}/.gitconfig"
fi

log "HOME=${HOME} (manager-workspace, host-mounted)"
cd "${HOME}"

# Clean orphaned session write locks (e.g. from SIGKILL or crash before exit handlers)
# Prevents "session file locked (timeout 10000ms)" when PID was reused
find "${HOME}/.openclaw/agents" -name "*.jsonl.lock" -delete 2>/dev/null || true
log "Cleaned up any orphaned session write locks"

# Clean Matrix crypto storage (SQLite WAL may be corrupted after unclean shutdown)
# Crypto state is re-negotiated on startup; losing it only means re-establishing E2EE sessions
rm -rf "${HOME}/.openclaw/matrix" 2>/dev/null || true
log "Cleaned Matrix crypto storage (will re-establish E2EE sessions)"

# ── Render agent doc templates ────────────────────────────────────────────
# Replace ${VAR} placeholders with actual values so the AI agent reads
# plain text and never needs to resolve environment variables.
export MANAGER_MATRIX_TOKEN MANAGER_TOKEN HIGRESS_COOKIE_FILE
RENDER=/opt/hiclaw/scripts/lib/render-skills.sh
log "Rendering agent doc templates..."
# Manager-owned docs (workspace)
bash "$RENDER" /root/manager-workspace/skills
bash "$RENDER" /root/manager-workspace/skills-alpha
bash "$RENDER" /root/manager-workspace AGENTS.md TOOLS.md HEARTBEAT.md SOUL.md
# Worker templates (workspace + image) — rendered before push to MinIO
# so Workers (including remote pip-install) receive plain text
bash "$RENDER" /root/manager-workspace/worker-skills
bash "$RENDER" /root/manager-workspace/worker-agent
bash "$RENDER" /root/manager-workspace/copaw-worker-agent
bash "$RENDER" /opt/hiclaw/agent/worker-skills
bash "$RENDER" /opt/hiclaw/agent/worker-agent
bash "$RENDER" /opt/hiclaw/agent/copaw-worker-agent
log "Agent doc templates rendered"

# Cloud mode: start background file sync (workspace ↔ OSS) and initial push
if [ "${HICLAW_RUNTIME}" = "aliyun" ]; then
    log "Syncing initial workspace to OSS..."
    ensure_mc_credentials
    mc mirror /root/manager-workspace/ "${HICLAW_STORAGE_PREFIX}/manager/" --overwrite \
        --exclude ".openclaw/**" --exclude ".cache/**" 2>/dev/null || true

    # Local → OSS: change-triggered sync
    (
        while true; do
            CHANGED=$(find /root/manager-workspace/ -type f -newermt "15 seconds ago" 2>/dev/null | head -1)
            if [ -n "${CHANGED}" ]; then
                ensure_mc_credentials 2>/dev/null || true
                mc mirror /root/manager-workspace/ "${HICLAW_STORAGE_PREFIX}/manager/" --overwrite \
                    --exclude ".openclaw/**" --exclude ".cache/**" --exclude ".npm/**" \
                    --exclude ".local/**" --exclude ".mc/**" 2>/dev/null || true
            fi
            sleep 10
        done
    ) &
    log "Local→OSS sync started (PID: $!)"

    # OSS → Local: periodic pull (shared data, agent configs)
    (
        while true; do
            sleep 300
            ensure_mc_credentials 2>/dev/null || true
            mc mirror "${HICLAW_STORAGE_PREFIX}/shared/" /root/hiclaw-fs/shared/ --overwrite --newer-than "5m" 2>/dev/null || true
            mc mirror "${HICLAW_STORAGE_PREFIX}/agents/" /root/hiclaw-fs/agents/ --overwrite --newer-than "5m" 2>/dev/null || true
        done
    ) &
    log "OSS→Local sync started (every 5m, PID: $!)"
fi

# Launch OpenClaw
exec openclaw gateway run --verbose --force
