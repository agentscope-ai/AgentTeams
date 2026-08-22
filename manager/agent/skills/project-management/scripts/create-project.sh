#!/bin/bash
# create-project.sh - Create a project directory structure and Matrix room
#
# Usage:
#   create-project.sh --id <PROJECT_ID> --title <TITLE> --workers <w1,w2,...>
#
# Prerequisites:
#   - Worker SOUL.md files must already exist
#   - Environment: HICLAW_MATRIX_DOMAIN, HICLAW_ADMIN_USER, MANAGER_MATRIX_TOKEN

set -e
source /opt/hiclaw/scripts/lib/hiclaw-env.sh

PROJECT_ID=""
PROJECT_TITLE=""
WORKERS_CSV=""

while [ $# -gt 0 ]; do
    case "$1" in
        --id)      PROJECT_ID="$2"; shift 2 ;;
        --title)   PROJECT_TITLE="$2"; shift 2 ;;
        --workers) WORKERS_CSV="$2"; shift 2 ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

if [ -z "${PROJECT_ID}" ] || [ -z "${PROJECT_TITLE}" ] || [ -z "${WORKERS_CSV}" ]; then
    echo "Usage: create-project.sh --id <PROJECT_ID> --title <TITLE> --workers <w1,w2,...>"
    exit 1
fi

MATRIX_DOMAIN="${HICLAW_MATRIX_DOMAIN:-matrix-local.hiclaw.io:8080}"
ADMIN_USER="${HICLAW_ADMIN_USER:-admin}"

_fail() {
    echo '{"error": "'"$1"'"}'
    exit 1
}

# Ensure Manager Matrix token is available
SECRETS_FILE="/data/hiclaw-secrets.env"
if [ -f "${SECRETS_FILE}" ]; then
    source "${SECRETS_FILE}"
fi
if [ -z "${MANAGER_MATRIX_TOKEN}" ]; then
    MANAGER_MATRIX_TOKEN=$(curl -sf -X POST ${HICLAW_MATRIX_SERVER}/_matrix/client/v3/login \
        -H 'Content-Type: application/json' \
        -d '{"type":"m.login.password","identifier":{"type":"m.id.user","user":"manager"},"password":"'"${HICLAW_MANAGER_PASSWORD}"'"}' \
        2>/dev/null | jq -r '.access_token // empty')
    [ -z "${MANAGER_MATRIX_TOKEN}" ] && _fail "Failed to obtain Manager Matrix token"
fi

# ============================================================
# Step 1: Create project directories and files
# ============================================================
log "Step 1: Creating project directories..."
PROJECT_DIR="/root/hiclaw-fs/shared/projects/${PROJECT_ID}"
mkdir -p "${PROJECT_DIR}"

NOW=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

WORKERS_JSON="[$(echo "${WORKERS_CSV}" | tr ',' '\n' | sed 's/.*/"&"/' | tr '\n' ',' | sed 's/,$//')]"

cat > "${PROJECT_DIR}/meta.json" << EOF
{
  "project_id": "${PROJECT_ID}",
  "title": "${PROJECT_TITLE}",
  "project_room_id": null,
  "status": "planning",
  "workers": ${WORKERS_JSON},
  "created_at": "${NOW}",
  "confirmed_at": null
}
EOF

# Write a minimal plan.md placeholder (Manager agent will fill in the full plan)
cat > "${PROJECT_DIR}/plan.md" << EOF
# Project: ${PROJECT_TITLE}

**ID**: ${PROJECT_ID}
**Status**: planning
**Room**: (pending)
**Created**: ${NOW}
**Confirmed**: pending

## Team

- @manager:${MATRIX_DOMAIN} — Project Manager
$(echo "${WORKERS_CSV}" | tr ',' '\n' | while read -r w; do echo "- @${w}:${MATRIX_DOMAIN} — (role TBD)"; done)

## Task Plan

(To be filled in by Manager)

## Change Log

- ${NOW}: Project initiated
EOF

log "  Project files created at ${PROJECT_DIR}"

# ============================================================
# Step 2: Create Matrix Project Room
# ============================================================
log "Step 2: Creating Matrix project room..."

# Build invite list and worker power level overrides (all workers → level 0)
INVITE_LIST="[\"@${ADMIN_USER}:${MATRIX_DOMAIN}\""
WORKER_POWER_LEVELS=""
IFS=',' read -ra WORKER_ARR <<< "${WORKERS_CSV}"
for worker in "${WORKER_ARR[@]}"; do
    worker=$(echo "${worker}" | tr -d ' ')
    [ -z "${worker}" ] && continue
    INVITE_LIST="${INVITE_LIST},\"@${worker}:${MATRIX_DOMAIN}\""
    WORKER_POWER_LEVELS="${WORKER_POWER_LEVELS},\"@${worker}:${MATRIX_DOMAIN}\": 0"
done
INVITE_LIST="${INVITE_LIST}]"

