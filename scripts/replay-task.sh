#!/bin/bash
# replay-task.sh - Send a task message to the Manager Agent via Matrix
#
# Acts as the "human admin" by sending a Matrix message to the Manager
# and optionally waiting for its reply.
#
# Usage:
#   ./scripts/replay-task.sh "Create a Worker named alice"   # CLI mode
#   ./scripts/replay-task.sh                                  # Interactive mode
#   echo "Create worker bob" | ./scripts/replay-task.sh       # Pipe mode
#
# Environment variables (or loaded from ./agentteams-manager.env):
#   AGENTTEAMS_ADMIN_USER          Admin username       (default: admin)
#   AGENTTEAMS_ADMIN_PASSWORD      Admin password       (required)
#   AGENTTEAMS_MATRIX_DOMAIN       Matrix domain        (default: matrix-local.agentteams.io:8080)
#   REPLAY_WAIT                Wait for reply        (default: 1, set 0 to skip)
#   REPLAY_TIMEOUT             Reply timeout secs    (default: 300)
#   REPLAY_READY_TIMEOUT       Manager readiness timeout (default: 300)
#   REPLAY_MANAGER_CONTAINER   Manager container name    (default: agentteams-manager)

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# ============================================================
# Load configuration
# ============================================================

# Load env file if present (variables already set in env take precedence)
# Search order: AGENTTEAMS_ENV_FILE > project root > HOME directory
ENV_FILE="${AGENTTEAMS_ENV_FILE:-${PROJECT_ROOT}/agentteams-manager.env}"
if [ ! -f "${ENV_FILE}" ] && [ -f "${HOME}/agentteams-manager.env" ]; then
    ENV_FILE="${HOME}/agentteams-manager.env"
