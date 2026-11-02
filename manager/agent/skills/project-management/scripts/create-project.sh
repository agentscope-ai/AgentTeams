#!/bin/bash
# create-project.sh - Create a project directory structure and Matrix room
#
# Usage:
#   create-project.sh --id <PROJECT_ID> --title <TITLE> --workers <w1,w2,...>
#
# Prerequisites:
#   - Worker SOUL.md files must already exist
#   - Environment: AGENTTEAMS_MATRIX_DOMAIN, AGENTTEAMS_ADMIN_USER, MANAGER_MATRIX_TOKEN

set -e
source /opt/agentteams/scripts/lib/agentteams-env.sh

PROJECT_ID=""
PROJECT_TITLE=""
WORKERS_CSV=""
GRANT_ADMIN_CSV=""

while [ $# -gt 0 ]; do
    case "$1" in
        --id)          PROJECT_ID="$2"; shift 2 ;;
        --title)       PROJECT_TITLE="$2"; shift 2 ;;
        --workers)     WORKERS_CSV="$2"; shift 2 ;;
        --grant-admin) GRANT_ADMIN_CSV="$2"; shift 2 ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

if [ -z "${PROJECT_ID}" ] || [ -z "${PROJECT_TITLE}" ] || [ -z "${WORKERS_CSV}" ]; then
    echo "Usage: create-project.sh --id <PROJECT_ID> --title <TITLE> --workers <w1,w2,...> [--grant-admin <u1,u2,...>]"
    exit 1
fi

MATRIX_DOMAIN="${AGENTTEAMS_MATRIX_DOMAIN:-matrix-local.agentteams.io:8080}"
ADMIN_USER="${AGENTTEAMS_ADMIN_USER:-admin}"

_fail() {
    echo '{"error": "'"$1"'"}'
    exit 1
}

# Ensure Manager Matrix token is available
SECRETS_FILE="/data/agentteams-secrets.env"
if [ -f "${SECRETS_FILE}" ]; then
    source "${SECRETS_FILE}"
fi
if [ -z "${MANAGER_MATRIX_TOKEN}" ]; then
    MANAGER_MATRIX_TOKEN=$(curl -sf -X POST ${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/login \
        -H 'Content-Type: application/json' \
        -d '{"type":"m.login.password","identifier":{"type":"m.id.user","user":"manager"},"password":"'"${AGENTTEAMS_MANAGER_PASSWORD}"'"}' \
        2>/dev/null | jq -r '.access_token // empty')
    [ -z "${MANAGER_MATRIX_TOKEN}" ] && _fail "Failed to obtain Manager Matrix token"
fi

# ============================================================
# Step 1: Create project directories and files
# ============================================================
log "Step 1: Creating project directories..."
PROJECT_DIR="/root/agentteams-fs/shared/projects/${PROJECT_ID}"
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
GRANT_ADMIN_LEVELS=""
IFS=',' read -ra WORKER_ARR <<< "${WORKERS_CSV}"
for worker in "${WORKER_ARR[@]}"; do
    worker=$(echo "${worker}" | tr -d ' ')
    [ -z "${worker}" ] && continue
    INVITE_LIST="${INVITE_LIST},\"@${worker}:${MATRIX_DOMAIN}\""
    WORKER_POWER_LEVELS="${WORKER_POWER_LEVELS},\"@${worker}:${MATRIX_DOMAIN}\": 0"
done
# --grant-admin: extra users that should co-own the room (level 100) — e.g.
# human operators who otherwise join at the implicit level 0 and 403 on room
# operations (rename / invite). Accepts local parts or full Matrix IDs.
GRANT_ADMIN_IDS=()
if [ -n "${GRANT_ADMIN_CSV}" ]; then
    IFS=',' read -ra GRANT_ARR <<< "${GRANT_ADMIN_CSV}"
    for ga in "${GRANT_ARR[@]}"; do
        ga=$(echo "${ga}" | tr -d ' ')
        [ -z "${ga}" ] && continue
        case "${ga}" in
            @*:*) grant_id="${ga}" ;;
            @*)   grant_id="@${ga#@}:${MATRIX_DOMAIN}" ;;
            *)    grant_id="@${ga}:${MATRIX_DOMAIN}" ;;
        esac
        # The grant only takes effect once the user is in the room — invite
        # them at creation time (a grant without membership is a no-op).
        INVITE_LIST="${INVITE_LIST},\"${grant_id}\""
        GRANT_ADMIN_LEVELS="${GRANT_ADMIN_LEVELS},\"${grant_id}\": 100"
        GRANT_ADMIN_IDS+=("${grant_id}")
    done