MANAGER_MATRIX_ID="@manager:${MATRIX_DOMAIN}"
ADMIN_MATRIX_ID="@${ADMIN_USER}:${MATRIX_DOMAIN}"
ROOM_RESP=$(curl -sf -X POST ${HICLAW_MATRIX_SERVER}/_matrix/client/v3/createRoom \
    -H "Authorization: Bearer ${MANAGER_MATRIX_TOKEN}" \
    -H 'Content-Type: application/json' \
    -d '{
        "name": "Project: '"${PROJECT_TITLE}"'",
        "topic": "Project room for '"${PROJECT_TITLE}"' — managed by @manager",
        "invite": '"${INVITE_LIST}"',
        "preset": "trusted_private_chat",
        "power_level_content_override": {
            "users": {
                "'"${MANAGER_MATRIX_ID}"'": 100,
                "'"${ADMIN_MATRIX_ID}"'": 100'"${WORKER_POWER_LEVELS}"'
            }
        }
    }' 2>/dev/null) || _fail "Failed to create Matrix project room"

ROOM_ID=$(echo "${ROOM_RESP}" | jq -r '.room_id // empty')
[ -z "${ROOM_ID}" ] && _fail "Failed to create Matrix project room: ${ROOM_RESP}"
log "  Project room created: ${ROOM_ID}"

# Update meta.json with room_id
jq --arg rid "${ROOM_ID}" '.project_room_id = $rid' "${PROJECT_DIR}/meta.json" > /tmp/proj-meta-updated.json
mv /tmp/proj-meta-updated.json "${PROJECT_DIR}/meta.json"
curl -sf -X POST "${HICLAW_MATRIX_SERVER}/_matrix/client/v3/rooms/${ROOM_ID}/invite" \
    -H "Authorization: Bearer ${MANAGER_MATRIX_TOKEN}" \
    -H 'Content-Type: application/json' \
    -d "{\"user_id\": \"${ADMIN_MATRIX_ID}\"}" > /dev/null 2>&1 || true
log "  Admin ${ADMIN_MATRIX_ID} invited to project room"

# ============================================================
# Step 3: Add Workers to Manager's groupAllowFrom
# ============================================================
log "Step 3: Updating Manager groupAllowFrom..."
MANAGER_CONFIG="/root/hiclaw-fs/agents/manager/openclaw.json"
if [ -f "${MANAGER_CONFIG}" ]; then
    UPDATED_CONFIG="${MANAGER_CONFIG}"
    for worker in "${WORKER_ARR[@]}"; do
        worker=$(echo "${worker}" | tr -d ' ')
        [ -z "${worker}" ] && continue
        WORKER_MATRIX_ID="@${worker}:${MATRIX_DOMAIN}"
        ALREADY_IN=$(jq -r --arg w "${WORKER_MATRIX_ID}" \
            '.channels.matrix.groupAllowFrom // [] | map(select(. == $w)) | length' \
            "${UPDATED_CONFIG}" 2>/dev/null || echo "0")
        if [ "${ALREADY_IN}" = "0" ]; then
            jq --arg w "${WORKER_MATRIX_ID}" \
                '.channels.matrix.groupAllowFrom += [$w]' \
                "${UPDATED_CONFIG}" > /tmp/manager-cfg-updated.json
            mv /tmp/manager-cfg-updated.json "${UPDATED_CONFIG}"
            log "  Added ${WORKER_MATRIX_ID} to groupAllowFrom"
        else
            log "  ${WORKER_MATRIX_ID} already in groupAllowFrom"
        fi
    done
    # Sync updated Manager config to MinIO
    mc cp "${MANAGER_CONFIG}" "${HICLAW_STORAGE_PREFIX}/agents/manager/openclaw.json" 2>/dev/null || true
    log "  Manager config synced to MinIO"
fi

# ============================================================
# Step 4: Sync project files to MinIO
# ============================================================
log "Step 4: Syncing project files to MinIO..."
mc mirror "${PROJECT_DIR}/" "${HICLAW_STORAGE_PREFIX}/shared/projects/${PROJECT_ID}/" --overwrite 2>&1 | tail -3
mc stat "${HICLAW_STORAGE_PREFIX}/shared/projects/${PROJECT_ID}/meta.json" > /dev/null 2>&1 \
    || _fail "meta.json not found in MinIO after sync"
log "  MinIO sync verified"

# ============================================================
# Output JSON result
# ============================================================
RESULT=$(jq -n \
    --arg id "${PROJECT_ID}" \
    --arg title "${PROJECT_TITLE}" \
    --arg room_id "${ROOM_ID}" \
    --arg status "planning" \
    --arg workers "${WORKERS_CSV}" \
    '{
        project_id: $id,
        title: $title,
        project_room_id: $room_id,
        status: $status,
        workers: ($workers | split(","))
    }')

echo "---RESULT---"
echo "${RESULT}"