fi
if [ -f "${ENV_FILE}" ]; then
    # Source the env file but don't override existing env vars
    while IFS='=' read -r key value; do
        # Skip comments and empty lines
        [[ "${key}" =~ ^#.*$ || -z "${key}" ]] && continue
        # Trim whitespace
        key=$(echo "${key}" | xargs)
        # Only set if not already in environment
        if [ -z "${!key}" ]; then
            export "${key}=${value}"
        fi
    done < "${ENV_FILE}"
fi

# Configuration with defaults
ADMIN_USER="${AGENTTEAMS_ADMIN_USER:-admin}"
ADMIN_PASSWORD="${AGENTTEAMS_ADMIN_PASSWORD:-}"
MATRIX_DOMAIN="${AGENTTEAMS_MATRIX_DOMAIN:-matrix-local.agentteams.io:${AGENTTEAMS_PORT_GATEWAY:-8080}}"
WAIT_FOR_REPLY="${REPLAY_WAIT:-1}"
REPLY_TIMEOUT="${REPLAY_TIMEOUT:-300}"
MANAGER_USER="manager"
MANAGER_CONTAINER="${REPLAY_MANAGER_CONTAINER:-agentteams-manager}"

# When REPLAY_USE_DOCKER_EXEC=1, all Matrix API calls go through docker exec
# inside the container using the internal Tuwunel port. This avoids host proxy
# interference and works even when the gateway port isn't directly accessible.
USE_DOCKER_EXEC="${REPLAY_USE_DOCKER_EXEC:-0}"
if [ "${USE_DOCKER_EXEC}" = "1" ]; then
    MATRIX_URL="http://127.0.0.1:6167"
    CURL_PREFIX="docker exec ${MANAGER_CONTAINER}"
else
    MATRIX_URL="http://${MATRIX_DOMAIN}"
    CURL_PREFIX=""
fi

# ============================================================
# Utility functions
# ============================================================

log() {
    echo -e "\033[36m[replay]\033[0m $1"
}

error() {
    echo -e "\033[31m[replay ERROR]\033[0m $1" >&2
    exit 1
}

# ============================================================
# Matrix API helpers (self-contained, no dependency on test libs)
# ============================================================

matrix_api() {
    local method="$1"
    local path="$2"
    local data="$3"
    local token="$4"

    local auth_args=()
    if [ -n "${token}" ]; then
        auth_args+=(-H "Authorization: Bearer ${token}")
    fi

    if [ -n "${data}" ]; then
        ${CURL_PREFIX} curl -sf -X "${method}" \
            -H "Content-Type: application/json" \
            "${auth_args[@]}" \
            -d "${data}" \
            "${MATRIX_URL}${path}"
    else
        ${CURL_PREFIX} curl -sf -X "${method}" \
            "${auth_args[@]}" \
            "${MATRIX_URL}${path}"
    fi
}

# Login to Matrix, return access_token
do_login() {
    local resp
    resp=$(matrix_api POST "/_matrix/client/v3/login" \
        "{\"type\":\"m.login.password\",\"identifier\":{\"type\":\"m.id.user\",\"user\":\"${ADMIN_USER}\"},\"password\":\"${ADMIN_PASSWORD}\"}")
    echo "${resp}" | jq -r '.access_token // empty'
}

# Get joined rooms
get_joined_rooms() {
    local token="$1"
    matrix_api GET "/_matrix/client/v3/joined_rooms" "" "${token}" | jq -r '.joined_rooms[]'
}

# Get room members
get_room_members() {
    local token="$1"
    local room_id="$2"
    # URL-encode the room_id (! -> %21, : -> %3A)
    local encoded_room_id
    encoded_room_id=$(printf '%s' "${room_id}" | sed 's/!/%21/g; s/:/%3A/g')
    matrix_api GET "/_matrix/client/v3/rooms/${encoded_room_id}/members" "" "${token}" | jq -r '.chunk[].state_key' 2>/dev/null
}

# Find DM room with manager
find_manager_room() {
    local token="$1"
    local rooms
    rooms=$(get_joined_rooms "${token}")

    for room_id in ${rooms}; do
        local members
        members=$(get_room_members "${token}" "${room_id}" 2>/dev/null) || continue
        local member_count
        member_count=$(echo "${members}" | wc -l | xargs)

        # DM room: exactly 2 members, one is admin, one is manager
        if [ "${member_count}" = "2" ] && echo "${members}" | grep -q "@${MANAGER_USER}:"; then
            echo "${room_id}"
            return 0
        fi
    done

    return 1
}

# Create a DM room with the manager and return room_id
create_dm_room() {
    local token="$1"

    local manager_full_id="@${MANAGER_USER}:${MATRIX_DOMAIN}"
    local resp
    resp=$(matrix_api POST "/_matrix/client/v3/createRoom" \
        "{\"is_direct\":true,\"invite\":[\"${manager_full_id}\"],\"preset\":\"trusted_private_chat\"}" "${token}")

    echo "${resp}" | jq -r '.room_id // empty' 2>/dev/null
}

# Send a message to a room
send_message() {
    local token="$1"
    local room_id="$2"
    local body="$3"
    local txn_id
    txn_id="replay_$(date +%s%N)"
    local encoded_room_id
    encoded_room_id=$(printf '%s' "${room_id}" | sed 's/!/%21/g; s/:/%3A/g')

    # Escape the body for JSON
    local json_body
    json_body=$(printf '%s' "${body}" | jq -Rs .)

    matrix_api PUT "/_matrix/client/v3/rooms/${encoded_room_id}/send/m.room.message/${txn_id}" \
        "{\"msgtype\":\"m.text\",\"body\":${json_body}}" "${token}" > /dev/null
}

# Read recent messages from a room
read_messages() {
    local token="$1"
    local room_id="$2"
    local limit="${3:-10}"
    local encoded_room_id
    encoded_room_id=$(printf '%s' "${room_id}" | sed 's/!/%21/g; s/:/%3A/g')
    matrix_api GET "/_matrix/client/v3/rooms/${encoded_room_id}/messages?dir=b&limit=${limit}" "" "${token}"
}

# Wait for a reply from the manager
# Outputs ONLY the reply body to stdout (for capture).
# Progress messages go to stderr (visible in terminal but not captured).
wait_for_manager_reply() {
    local token="$1"
    local room_id="$2"
    local after_event="$3"
    local timeout="${4:-${REPLY_TIMEOUT}}"
    local elapsed=0

    local baseline_event="${after_event}"

    echo -e "\033[36m[replay]\033[0m Waiting for Manager reply (timeout: ${timeout}s)..." >&2

    while [ "${elapsed}" -lt "${timeout}" ]; do
        sleep 5
        elapsed=$((elapsed + 5))

        local messages
        messages=$(read_messages "${token}" "${room_id}" 10 2>/dev/null) || continue

        # Get latest manager message
        local latest_event latest_body
        latest_event=$(echo "${messages}" | jq -r --arg user "@${MANAGER_USER}:" \
            '[.chunk[] | select(.sender | contains($user)) | .event_id] | first // ""' 2>/dev/null)
        latest_body=$(echo "${messages}" | jq -r --arg user "@${MANAGER_USER}:" \
            '[.chunk[] | select(.sender | contains($user)) | .content.body] | first // empty' 2>/dev/null)

        # Only return if the event_id differs from baseline (new message)
        if [ -n "${latest_body}" ] && [ "${latest_event}" != "${baseline_event}" ]; then
            echo "" >&2
            echo -e "\033[32m[Manager]\033[0m" >&2
            echo "${latest_body}" >&2
            # Output ONLY the clean reply to stdout
            echo "${latest_body}"
            return 0
        fi

        printf "\r\033[36m[replay]\033[0m Waiting... (%ds/%ds)" "${elapsed}" "${timeout}" >&2
    done

    echo "" >&2
    echo -e "\033[36m[replay]\033[0m Timeout: no reply from Manager within ${timeout}s" >&2
    return 1
}

# ============================================================
# Main
# ============================================================

# Validate configuration
if [ -z "${ADMIN_PASSWORD}" ]; then
    error "AGENTTEAMS_ADMIN_PASSWORD is required. Set it via env var or ensure ./agentteams-manager.env exists."
fi

# Get task message from CLI arg, stdin, or interactive prompt
TASK_MSG=""

if [ $# -gt 0 ]; then
    # CLI mode
    TASK_MSG="$*"
elif [ ! -t 0 ]; then
    # Pipe mode (stdin is not a terminal)
    TASK_MSG=$(cat)
else
    # Interactive mode
    echo -e "\033[36m[replay]\033[0m Enter the task message to send to Manager:"
    echo -e "\033[36m[replay]\033[0m (Press Enter to send, Ctrl+C to cancel)"
    echo -n "> "
    read -r TASK_MSG
fi

if [ -z "${TASK_MSG}" ]; then
    error "Task message cannot be empty"
fi

log "Task: ${TASK_MSG}"
log ""

# ============================================================
# Conversation log setup
# ============================================================
LOG_DIR="${REPLAY_LOG_DIR:-${PROJECT_ROOT}/logs/replay}"
mkdir -p "${LOG_DIR}"
LOG_TS=$(date '+%Y%m%d-%H%M%S')
LOG_FILE="${LOG_DIR}/replay-${LOG_TS}.log"

write_log() {
    echo "$1" >> "${LOG_FILE}"
}

write_log "# AgentTeams Replay Log"
write_log "# Time: $(date '+%Y-%m-%d %H:%M:%S')"
write_log "# Task: ${TASK_MSG}"
write_log ""

# Step 1: Login
log "Logging in as '${ADMIN_USER}'..."
ACCESS_TOKEN=$(do_login)
if [ -z "${ACCESS_TOKEN}" ]; then
    error "Login failed. Check AGENTTEAMS_ADMIN_USER and AGENTTEAMS_ADMIN_PASSWORD."
fi
log "Login successful"

# Step 2: Find or create DM room with Manager
log "Finding DM room with Manager..."
ROOM_ID=$(find_manager_room "${ACCESS_TOKEN}" 2>/dev/null || true)
if [ -z "${ROOM_ID}" ]; then
    log "No existing DM room found, creating one..."
    ROOM_ID=$(create_dm_room "${ACCESS_TOKEN}")
    if [ -z "${ROOM_ID}" ]; then
        error "Failed to create DM room with @${MANAGER_USER}. Is the Manager Agent running?"
    fi
    log "Created DM room: ${ROOM_ID}"
else
    log "Found existing room: ${ROOM_ID}"
fi

# Step 3: Wait for Manager agent to be ready
# Use `openclaw gateway health` inside the container to confirm the gateway is running
# and processing Matrix events, then verify Manager has joined the DM room.
READY_TIMEOUT="${REPLAY_READY_TIMEOUT:-300}"
READY_ELAPSED=0
MANAGER_FULL_ID="@${MANAGER_USER}:${MATRIX_DOMAIN}"

log "Waiting for Manager agent to be ready..."

# Phase 1: Wait for OpenClaw gateway to be healthy inside the container
GATEWAY_READY=false
while [ "${READY_ELAPSED}" -lt "${READY_TIMEOUT}" ]; do
    if docker exec "${MANAGER_CONTAINER}" openclaw gateway health --json 2>/dev/null | grep -q '"ok"' 2>/dev/null; then
        GATEWAY_READY=true
        log "Manager OpenClaw gateway is healthy"
        break
    fi
    sleep 5
    READY_ELAPSED=$((READY_ELAPSED + 5))
    printf "\r\033[36m[replay]\033[0m Waiting for OpenClaw gateway... (%ds/%ds)" "${READY_ELAPSED}" "${READY_TIMEOUT}"
done

if [ "${GATEWAY_READY}" != "true" ]; then
    error "Manager OpenClaw gateway did not become healthy within ${READY_TIMEOUT}s. Check: docker logs ${MANAGER_CONTAINER}"
fi

# Phase 2: Wait for Manager to join the DM room (confirms Matrix channel is active)
# Reset elapsed counter for phase 2 (phase 1 may have consumed the budget)
READY_ELAPSED=0
while [ "${READY_ELAPSED}" -lt "${READY_TIMEOUT}" ]; do
    MEMBERS=$(get_room_members "${ACCESS_TOKEN}" "${ROOM_ID}" 2>/dev/null) || true
    if echo "${MEMBERS}" | grep -q "${MANAGER_FULL_ID}"; then
        log "Manager has joined the room"
        break
    fi
    sleep 3
    READY_ELAPSED=$((READY_ELAPSED + 3))
    printf "\r\033[36m[replay]\033[0m Waiting for Manager to join room... (%ds/%ds)" "${READY_ELAPSED}" "${READY_TIMEOUT}"
done

if ! echo "${MEMBERS}" | grep -q "${MANAGER_FULL_ID}" 2>/dev/null; then
    error "Manager did not join the room within ${READY_TIMEOUT}s. Gateway is healthy but Matrix channel may not be configured."
fi

# Step 4: Send message
BASELINE_MANAGER_EVENT=""
if [ "${WAIT_FOR_REPLY}" = "1" ]; then
    # Capture the boundary before sending so an immediate reply cannot become the baseline.
    if ! BASELINE_MANAGER_EVENT=$(read_messages "${ACCESS_TOKEN}" "${ROOM_ID}" 5 2>/dev/null | \
        jq -r --arg user "@${MANAGER_USER}:" \
        '[.chunk[] | select(.sender | contains($user)) | .event_id] | first // ""' 2>/dev/null); then
        error "Failed to read the Manager message boundary before sending the task."
    fi
fi

log "Sending task message..."
send_message "${ACCESS_TOKEN}" "${ROOM_ID}" "${TASK_MSG}"
log "Message sent"

# Step 5: Wait for reply
if [ "${WAIT_FOR_REPLY}" = "1" ]; then
    REPLY=$(wait_for_manager_reply "${ACCESS_TOKEN}" "${ROOM_ID}" "${BASELINE_MANAGER_EVENT}" "${REPLY_TIMEOUT}")

    # ============================================================
    # Collect room messages into the log file
    # ============================================================
    log ""
    log "--- Collecting room messages ---"

    # Helper: dump messages of a room into the log
    dump_room_messages() {
        local token="$1"
        local rid="$2"
        local limit="${3:-100}"
        read_messages "${token}" "${rid}" "${limit}" 2>/dev/null | jq -r '
            .chunk | reverse | .[] |
            select(.content.body != null and .content.body != "") |
            "**[\(.origin_server_ts / 1000 | strftime("%H:%M:%S"))] \(.sender | split(":")[0] | ltrimstr("@"))**\n\n\(.content.body)\n"
        ' 2>/dev/null
    }

    # DM Room
    write_log "## DM (admin <-> manager)"
    write_log ""
    dump_room_messages "${ACCESS_TOKEN}" "${ROOM_ID}" 100 >> "${LOG_FILE}"

    # Worker / other rooms that include @manager
    ALL_ROOMS=$(get_joined_rooms "${ACCESS_TOKEN}" 2>/dev/null)
    for rid in ${ALL_ROOMS}; do
        [ "${rid}" = "${ROOM_ID}" ] && continue
        ROOM_MEMBERS=$(get_room_members "${ACCESS_TOKEN}" "${rid}" 2>/dev/null) || continue
        echo "${ROOM_MEMBERS}" | grep -q "@${MANAGER_USER}:" || continue

        MEMBER_NAMES=$(echo "${ROOM_MEMBERS}" | sed 's/@//g; s/:.*//g' | tr '\n' ', ' | sed 's/,$//')
        write_log "---"
        write_log ""
        write_log "## Room: ${MEMBER_NAMES}"
        write_log ""
        dump_room_messages "${ACCESS_TOKEN}" "${rid}" 100 >> "${LOG_FILE}"
    done

    log "Conversation log saved to: ${LOG_FILE}"
else
    log "Skipping reply wait (REPLAY_WAIT=0)"
fi