fi
INVITE_LIST="${INVITE_LIST}]"

MANAGER_MATRIX_ID="@manager:${MATRIX_DOMAIN}"
ADMIN_MATRIX_ID="@${ADMIN_USER}:${MATRIX_DOMAIN}"
ROOM_RESP=$(curl -sf -X POST ${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/createRoom \
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
                "'"${ADMIN_MATRIX_ID}"'": 100'"${WORKER_POWER_LEVELS}"''"${GRANT_ADMIN_LEVELS}"'
            }
        }
    }' 2>/dev/null) || _fail "Failed to create Matrix project room"

ROOM_ID=$(echo "${ROOM_RESP}" | jq -r '.room_id // empty')
[ -z "${ROOM_ID}" ] && _fail "Failed to create Matrix project room: ${ROOM_RESP}"
log "  Project room created: ${ROOM_ID}"

# Update meta.json with room_id
jq --arg rid "${ROOM_ID}" '.project_room_id = $rid' "${PROJECT_DIR}/meta.json" > /tmp/proj-meta-updated.json
mv /tmp/proj-meta-updated.json "${PROJECT_DIR}/meta.json"
curl -sf -X POST "${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/rooms/${ROOM_ID}/invite" \
    -H "Authorization: Bearer ${MANAGER_MATRIX_TOKEN}" \
    -H 'Content-Type: application/json' \
    -d "{\"user_id\": \"${ADMIN_MATRIX_ID}\"}" > /dev/null 2>&1 || true
log "  Admin ${ADMIN_MATRIX_ID} invited to project room"

# Auto-join admin into project room
ADMIN_TOKEN=""
if [ -n "${AGENTTEAMS_ADMIN_PASSWORD:-}" ]; then
    ADMIN_TOKEN=$(curl -sf -X POST ${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/login \
        -H 'Content-Type: application/json' \
        -d '{"type":"m.login.password","identifier":{"type":"m.id.user","user":"'"${ADMIN_USER}"'"},"password":"'"${AGENTTEAMS_ADMIN_PASSWORD}"'"}' \
        2>/dev/null | jq -r '.access_token // empty')
fi
if [ -n "${ADMIN_TOKEN}" ]; then
    ROOM_ENC=$(echo "${ROOM_ID}" | sed 's/!/%21/g')
    if curl -sf -X POST "${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/rooms/${ROOM_ENC}/join" \
        -H "Authorization: Bearer ${ADMIN_TOKEN}" \
        -H 'Content-Type: application/json' \
        -d '{}' > /dev/null 2>&1; then
        log "  Admin auto-joined project room"
    else
        log "  WARNING: Admin failed to auto-join project room"
    fi
else
    log "  WARNING: Could not obtain admin token — admin will need to accept invite manually"
fi

# Per-worker auto-join outcome ledger (#1390). Rows are
# "<worker>\t<status>\t<detail>" with status one of joined|pending|failed,
# so the caller can surface a result instead of only a warning.
WORKER_JOIN_STATUS_ROWS=""

_record_worker_join() { # _record_worker_join <worker> <status> <detail>
    WORKER_JOIN_STATUS_ROWS="${WORKER_JOIN_STATUS_ROWS}${1}"$'\t'"${2}"$'\t'"${3}"$'\n'
}

_worker_join_summary() { # prints "joined=N pending=N failed=N"
    local joined=0 pending=0 failed=0 status
    while IFS=$'\t' read -r _worker status _detail; do
        case "${status}" in
            joined) joined=$((joined + 1)) ;;
            pending) pending=$((pending + 1)) ;;
            failed) failed=$((failed + 1)) ;;
        esac
    done <<< "${WORKER_JOIN_STATUS_ROWS}"
    printf 'joined=%s pending=%s failed=%s' "${joined}" "${pending}" "${failed}"
}

