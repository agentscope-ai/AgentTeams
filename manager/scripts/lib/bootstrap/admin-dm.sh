#!/bin/bash
# bootstrap/admin-dm.sh - Admin DM room creation and welcome message

SEND_MANAGER_MESSAGE="/opt/hiclaw/scripts/lib/send-manager-message.sh"

bootstrap_setup_admin_dm() {
    if [ "${AGENTTEAMS_RUNTIME}" = "k8s" ]; then
        log "K8s mode: skipping admin DM room creation and welcome message (both handled by agentteams-controller)"
        return 0
    fi

    local MANAGER_FULL_ID="@manager:${MATRIX_DOMAIN}"
    local ADMIN_FULL_ID="@${AGENTTEAMS_ADMIN_USER}:${MATRIX_DOMAIN}"

    log "Logging in as admin to create DM room..."
    local _ADMIN_LOGIN ADMIN_MATRIX_TOKEN
    _ADMIN_LOGIN=$(curl -sf -X POST "${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/login" \
        -H 'Content-Type: application/json' \
        -d '{
            "type": "m.login.password",
            "identifier": {"type": "m.id.user", "user": "'"${AGENTTEAMS_ADMIN_USER}"'"},
            "password": "'"${AGENTTEAMS_ADMIN_PASSWORD}"'"
        }' 2>&1) || true

    ADMIN_MATRIX_TOKEN=$(echo "${_ADMIN_LOGIN}" | jq -r '.access_token // empty' 2>/dev/null)
    if [ -z "${ADMIN_MATRIX_TOKEN}" ]; then
        log "WARNING: Failed to login as admin, skipping DM room creation"
        return 0
    fi

    local DM_ROOM_ID="" _JOINED_ROOMS _rid _members _count _RAW _HTTP_CODE _CREATE_RESP
    _JOINED_ROOMS=$(curl -sf "${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/joined_rooms" \
        -H "Authorization: Bearer ${ADMIN_MATRIX_TOKEN}" 2>/dev/null \
        | jq -r '.joined_rooms[]' 2>/dev/null) || true
    for _rid in ${_JOINED_ROOMS}; do
        _members=$(curl -sf "${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/rooms/${_rid}/members" \
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
        _RAW=$(curl -s -w '\nHTTP_CODE:%{http_code}' -X POST "${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/createRoom" \
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

    if [ -n "${DM_ROOM_ID}" ]; then
        local STATE_SCRIPT="/opt/hiclaw/agent/skills/task-management/scripts/manage-state.sh"
        if [ -f "${STATE_SCRIPT}" ]; then
            bash "${STATE_SCRIPT}" --action init 2>/dev/null || true
            bash "${STATE_SCRIPT}" --action set-admin-dm --room-id "${DM_ROOM_ID}" 2>/dev/null || true
            log "Admin DM room persisted to state.json: ${DM_ROOM_ID}"
        fi
    fi

    if [ -n "${DM_ROOM_ID}" ] && [ ! -f "/root/manager-workspace/soul-configured" ]; then
        bootstrap_schedule_welcome_message "${DM_ROOM_ID}" "${ADMIN_MATRIX_TOKEN}"
    fi
}

bootstrap_schedule_welcome_message() {
    local DM_ROOM_ID="$1"
    local ADMIN_MATRIX_TOKEN="${2:-}"
    local runtime_label="OpenClaw"
    if [ "${MANAGER_RUNTIME}" = "copaw" ]; then
        runtime_label="CoPaw"
    fi
    log "Scheduling welcome message (background, waiting for ${runtime_label} to start)..."
    (
        local _AGENTTEAMS_LANGUAGE="${AGENTTEAMS_LANGUAGE:-zh}"
        local _AGENTTEAMS_TIMEZONE="${TZ:-Asia/Shanghai}"
        local _wait=0 _ready=false
        while [ "${_wait}" -lt 300 ]; do
            if curl -sf http://127.0.0.1:18799/ > /dev/null 2>&1; then
                _ready=true
                break
            fi
            sleep 3
            _wait=$((_wait + 3))
        done
        if [ "${_ready}" != "true" ]; then
            echo "[manager] WARNING: Manager runtime not ready within 300s, skipping welcome message"
            exit 0
        fi

        local _join_ok=false _join_attempt
        for _join_attempt in 1 2 3; do
            if curl -sf -X POST "${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/rooms/${DM_ROOM_ID}/join" \
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

        local _welcome_msg="This is an automated message from the AgentTeams setup. This is a fresh installation.

--- Installation Context ---
User Language: ${_AGENTTEAMS_LANGUAGE}  (zh = Chinese, en = English)
User Timezone: ${_AGENTTEAMS_TIMEZONE}  (IANA timezone identifier)
---

You are an AI agent that manages a team of worker agents. Your identity and personality have not been configured yet — the human admin is about to meet you for the first time.

Please begin the onboarding conversation:

1. Greet the admin warmly and briefly describe what you can do (coordinate workers, manage tasks, run multi-agent projects)
2. The user has selected \"${_AGENTTEAMS_LANGUAGE}\" as their preferred language during installation. Use this language for your greeting and all subsequent communication.
3. The user's timezone is ${_AGENTTEAMS_TIMEZONE}. Based on this timezone, you may infer their likely region and suggest additional language options.
4. Ask them: a) What would they like to call you? b) Communication style preference? c) Any behavior guidelines? d) Confirm default language
5. After they reply, write their preferences to ~/SOUL.md
6. Confirm what you wrote, and ask if they would like to adjust anything
7. Once confirmed, run: touch ~/soul-configured

The human admin will start chatting shortly."

        local _send_args=(
            bash "${SEND_MANAGER_MESSAGE}"
            --room "${DM_ROOM_ID}"
            --text "${_welcome_msg}"
            --wait-runtime
        )
        if [ "${MANAGER_RUNTIME}" = "openclaw" ]; then
            _send_args+=(--token "${ADMIN_MATRIX_TOKEN}")
        fi
        if "${_send_args[@]}" 2>/dev/null; then
            echo "[manager] Welcome message sent to DM room"
        else
            echo "[manager] WARNING: Failed to send welcome message via send-manager-message.sh"
        fi
    ) &
    log "Welcome message background process started (PID: $!)"
}
