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

# Send a message that visibly mentions another Matrix user, the way Element does.
# Usage: matrix_send_mention_message <access_token> <room_id> <mention_user_id> <message_body>
# Returns: JSON with event_id
#
# openclaw's mention detection (extensions/matrix/src/matrix/monitor/mentions.ts)
# requires BOTH `m.mentions.user_ids` metadata AND a *visible* mention — either
# a `matrix.to` link in `formatted_body` or a regex match on plain text derived
# from the agent's identity. A worker created from a minimal SOUL has no custom
# identity regex, so a body-only mention is silently dropped with
# `reason: "no-mention"`. Always use this helper for tests that need a worker
# to actually wake up and reply.
matrix_send_mention_message() {
    local token="$1"
    local room_id="$2"
    local mention_user="$3"
    local body="$4"
    local txn_id="$(date +%s%N)"
    local room_enc
    room_enc="$(_encode_room_id "${room_id}")"

    # URL-encode the user_id for the matrix.to link (@ -> %40, : -> %3A)
    local user_enc="${mention_user//@/%40}"
    user_enc="${user_enc//:/%3A}"

    local payload
    payload=$(jq -nc \
        --arg user "${mention_user}" \
        --arg user_enc "${user_enc}" \
        --arg msg "${body}" \
        '{
            msgtype: "m.text",
            body: ($user + " " + $msg),
            format: "org.matrix.custom.html",
            formatted_body: ("<a href=\"https://matrix.to/#/" + $user_enc + "\">" + $user + "</a> " + $msg),
            "m.mentions": { user_ids: [$user] }
        }')

    exec_in_manager curl -sf -X PUT "${TEST_MATRIX_DIRECT_URL}/_matrix/client/v3/rooms/${room_enc}/send/m.room.message/${txn_id}" \
        -H "Authorization: Bearer ${token}" \
        -H 'Content-Type: application/json' \
        -d "${payload}"
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

    # Snapshot the latest m.room.message event_id from the target user before we
    # start waiting. We filter on type=m.room.message with a non-null body so
    # that reactions, redactions, typing indicators, and similar zero-content
    # events from runtimes like hermes-agent don't get treated as "the reply".
    local baseline_event
    baseline_event=$(matrix_read_messages "${token}" "${room_id}" 20 2>/dev/null | \
        jq -r --arg user "${from_user}" \
        '[.chunk[] | select(.sender | startswith($user)) | select(.type == "m.room.message") | select(.content.body != null) | .event_id] | first // ""' 2>/dev/null)

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
        messages=$(matrix_read_messages "${token}" "${room_id}" 20 2>/dev/null) || continue

        # Find the newest m.room.message from the target user that has a body.
        # Some runtimes (hermes-agent) emit reactions/redactions around their
        # actual reply, so we must look past those to find the real message.
        local latest_event latest_body
        latest_event=$(echo "${messages}" | jq -r --arg user "${from_user}" \
            '[.chunk[] | select(.sender | startswith($user)) | select(.type == "m.room.message") | select(.content.body != null) | .event_id] | first // ""' 2>/dev/null)
        latest_body=$(echo "${messages}" | jq -r --arg user "${from_user}" \
            '[.chunk[] | select(.sender | startswith($user)) | select(.type == "m.room.message") | select(.content.body != null) | .content.body] | first // empty' 2>/dev/null)

        # Only return if the event_id differs from baseline (i.e., it's a NEW message)
        if [ -n "${latest_body}" ] && [ "${latest_event}" != "${baseline_event}" ]; then
            echo "${latest_body}"
            return 0
        fi
    done

    return 1
}