_write_worker_join_status() { # _write_worker_join_status <path>
    local out="${1:-/tmp/agentteams-project-join-status.json}"
    local tmp="${out}.tmp"
    {
        printf '{\n'
        printf '  "room_id": %s,\n' "$(printf '%s' "${ROOM_ID}" | jq -R .)"
        printf '  "generated_at": %s,\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ | jq -R .)"
        printf '  "summary": %s,\n' "$(_worker_join_summary | jq -R .)"
        printf '  "workers": [\n'
        local first=1 worker status detail
        while IFS=$'\t' read -r worker status detail; do
            [ -z "${worker}" ] && continue
            [ "${first}" -eq 1 ] || printf ',\n'
            first=0
            printf '    {"worker": %s, "status": %s, "detail": %s}' \
                "$(printf '%s' "${worker}" | jq -R .)" \
                "$(printf '%s' "${status}" | jq -R .)" \
                "$(printf '%s' "${detail}" | jq -R .)"
        done <<< "${WORKER_JOIN_STATUS_ROWS}"
        printf '\n  ]\n}\n'
    } > "${tmp}" && mv "${tmp}" "${out}" && log "  Worker join status written to ${out}"
}

# Returns 0 = joined, 2 = pending (no credentials/token), 3 = join failed.
# The ledger above records the detail either way, so the caller can report
# `joined/pending/failed` instead of relying on a log line alone (#1390).
_worker_auto_join() {
    local worker="$1"
    local room_id="$2"
    local creds_file="/data/worker-creds/${worker}.env"
    local worker_token=""
    local room_enc=""
    local WORKER_PASSWORD=""
    local WORKER_MATRIX_TOKEN=""

    if [ -z "${worker}" ] || [ -z "${room_id}" ]; then
        _record_worker_join "${worker:-?}" "pending" "invalid-arguments"
        return 2
    fi

    if [ -f "${creds_file}" ]; then
        # shellcheck disable=SC1090
        source "${creds_file}"
    fi

    if [ -z "${WORKER_PASSWORD}" ]; then
        WORKER_PASSWORD=$(mc cat "${AGENTTEAMS_STORAGE_PREFIX}/agents/${worker}/credentials/matrix/password" 2>/dev/null || true)
    fi
    if [ -z "${WORKER_PASSWORD}" ] && [ -f "/root/agentteams-fs/agents/${worker}/credentials/matrix/password" ]; then
        WORKER_PASSWORD=$(cat "/root/agentteams-fs/agents/${worker}/credentials/matrix/password" 2>/dev/null || true)
    fi

    if [ -n "${WORKER_PASSWORD}" ]; then
        worker_token=$(curl -sf -X POST ${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/login \
            -H 'Content-Type: application/json' \
            -d '{"type":"m.login.password","identifier":{"type":"m.id.user","user":"'"${worker}"'"},"password":"'"${WORKER_PASSWORD}"'"}' \
            2>/dev/null | jq -r '.access_token // empty')
    elif [ -n "${WORKER_MATRIX_TOKEN}" ]; then
        worker_token="${WORKER_MATRIX_TOKEN}"
    fi

    if [ -z "${worker_token}" ]; then
        log "  WARNING: Could not obtain Matrix token for worker ${worker} — worker will need to accept project room invite"
        _record_worker_join "${worker}" "pending" "no-credentials"
        return 2
    fi

    room_enc=$(echo "${room_id}" | sed 's/!/%21/g')
    local join_http
    join_http=$(curl -s -o /dev/null -w '%{http_code}' -X POST "${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/rooms/${room_enc}/join" \
        -H "Authorization: Bearer ${worker_token}" \
        -H 'Content-Type: application/json' \
        -d '{}' 2>/dev/null || true)
    case "${join_http}" in
        200|201)
            log "  Worker ${worker} auto-joined project room"
            _record_worker_join "${worker}" "joined" "http-${join_http}"
            return 0
            ;;
        *)
            log "  WARNING: Worker ${worker} failed to auto-join project room (http ${join_http:-no-response})"
            _record_worker_join "${worker}" "failed" "http-${join_http:-no-response}"
            return 3
            ;;
    esac
}

