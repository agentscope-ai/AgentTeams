#!/bin/bash
# send-project-message.sh - Send a Matrix message into a project room.
#
# Usage:
#   send-project-message.sh --room-id <ROOM_ID> --target-user <MXID> --text <TEXT>

set -e
source /opt/hiclaw/scripts/lib/hiclaw-env.sh

ROOM_ID=""
TARGET_USER=""
TEXT=""

while [ $# -gt 0 ]; do
    case "$1" in
        --room-id)      ROOM_ID="$2"; shift 2 ;;
        --target-user)  TARGET_USER="$2"; shift 2 ;;
        --text)         TEXT="$2"; shift 2 ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

if [ -z "${ROOM_ID}" ] || [ -z "${TARGET_USER}" ] || [ -z "${TEXT}" ]; then
    echo "Usage: send-project-message.sh --room-id <ROOM_ID> --target-user <MXID> --text <TEXT>" >&2
    exit 1
fi

if command -v copaw >/dev/null 2>&1; then
    if copaw channels send \
        --agent-id default \
        --channel matrix \
        --target-user "${TARGET_USER}" \
        --target-session "${ROOM_ID}" \
        --text "${TEXT}"; then
        exit 0
    fi
    log "WARNING: copaw channels send failed for ${ROOM_ID}; falling back to Matrix API"
fi

SECRETS_FILE="/data/hiclaw-secrets.env"
if [ -f "${SECRETS_FILE}" ]; then
    source "${SECRETS_FILE}"
fi

if [ -z "${MANAGER_MATRIX_TOKEN:-}" ]; then
    MANAGER_MATRIX_TOKEN=$(curl -sf -X POST "${HICLAW_MATRIX_URL}/_matrix/client/v3/login" \
        -H 'Content-Type: application/json' \
        -d '{"type":"m.login.password","identifier":{"type":"m.id.user","user":"manager"},"password":"'"${HICLAW_MANAGER_PASSWORD}"'"}' \
        2>/dev/null | jq -r '.access_token // empty')
fi

if [ -z "${MANAGER_MATRIX_TOKEN:-}" ]; then
    echo "ERROR: failed to obtain Manager Matrix token" >&2
    exit 1
fi

ROOM_ENC=$(jq -rn --arg v "${ROOM_ID}" '$v|@uri')
TXN_ID="project-$(date +%s%N)"
PAYLOAD=$(jq -n \
    --arg body "${TEXT}" \
    --arg target "${TARGET_USER}" \
    '{
        msgtype: "m.text",
        body: $body,
        "m.mentions": {user_ids: [$target]}
    }')

curl -sf -X PUT \
    "${HICLAW_MATRIX_URL}/_matrix/client/v3/rooms/${ROOM_ENC}/send/m.room.message/${TXN_ID}" \
    -H "Authorization: Bearer ${MANAGER_MATRIX_TOKEN}" \
    -H 'Content-Type: application/json' \
    -d "${PAYLOAD}" > /dev/null

echo "OK: message sent to ${ROOM_ID}"
