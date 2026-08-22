#!/bin/bash
# matrix-client.sh - Matrix API wrapper for integration tests
#
# All requests are sent via exec_in_manager() (docker exec into the Manager container)
# so that Matrix (port 6167) does not need to be exposed to the host.

# Source test-helpers for environment vars
_MATRIX_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${_MATRIX_LIB_DIR}/test-helpers.sh" 2>/dev/null || true

# ============================================================
# User Management
# ============================================================

# Register a Matrix user
# Usage: matrix_register <username> <password>
# Returns: JSON response with access_token
matrix_register() {
    local username="$1"
    local password="$2"
    local token="${TEST_REGISTRATION_TOKEN}"

    exec_in_manager curl -sf -X POST "${TEST_MATRIX_DIRECT_URL}/_matrix/client/v3/register" \
        -H 'Content-Type: application/json' \
        -d '{
            "username": "'"${username}"'",
            "password": "'"${password}"'",
            "auth": {
                "type": "m.login.registration_token",
                "token": "'"${token}"'"
            }
        }'
}

# Login to Matrix
# Usage: matrix_login <username> <password>
# Returns: JSON with access_token
matrix_login() {
    local username="$1"
    local password="$2"

    exec_in_manager curl -sf -X POST "${TEST_MATRIX_DIRECT_URL}/_matrix/client/v3/login" \
        -H 'Content-Type: application/json' \
        -d '{
            "type": "m.login.password",
            "identifier": {"type": "m.id.user", "user": "'"${username}"'"},
            "password": "'"${password}"'"
        }'
}

# ============================================================
# Room Management
# ============================================================

# Get list of joined rooms
# Usage: matrix_joined_rooms <access_token>
matrix_joined_rooms() {
    local token="$1"
    exec_in_manager curl -sf "${TEST_MATRIX_DIRECT_URL}/_matrix/client/v3/joined_rooms" \
        -H "Authorization: Bearer ${token}"
}

# URL-encode a room ID for use in URL paths (! -> %21)
_encode_room_id() {
    echo "${1//!/%21}"
}

# ============================================================
# Messaging
# ============================================================

# Send a message to a room
# Usage: matrix_send_message <access_token> <room_id> <message_body>
# Returns: JSON with event_id
matrix_send_message() {
    local token="$1"
    local room_id="$2"
    local body="$3"
    local txn_id="$(date +%s%N)"
    local room_enc
    room_enc="$(_encode_room_id "${room_id}")"

    # Escape newlines and special chars for JSON
    local escaped_body
    escaped_body=$(echo "$body" | jq -Rs '.' | sed 's/^"//;s/"$//')

    exec_in_manager curl -sf -X PUT "${TEST_MATRIX_DIRECT_URL}/_matrix/client/v3/rooms/${room_enc}/send/m.room.message/${txn_id}" \
        -H "Authorization: Bearer ${token}" \
        -H 'Content-Type: application/json' \
        -d '{
            "msgtype": "m.text",
            "body": "'"${escaped_body}"'"
        }'
}

# Read recent messages from a room
# Usage: matrix_read_messages <access_token> <room_id> [limit]
# Returns: JSON with messages
matrix_read_messages() {
    local token="$1"
    local room_id="$2"
    local limit="${3:-20}"
    local room_enc
    room_enc="$(_encode_room_id "${room_id}")"

    exec_in_manager curl -sf "${TEST_MATRIX_DIRECT_URL}/_matrix/client/v3/rooms/${room_enc}/messages?dir=b&limit=${limit}" \
        -H "Authorization: Bearer ${token}"
}

# Wait for a reply from a specific user in a room
# Usage: matrix_wait_for_reply <access_token> <room_id> <from_user_prefix> [timeout_seconds]
# Returns: the reply message body, or empty string on timeout
#
# This function snapshots the latest known event_id before polling, then only
# returns messages that appear AFTER that snapshot. This prevents returning
# stale messages from previous conversations (important in --use-existing mode).
matrix_wait_for_reply() {
    local token="$1"
    local room_id="$2"
    local from_user="$3"
    local timeout="${4:-180}"
    local nudge_token="${5:-}"
    local nudge_room="${6:-}"
    local nudge_message="${7:-}"
    local nudge_interval="${8:-600}"
    local elapsed=0

    # Snapshot the latest event_id from the target user before we start waiting
    local baseline_event
    baseline_event=$(matrix_read_messages "${token}" "${room_id}" 5 2>/dev/null | \
        jq -r --arg user "${from_user}" \
        '[.chunk[] | select(.sender | startswith($user)) | .event_id] | first // ""' 2>/dev/null)

    while [ "${elapsed}" -lt "${timeout}" ]; do
        sleep 10
        elapsed=$((elapsed + 10))

        # Send nudge if configured and interval reached
        if [ -n "${nudge_token}" ] && [ -n "${nudge_room}" ] && [ -n "${nudge_message}" ] \
                && [ $((elapsed % nudge_interval)) -eq 0 ]; then
            log_info "Sending nudge to Manager (elapsed: ${elapsed}s)..."
            matrix_send_message "${nudge_token}" "${nudge_room}" "${nudge_message}" 2>/dev/null || true
        fi

        local messages
        messages=$(matrix_read_messages "${token}" "${room_id}" 10 2>/dev/null) || continue

        # Get the latest message from the target user
        local latest_event latest_body
        latest_event=$(echo "${messages}" | jq -r --arg user "${from_user}" \
            '[.chunk[] | select(.sender | startswith($user)) | .event_id] | first // ""' 2>/dev/null)
        latest_body=$(echo "${messages}" | jq -r --arg user "${from_user}" \
            '[.chunk[] | select(.sender | startswith($user)) | .content.body] | first // empty' 2>/dev/null)

        # Only return if the event_id differs from baseline (i.e., it's a NEW message)
        if [ -n "${latest_body}" ] && [ "${latest_event}" != "${baseline_event}" ]; then
            echo "${latest_body}"
            return 0
        fi
    done

    return 1
}