# Wait for a reply from a specific user that matches a (case-insensitive) regex.
#
# Usage: matrix_wait_for_reply_matching <token> <room_id> <from_user_prefix> <pattern> \
#        [timeout_seconds] [nudge_token nudge_room nudge_message nudge_interval]
#
# Returns: the body of the FIRST matching reply (printed to stdout), exit 0 on
# match, or exit 1 on timeout. Non-matching new replies during the wait are
# logged via log_info so the test artifact still captures the agent's
# progressive ack messages.
#
# WHY this exists in addition to matrix_wait_for_reply: some Manager runtimes
# (notably CoPaw) reply progressively — the very first DM ack may be a generic
# "let me set that up" without yet naming the Worker, with the Worker name
# appearing in a follow-up reply 5-30s later. Tests that need to assert on
# specific content (e.g. the Worker's name) should use this helper rather than
# locking onto the first ack.
matrix_wait_for_reply_matching() {
    local token="$1"
    local room_id="$2"
    local from_user="$3"
    local pattern="$4"
    local timeout="${5:-180}"
    local nudge_token="${6:-}"
    local nudge_room="${7:-}"
    local nudge_message="${8:-}"
    local nudge_interval="${9:-600}"
    local elapsed=0

    # Snapshot the baseline event_id so we only consider NEW messages.
    local baseline_event
    baseline_event=$(matrix_read_messages "${token}" "${room_id}" 20 2>/dev/null | \
        jq -r --arg user "${from_user}" \
        '[.chunk[] | select(.sender | startswith($user)) | select(.type == "m.room.message") | select(.content.body != null) | .event_id] | first // ""' 2>/dev/null)

    # Track which non-matching new replies we've already logged, to avoid
    # repeating them on every poll.
    local seen_log=""

    while [ "${elapsed}" -lt "${timeout}" ]; do
        sleep 10
        elapsed=$((elapsed + 10))

        if [ -n "${nudge_token}" ] && [ -n "${nudge_room}" ] && [ -n "${nudge_message}" ] \
                && [ $((elapsed % nudge_interval)) -eq 0 ]; then
            log_info "Sending nudge to Manager (elapsed: ${elapsed}s)..."
            matrix_send_message "${nudge_token}" "${nudge_room}" "${nudge_message}" 2>/dev/null || true
        fi

        local messages
        messages=$(matrix_read_messages "${token}" "${room_id}" 30 2>/dev/null) || continue

        # Collect all NEW replies from the target user (oldest -> newest order)
        # by walking messages until we hit the baseline event_id. matrix_read_messages
        # returns newest-first (dir=b), so we reverse the chunk for chronological order.
        local new_replies
        new_replies=$(echo "${messages}" | jq -r --arg user "${from_user}" --arg baseline "${baseline_event}" '
            [ .chunk[]
              | select(.sender | startswith($user))
              | select(.type == "m.room.message")
              | select(.content.body != null)
            ] as $msgs
            | ($msgs | map(.event_id) | index($baseline)) as $idx
            | (if $idx == null then $msgs else $msgs[0:$idx] end) | reverse
            | .[] | "\(.event_id)\t\(.content.body | gsub("\n";" "))"
        ' 2>/dev/null)

        if [ -z "${new_replies}" ]; then
            continue
        fi

        # Iterate chronologically, return on first match.
        local line event body
        while IFS=$'\t' read -r event body; do
            [ -z "${event}" ] && continue
            if echo "${body}" | grep -qiE "${pattern}" 2>/dev/null; then
                echo "${body}"
                return 0
            fi
            # Log non-matching new replies once each, so debugging is easier.
            if [[ "${seen_log}" != *"|${event}|"* ]]; then
                seen_log="${seen_log}|${event}|"
                log_info "Manager reply does not yet match '${pattern}' (waiting): $(echo "${body}" | head -c 200)"
            fi
        done <<< "${new_replies}"
    done

    return 1
}

# Send a mention message to a worker and wait for its reply, with at-least-once
# semantics (resends the message periodically if no reply comes).
#
# Usage:
#   matrix_send_and_wait_for_reply <token> <room_id> <worker_user_id> <body> \
#       [total_timeout=180] [resend_interval=30]
#
# Returns 0 with the reply body on stdout, or 1 on timeout.
#
# WHY: "membership = join" is necessary but NOT sufficient for a worker to be
# ready to process messages. Different runtimes have different readiness gaps:
#
#   - CoPaw: its first-boot catch-up sync intentionally suppresses message
#     callbacks (copaw/src/matrix/channel.py::_sync_loop) so that historical
#     messages aren't replayed on container restart. Any message that arrives
#     between "join" and "next_batch persisted" is silently dropped.
#   - Hermes: hermes-agent's matrix adapter doesn't auto-join invited rooms,
#     so the controller pre-joins on its behalf — which means the room shows
#     "join" before the worker container has even booted its sync loop.
#   - OpenClaw: smaller window but not zero — the matrix plugin still needs
#     to register message handlers after login.
#
# Rather than codifying runtime-specific readiness markers in the test (fragile
# and runtime-coupled), we treat the send as an at-least-once delivery: keep
# resending until the worker actually replies. This mirrors how a real human
# would interact with a chat bot that didn't respond.
#
# The body should be idempotent — i.e. the worker's reply to N copies of the
# same prompt should still be a valid reply. A simple greeting works fine.
matrix_send_and_wait_for_reply() {
    local token="$1"
    local room_id="$2"
    local worker_user="$3"
    local body="$4"
    local total_timeout="${5:-180}"
    local resend_interval="${6:-30}"

    local elapsed=0
    local last_send=-9999  # ensures we send immediately on first iteration
    local send_count=0
    local last_event_id=""

    # Snapshot baseline so we don't return a stale reply from a previous round.
    local baseline_event
    baseline_event=$(matrix_read_messages "${token}" "${room_id}" 20 2>/dev/null | \
        jq -r --arg user "${worker_user}" \
        '[.chunk[] | select(.sender | startswith($user)) | select(.type == "m.room.message") | select(.content.body != null) | .event_id] | first // ""' 2>/dev/null)

    while [ "${elapsed}" -lt "${total_timeout}" ]; do
        # Send (or resend) if it's been long enough since the last send.
        if [ $((elapsed - last_send)) -ge "${resend_interval}" ]; then
            send_count=$((send_count + 1))
            local send_result
            send_result=$(matrix_send_mention_message "${token}" "${room_id}" "${worker_user}" "${body}" 2>&1) || true
            local sent_event
            sent_event=$(echo "${send_result}" | jq -r '.event_id // empty' 2>/dev/null)
            if [ -n "${sent_event}" ] && [ "${sent_event}" != "null" ]; then
                last_event_id="${sent_event}"
                if [ "${send_count}" -eq 1 ]; then
                    log_info "Sent message to worker (event: ${sent_event})"
                else
                    log_info "Resent message to worker (attempt ${send_count}, event: ${sent_event})"
                fi
            else
                log_info "Send attempt ${send_count} failed: ${send_result}"
            fi
            last_send="${elapsed}"
        fi

        sleep 5
        elapsed=$((elapsed + 5))

        local messages
        messages=$(matrix_read_messages "${token}" "${room_id}" 20 2>/dev/null) || continue

        local latest_event latest_body
        latest_event=$(echo "${messages}" | jq -r --arg user "${worker_user}" \
            '[.chunk[] | select(.sender | startswith($user)) | select(.type == "m.room.message") | select(.content.body != null) | .event_id] | first // ""' 2>/dev/null)
        latest_body=$(echo "${messages}" | jq -r --arg user "${worker_user}" \
            '[.chunk[] | select(.sender | startswith($user)) | select(.type == "m.room.message") | select(.content.body != null) | .content.body] | first // empty' 2>/dev/null)

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

# Wait until a specific user has joined a room (membership = join)
# Usage: matrix_wait_for_user_joined <access_token> <room_id> <user_matrix_id> [timeout_seconds]
# Returns 0 when user has joined, 1 on timeout
#
# Use this before sending a message to a user, because `m.room.history_visibility`
# defaults to "shared" in Matrix — messages sent before the user joined will NOT
# be visible to them. Worker containers being "running" is not sufficient; the
# Matrix client inside still needs time to log in and accept the invite.
matrix_wait_for_user_joined() {
    local token="$1"
    local room_id="$2"
    local user_id="$3"
    local timeout="${4:-120}"
    local room_enc
    room_enc="$(_encode_room_id "${room_id}")"
    local elapsed=0

    while [ "${elapsed}" -lt "${timeout}" ]; do
        local members
        members=$(exec_in_manager curl -sf \
            "${TEST_MATRIX_DIRECT_URL}/_matrix/client/v3/rooms/${room_enc}/members" \
            -H "Authorization: Bearer ${token}" 2>/dev/null | \
            jq -r '.chunk[] | select(.content.membership == "join") | .state_key' 2>/dev/null)
        if echo "${members}" | grep -qF "${user_id}"; then
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
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

# Dump recent Manager DM messages to stderr.
#
# Use when a test fails because Manager didn't perform an expected action
# (didn't create a Worker, didn't open a project room, etc.). Manager's last
# few DM replies usually reveal the cause — e.g. it asked for confirmation
# instead of acting, or it errored out internally. Without this dump, those
# failures look like silent infrastructure timeouts.
#
# Usage: dump_manager_dm_messages <token> <dm_room> [note]
dump_manager_dm_messages() {
    local token="$1"
    local dm_room="$2"
    local note="${3:-Manager DM dump}"
    local raw formatted
    raw=$(matrix_read_messages "${token}" "${dm_room}" 20 2>&1 || true)
    {
        printf "\n--- %s ---\n" "${note}"
        # Try to render as one-line-per-message. If matrix_read_messages
        # returned a curl error instead of JSON, jq will fail — fall back to
        # the raw response so the dump always shows something useful.
        if formatted=$(printf '%s' "${raw}" \
            | jq -r '.chunk[] | "[\(.sender // "?")] \(.content.body // "<no body>" | tostring | .[0:400])"' 2>/dev/null) \
            && [ -n "${formatted}" ]; then
            printf '%s\n' "${formatted}" | head -40
        else
            printf 'raw response (jq parse failed):\n%s\n' "${raw}" | head -40
        fi
        printf -- "--- end of Manager DM dump ---\n"
    } >&2
    return 0
}
