#!/bin/bash
# enable-peer-mentions.sh - Enable direct @mention between a set of Workers
#
# By default, Workers can only be @mentioned by Manager and the human admin.
# This script adds each Worker in the group to every other Worker's
# groupAllowFrom, so they can trigger each other directly in project rooms.
#
# Usage:
#   enable-peer-mentions.sh --workers w1,w2,w3
#
# Example:
#   enable-peer-mentions.sh --workers alice,bob,charlie
#
# Prerequisites:
#   - All named Workers must already exist (created via create-worker.sh)
#   - Environment: HICLAW_MATRIX_DOMAIN, MANAGER_MATRIX_TOKEN

set -e
source /opt/hiclaw/scripts/lib/hiclaw-env.sh

# ============================================================
# Parse arguments
# ============================================================
WORKERS_CSV=""

while [ $# -gt 0 ]; do
    case "$1" in
        --workers) WORKERS_CSV="$2"; shift 2 ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

if [ -z "${WORKERS_CSV}" ]; then
    echo "Usage: enable-peer-mentions.sh --workers w1,w2,w3"
    exit 1
fi

MATRIX_DOMAIN="${HICLAW_MATRIX_DOMAIN:-matrix-local.hiclaw.io:8080}"
REGISTRY_FILE="${HOME}/workers-registry.json"

_fail() {
    echo '{"error": "'"$1"'"}'
    exit 1
}

# ============================================================
# Ensure Manager Matrix token is available
# ============================================================
SECRETS_FILE="/data/hiclaw-secrets.env"
if [ -f "${SECRETS_FILE}" ]; then
    source "${SECRETS_FILE}"
fi

if [ -z "${MANAGER_MATRIX_TOKEN}" ]; then
    if [ -z "${HICLAW_MANAGER_PASSWORD}" ]; then
        _fail "MANAGER_MATRIX_TOKEN not set and HICLAW_MANAGER_PASSWORD not available"
    fi
    MANAGER_MATRIX_TOKEN=$(curl -sf -X POST ${HICLAW_MATRIX_SERVER}/_matrix/client/v3/login \
        -H 'Content-Type: application/json' \
        -d '{"type":"m.login.password","identifier":{"type":"m.id.user","user":"manager"},"password":"'"${HICLAW_MANAGER_PASSWORD}"'"}' \
        | jq -r '.access_token // empty')
    if [ -z "${MANAGER_MATRIX_TOKEN}" ]; then
        _fail "Failed to obtain Manager Matrix token"
    fi
fi

# ============================================================
# Parse worker list and validate
# ============================================================
IFS=',' read -ra WORKER_ARR <<< "${WORKERS_CSV}"
VALIDATED=()
for w in "${WORKER_ARR[@]}"; do
    w=$(echo "${w}" | tr -d ' ')
    [ -z "${w}" ] && continue
    SOUL_FILE="/root/hiclaw-fs/agents/${w}/SOUL.md"
    if [ ! -f "${SOUL_FILE}" ]; then
        _fail "Worker '${w}' not found (no SOUL.md at ${SOUL_FILE})"
    fi
    VALIDATED+=("${w}")
done

if [ "${#VALIDATED[@]}" -lt 2 ]; then
    _fail "Need at least 2 workers to enable peer mentions"
fi

log "Enabling peer @mentions for: ${VALIDATED[*]}"

# ============================================================
# For each Worker A: add all other Workers B to A's groupAllowFrom
# ============================================================
UPDATED_WORKERS=()

for target in "${VALIDATED[@]}"; do
    TARGET_CONFIG="/root/hiclaw-fs/agents/${target}/openclaw.json"
    TARGET_MINIO="${HICLAW_STORAGE_PREFIX}/agents/${target}/openclaw.json"

    # Pull latest config from MinIO
    if ! mc cp "${TARGET_MINIO}" "${TARGET_CONFIG}" 2>/dev/null; then
        log "WARNING: Could not pull openclaw.json for ${target} from MinIO, using local copy"
    fi

    if [ ! -f "${TARGET_CONFIG}" ]; then
        log "WARNING: No openclaw.json for ${target}, skipping"
        continue
    fi

    CHANGED=false
    for peer in "${VALIDATED[@]}"; do
        [ "${peer}" = "${target}" ] && continue
        PEER_ID="@${peer}:${MATRIX_DOMAIN}"

        ALREADY=$(jq -r --arg w "${PEER_ID}" \
            '.channels.matrix.groupAllowFrom // [] | map(select(. == $w)) | length' \
            "${TARGET_CONFIG}" 2>/dev/null || echo "0")

        if [ "${ALREADY}" = "0" ]; then
            jq --arg w "${PEER_ID}" '.channels.matrix.groupAllowFrom += [$w]' \
                "${TARGET_CONFIG}" > /tmp/oclaw-peer-tmp.json
            mv /tmp/oclaw-peer-tmp.json "${TARGET_CONFIG}"
            log "  ${target}: added ${PEER_ID} to groupAllowFrom"
            CHANGED=true
        else
            log "  ${target}: ${PEER_ID} already in groupAllowFrom"
        fi
    done

    if [ "${CHANGED}" = true ]; then
        # Push updated config to MinIO
        if mc cp "${TARGET_CONFIG}" "${TARGET_MINIO}" 2>/dev/null; then
            log "  ${target}: config synced to MinIO"
            UPDATED_WORKERS+=("${target}")
        else
            log "  WARNING: Failed to push updated config for ${target} to MinIO"
        fi
    fi
done

# ============================================================
# Notify updated Workers to reload config
# ============================================================
for w in "${UPDATED_WORKERS[@]}"; do
    ROOM_ID=$(jq -r --arg w "${w}" '.workers[$w].room_id // empty' \
        "${REGISTRY_FILE}" 2>/dev/null || true)
    if [ -n "${ROOM_ID}" ]; then
        PEERS=$(printf '%s\n' "${VALIDATED[@]}" | grep -v "^${w}$" | sed "s|^|@|; s|$|:${MATRIX_DOMAIN}|" | tr '\n' ' ' | sed 's/ $//')
        TXN_ID=$(openssl rand -hex 8)
        curl -sf -X PUT \
            "${HICLAW_MATRIX_SERVER}/_matrix/client/v3/rooms/${ROOM_ID}/send/m.room.message/${TXN_ID}" \
            -H "Authorization: Bearer ${MANAGER_MATRIX_TOKEN}" \
            -H 'Content-Type: application/json' \
            -d "{\"msgtype\":\"m.text\",\"body\":\"@${w}:${MATRIX_DOMAIN} Peer mention enabled: you can now be @mentioned by ${PEERS}. Please run: hiclaw-sync\",\"m.mentions\":{\"user_ids\":[\"@${w}:${MATRIX_DOMAIN}\"]}}" \
            > /dev/null 2>&1 \
            && log "  Notified @${w} to run hiclaw-sync" \
            || log "  WARNING: Failed to notify @${w}"
    fi
done

# ============================================================
# Output result
# ============================================================
WORKERS_JSON=$(printf '"%s",' "${VALIDATED[@]}" | sed 's/,$//')
echo "{\"status\":\"ok\",\"workers\":[${WORKERS_JSON}],\"updated\":${#UPDATED_WORKERS[@]}}"