# Wait for a message containing a specific keyword from a user
# Usage: matrix_wait_for_message_containing <token> <room_id> <from_user_prefix> <keyword> [timeout_seconds]
# Returns: the matching message body, or empty string on timeout
# <keyword> is passed to grep -qi (supports regex like "done\|完成")
matrix_wait_for_message_containing() {
    local token="$1"
    local room_id="$2"
    local from_user="$3"
    local keyword="$4"
    local timeout="${5:-1800}"
    local nudge_token="${6:-}"
    local nudge_room="${7:-}"
    local nudge_message="${8:-}"
    local nudge_interval="${9:-600}"
    local elapsed=0

    # Snapshot the latest known event_id to avoid returning stale messages
    local baseline_event
    baseline_event=$(matrix_read_messages "${token}" "${room_id}" 5 2>/dev/null | \
        jq -r --arg user "${from_user}" \
        '[.chunk[] | select(.sender | startswith($user)) | .event_id] | first // ""' 2>/dev/null)

    while [ "${elapsed}" -lt "${timeout}" ]; do
        sleep 15
        elapsed=$((elapsed + 15))

        # Send nudge if configured and interval reached
        if [ -n "${nudge_token}" ] && [ -n "${nudge_room}" ] && [ -n "${nudge_message}" ] \
                && [ $((elapsed % nudge_interval)) -eq 0 ]; then
            log_info "Sending nudge to Manager (elapsed: ${elapsed}s)..."
            matrix_send_message "${nudge_token}" "${nudge_room}" "${nudge_message}" 2>/dev/null || true
        fi

        local messages all_bodies
        messages=$(matrix_read_messages "${token}" "${room_id}" 20 2>/dev/null) || continue

        # Check if there's any new message from the target user containing the keyword
        local latest_event
        latest_event=$(echo "${messages}" | jq -r --arg user "${from_user}" \
            '[.chunk[] | select(.sender | startswith($user)) | .event_id] | first // ""' 2>/dev/null)

        if [ "${latest_event}" != "${baseline_event}" ]; then
            # There are new messages; check if any match the keyword
            local matching_body
            matching_body=$(echo "${messages}" | jq -r --arg user "${from_user}" \
                '[.chunk[] | select(.sender | startswith($user)) | .content.body] | join("\n")' 2>/dev/null)
            if echo "${matching_body}" | grep -qi "${keyword}"; then
                echo "${matching_body}"
                return 0
            fi
        fi
    done

    return 1
}

# Create a DM room with another user
# Usage: matrix_create_dm_room <access_token> <other_user_id>
# Returns: room_id
matrix_create_dm_room() {
    local token="$1"
    local other_user="$2"

    local result
    result=$(exec_in_manager curl -sf -X POST "${TEST_MATRIX_DIRECT_URL}/_matrix/client/v3/createRoom" \
        -H "Authorization: Bearer ${token}" \
        -H 'Content-Type: application/json' \
        -d '{
            "preset": "trusted_private_chat",
            "invite": ["'"${other_user}"'"],
            "is_direct": true
        }' 2>/dev/null)

    echo "${result}" | jq -r '.room_id // empty'
}

# Find a room by name prefix
# Usage: matrix_find_room_by_name <access_token> <name_pattern>
# Returns: room_id of first matching room
matrix_find_room_by_name() {
    local token="$1"
    local name_pattern="$2"

    local rooms
    rooms=$(matrix_joined_rooms "${token}" | jq -r '.joined_rooms[]')

    for room_id in ${rooms}; do
        local room_enc name
        room_enc="$(_encode_room_id "${room_id}")"
        name=$(exec_in_manager curl -sf "${TEST_MATRIX_DIRECT_URL}/_matrix/client/v3/rooms/${room_enc}/state/m.room.name" \
            -H "Authorization: Bearer ${token}" 2>/dev/null | jq -r '.name // empty')
        if echo "${name}" | grep -qi "${name_pattern}"; then
            echo "${room_id}"
            return 0
        fi
    done

    return 1
}

# Find a DM room between two users
# Usage: matrix_find_dm_room <access_token> <other_user_id>
matrix_find_dm_room() {
    local token="$1"
    local other_user="$2"

    log_info "Looking for DM room with user: ${other_user}"

    local rooms
    rooms=$(matrix_joined_rooms "${token}" | jq -r '.joined_rooms[]')

    for room_id in ${rooms}; do
        local room_enc members member_count
        room_enc="$(_encode_room_id "${room_id}")"
        members=$(exec_in_manager curl -sf "${TEST_MATRIX_DIRECT_URL}/_matrix/client/v3/rooms/${room_enc}/members" \
            -H "Authorization: Bearer ${token}" 2>/dev/null | jq -r '.chunk[].state_key' 2>/dev/null)

        # DM rooms have exactly 2 members; skip group rooms (3+ members)
        member_count=$(echo "${members}" | grep -c '.' 2>/dev/null || echo 0)
        if [ "${member_count}" -eq 2 ] && echo "${members}" | grep -q "${other_user}"; then
            echo "${room_id}"
            return 0
        fi
    done

    return 1
}
