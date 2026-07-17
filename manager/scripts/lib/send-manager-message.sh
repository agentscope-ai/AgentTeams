#!/bin/bash
# send-manager-message.sh - Send a Matrix room message during Manager bootstrap
#
# Mirrors send-worker-greeting.sh runtime branching for init-time shell sends
# (welcome DM, worker builtin notify). CoPaw uses `copaw channels send` for
# formatted_body; OpenClaw bootstrap keeps curl + m.mentions when needed.
#
# Usage:
#   send-manager-message.sh --room <ROOM_ID> --text <TEXT> [--token <ACCESS_TOKEN>]
#       [--mention-user <@user:domain>] [--wait-runtime]
#
# Environment:
#   AGENTTEAMS_MANAGER_RUNTIME  openclaw (default) | copaw
#   AGENTTEAMS_MATRIX_URL       Matrix client API base URL

set -euo pipefail

ROOM=""
TEXT=""
TOKEN=""
MENTION_USER=""
WAIT_RUNTIME=false

while [ $# -gt 0 ]; do
    case "$1" in
        --room)         ROOM="$2"; shift 2 ;;
        --text)         TEXT="$2"; shift 2 ;;
        --token)        TOKEN="$2"; shift 2 ;;
        --mention-user) MENTION_USER="$2"; shift 2 ;;
        --wait-runtime) WAIT_RUNTIME=true; shift ;;
        -h|--help)
            grep '^#' "$0" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *)
            echo "Unknown option: $1" >&2
            exit 1
            ;;
    esac
done

if [ -z "${ROOM}" ] || [ -z "${TEXT}" ]; then
    echo "Usage: send-manager-message.sh --room <ROOM_ID> --text <TEXT> [--token <TOKEN>] [--mention-user <MXID>] [--wait-runtime]" >&2
    exit 1
fi

RUNTIME="${AGENTTEAMS_MANAGER_RUNTIME:-openclaw}"
MATRIX_URL="${AGENTTEAMS_MATRIX_URL:?AGENTTEAMS_MATRIX_URL is required}"

_wait_for_runtime() {
    local wait=0
    while [ "${wait}" -lt 300 ]; do
        if curl -sf http://127.0.0.1:18799/ > /dev/null 2>&1; then
            return 0
        fi
        sleep 3
        wait=$((wait + 3))
    done
    echo "[send-manager-message] WARNING: runtime not ready on :18799 within 300s" >&2
    return 1
}

_send_via_copaw() {
    local args=(
        copaw channels send
        --agent-id default
        --channel matrix
        --target-session "${ROOM}"
        --text "${TEXT}"
    )
    if [ -n "${MENTION_USER}" ]; then
        args+=(--target-user "${MENTION_USER}")
    fi
    "${args[@]}"
}

_send_via_curl() {
    if [ -z "${TOKEN}" ]; then
        echo "[send-manager-message] ERROR: --token is required for OpenClaw curl send" >&2
        return 1
    fi
    local txn_id="mgr-msg-$(date +%s%N)"
    local payload
    if [ -n "${MENTION_USER}" ]; then
        payload=$(jq -nc --arg body "${TEXT}" --arg user "${MENTION_USER}" \
            '{"msgtype":"m.text","body":$body,"m.mentions":{"user_ids":[$user]}}')
    else
        payload=$(jq -nc --arg body "${TEXT}" '{"msgtype":"m.text","body":$body}')
    fi
    local raw http_code resp
    raw=$(curl -s -w '\nHTTP_CODE:%{http_code}' -X PUT \
        "${MATRIX_URL}/_matrix/client/v3/rooms/${ROOM}/send/m.room.message/${txn_id}" \
        -H "Authorization: Bearer ${TOKEN}" \
        -H 'Content-Type: application/json' \
        -d "${payload}" 2>&1) || true
    http_code=$(echo "${raw}" | tail -1 | sed 's/HTTP_CODE://')
    resp=$(echo "${raw}" | sed '$d')
    if echo "${resp}" | jq -e '.event_id' > /dev/null 2>&1; then
        echo "${resp}"
        return 0
    fi
    echo "[send-manager-message] WARNING: curl send failed (HTTP ${http_code}): ${resp}" >&2
    return 1
}

case "${RUNTIME}" in
    copaw)
        if [ "${WAIT_RUNTIME}" = true ]; then
            _wait_for_runtime || exit 0
        fi
        _send_via_copaw
        ;;
    openclaw|*)
        _send_via_curl
        ;;
esac