# Builds the project-room member list (workers + --grant-admin humans) as a
# JSON array. Granted humans must be on the allowlists too: in allowlist
# mode the Manager silently drops messages from users not listed, and the
# room's human operators are exactly the --grant-admin users.
_build_project_room_members_json() {
    {
        for worker in "${WORKER_ARR[@]}"; do
            worker=$(echo "${worker}" | tr -d ' ')
            [ -z "${worker}" ] && continue
            echo "@${worker}:${MATRIX_DOMAIN}"
        done
        for uid in "${GRANT_ADMIN_IDS[@]}"; do
            [ -z "${uid}" ] && continue
            echo "${uid}"
        done
    } | jq -R . | jq -s .
}

_patch_manager_project_room_config() {
    local config_path="$1"
    local members_json=""
    [ -f "${config_path}" ] || return 0

    members_json=$(_build_project_room_members_json)

    jq --arg room "${ROOM_ID}" --argjson members "${members_json}" \
        ".channels.matrix.groupAllowFrom = ((.channels.matrix.groupAllowFrom // []) + \$members | unique)
         | .channels.matrix.groups = (.channels.matrix.groups // {})
         | .channels.matrix.groups[\$room] = {\"allow\": true, \"requireMention\": false, \"autoReply\": true}" \
        "${config_path}" > /tmp/project-manager-config.json
    mv /tmp/project-manager-config.json "${config_path}"
}

_patch_copaw_project_room_config() {
    local agent_json="${HOME}/.copaw/workspaces/default/agent.json"
    local members_json=""
    [ -f "${agent_json}" ] || return 0

    members_json=$(_build_project_room_members_json)

    jq --arg room "${ROOM_ID}" --argjson members "${members_json}" \
        ".channels.matrix.group_allow_from = ((.channels.matrix.group_allow_from // []) + \$members | unique)
         | .channels.matrix.groups = (.channels.matrix.groups // {})
         | .channels.matrix.groups[\$room] = {\"allow\": true, \"requireMention\": false, \"autoReply\": true}" \
        "${agent_json}" > /tmp/project-copaw-agent.json
    mv /tmp/project-copaw-agent.json "${agent_json}"
}

for worker in "${WORKER_ARR[@]}"; do
    worker=$(echo "${worker}" | tr -d ' ')
    [ -z "${worker}" ] && continue
    # set -e is on: a pending/failed join must not abort room creation, so
    # the per-worker outcome is recorded and reported instead (#1390).
    _worker_auto_join "${worker}" "${ROOM_ID}" || true
done
if [ -n "${WORKER_JOIN_STATUS_ROWS}" ]; then
    log "Worker auto-join: $(_worker_join_summary)"
    _write_worker_join_status "${AGENTTEAMS_JOIN_STATUS_FILE:-/tmp/agentteams-project-join-status.json}"
fi

# ============================================================
# Step 3: Add Workers to Manager's groupAllowFrom
# ============================================================
log "Step 3: Updating Manager groupAllowFrom..."
MANAGER_CONFIG="/root/agentteams-fs/agents/manager/openclaw.json"
if [ -f "${MANAGER_CONFIG}" ]; then
    _patch_manager_project_room_config "${MANAGER_CONFIG}"
    # Sync updated Manager config to MinIO
    mc cp "${MANAGER_CONFIG}" "${AGENTTEAMS_STORAGE_PREFIX}/agents/manager/openclaw.json" 2>/dev/null || true
    log "  Manager config synced to MinIO"
fi
if [ -f "${HOME}/openclaw.json" ]; then
    _patch_manager_project_room_config "${HOME}/openclaw.json"
    log "  Manager runtime openclaw.json updated"
fi
_patch_copaw_project_room_config
log "  CoPaw Manager project room config updated when available"

# ============================================================
# Step 4: Sync project files to MinIO
# ============================================================
log "Step 4: Syncing project files to MinIO..."
mc mirror "${PROJECT_DIR}/" "${AGENTTEAMS_STORAGE_PREFIX}/shared/projects/${PROJECT_ID}/" --overwrite 2>&1 | tail -3
mc stat "${AGENTTEAMS_STORAGE_PREFIX}/shared/projects/${PROJECT_ID}/meta.json" > /dev/null 2>&1 \
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
