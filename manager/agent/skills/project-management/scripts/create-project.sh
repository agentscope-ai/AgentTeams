#!/bin/bash
# create-project.sh - Create a project directory structure and Matrix room
#
# Usage:
#   create-project.sh --id <PROJECT_ID> --title <TITLE> --workers <w1,w2,...> \
#     [--team <TEAM_NAME>] [--repo <URL>:<rw|ro> ...]
#
# Prerequisites:
#   - Worker SOUL.md files must already exist
#   - Environment: AGENTTEAMS_MATRIX_DOMAIN, AGENTTEAMS_ADMIN_USER, MANAGER_MATRIX_TOKEN

set -e
source /opt/hiclaw/scripts/lib/hiclaw-env.sh

PROJECT_ID=""
PROJECT_TITLE=""
WORKERS_CSV=""
PROJECT_TEAM=""
declare -a PROJECT_REPOS=()

while [ $# -gt 0 ]; do
    case "$1" in
        --id)      PROJECT_ID="$2"; shift 2 ;;
        --title)   PROJECT_TITLE="$2"; shift 2 ;;
        --workers) WORKERS_CSV="$2"; shift 2 ;;
        --team)    PROJECT_TEAM="$2"; shift 2 ;;
        --repo)    PROJECT_REPOS+=("$2"); shift 2 ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

if [ -z "${PROJECT_ID}" ] || [ -z "${PROJECT_TITLE}" ] || [ -z "${WORKERS_CSV}" ]; then
    echo "Usage: create-project.sh --id <PROJECT_ID> --title <TITLE> --workers <w1,w2,...> [--team <TEAM_NAME>] [--repo <URL>:<rw|ro> ...]"
    exit 1
fi

MATRIX_DOMAIN="${AGENTTEAMS_MATRIX_DOMAIN:-matrix-local.agentteams.io:8080}"
ADMIN_USER="${AGENTTEAMS_ADMIN_USER:-admin}"

_fail() {
    echo '{"error": "'"$1"'"}'
    exit 1
}

# Escape an arbitrary string for use as a YAML double-quoted scalar and wrap
# it in the surrounding quotes. Chat-origin values (PROJECT_TITLE especially)
# must never be interpolated into emitted YAML unescaped — a stray '"', '\',
# or newline would break out of the scalar and corrupt/inject the document
# that is later fed to `hiclaw apply -f`.
# Order matters: escape backslashes FIRST (so we don't double-escape the
# backslashes we're about to introduce for \n / \t), then quotes, then map
# newlines/tabs to their YAML escapes, and finally drop bare CRs.
yaml_dq() {
    local s="$1"
    s="${s//\\/\\\\}"
    s="${s//\"/\\\"}"
    s="${s//$'\n'/\\n}"
    s="${s//$'\r'/}"
    s="${s//$'\t'/\\t}"
    printf '"%s"' "$s"
}

# Validate the optional federation flags (--team / --repo) up front, before
# any Matrix/MinIO side effects — a bad --repo access value should never
# leave a half-created project behind.
if [ -n "${PROJECT_TEAM}" ] && [ "${#PROJECT_REPOS[@]}" -eq 0 ]; then
    _fail "--team requires at least one --repo <url>:<rw|ro>"
fi
if [ "${#PROJECT_REPOS[@]}" -gt 0 ] && [ -z "${PROJECT_TEAM}" ]; then
    _fail "--repo requires --team"
fi
for _repo_spec in "${PROJECT_REPOS[@]+"${PROJECT_REPOS[@]}"}"; do
    _repo_access="${_repo_spec##*:}"
    if [ "${_repo_access}" != "rw" ] && [ "${_repo_access}" != "ro" ]; then
        _fail "--repo access must be rw or ro: ${_repo_spec}"
    fi
done

# PROJECT_ID and PROJECT_TEAM must be k8s-name-safe anyway (they become
# Project/Team resource identifiers), so hard-validate against a safe
# charset here — this also guarantees they can never break out of an
# unquoted YAML scalar (':' or a leading '- ' would otherwise misparse).
_SAFE_NAME_RE='^[A-Za-z0-9._-]+$'
if ! [[ "${PROJECT_ID}" =~ ${_SAFE_NAME_RE} ]]; then
    _fail "--id must match ${_SAFE_NAME_RE}: ${PROJECT_ID}"
fi
if [ -n "${PROJECT_TEAM}" ] && ! [[ "${PROJECT_TEAM}" =~ ${_SAFE_NAME_RE} ]]; then
    _fail "--team must match ${_SAFE_NAME_RE}: ${PROJECT_TEAM}"
fi

# Ensure Manager Matrix token is available
SECRETS_FILE="/data/hiclaw-secrets.env"
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

_worker_auto_join() {
    local worker="$1"
    local room_id="$2"
    local creds_file="/data/worker-creds/${worker}.env"
    local worker_token=""
    local room_enc=""
    local WORKER_PASSWORD=""
    local WORKER_MATRIX_TOKEN=""

    if [ -z "${worker}" ] || [ -z "${room_id}" ]; then
        return 0
    fi

    if [ -f "${creds_file}" ]; then
        # shellcheck disable=SC1090
        source "${creds_file}"
    fi

    if [ -z "${WORKER_PASSWORD}" ]; then
        WORKER_PASSWORD=$(mc cat "${AGENTTEAMS_STORAGE_PREFIX}/agents/${worker}/credentials/matrix/password" 2>/dev/null || true)
    fi
    if [ -z "${WORKER_PASSWORD}" ] && [ -f "/root/hiclaw-fs/agents/${worker}/credentials/matrix/password" ]; then
        WORKER_PASSWORD=$(cat "/root/hiclaw-fs/agents/${worker}/credentials/matrix/password" 2>/dev/null || true)
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
        return 0
    fi

    room_enc=$(echo "${room_id}" | sed 's/!/%21/g')
    if curl -sf -X POST "${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/rooms/${room_enc}/join" \
        -H "Authorization: Bearer ${worker_token}" \
        -H 'Content-Type: application/json' \
        -d '{}' > /dev/null 2>&1; then
        log "  Worker ${worker} auto-joined project room"
    else
        log "  WARNING: Worker ${worker} failed to auto-join project room"
    fi
}

_patch_manager_project_room_config() {
    local config_path="$1"
    local workers_json=""
    [ -f "${config_path}" ] || return 0

    workers_json=$(
        for worker in "${WORKER_ARR[@]}"; do
            worker=$(echo "${worker}" | tr -d ' ')
            [ -z "${worker}" ] && continue
            echo "@${worker}:${MATRIX_DOMAIN}"
        done | jq -R . | jq -s .
    )

    jq --arg room "${ROOM_ID}" --argjson workers "${workers_json}" \
        ".channels.matrix.groupAllowFrom = ((.channels.matrix.groupAllowFrom // []) + \$workers | unique)
         | .channels.matrix.groups = (.channels.matrix.groups // {})
         | .channels.matrix.groups[\$room] = {\"allow\": true, \"requireMention\": false, \"autoReply\": true}" \
        "${config_path}" > /tmp/project-manager-config.json
    mv /tmp/project-manager-config.json "${config_path}"
}

_patch_copaw_project_room_config() {
    local agent_json="${HOME}/.copaw/workspaces/default/agent.json"
    local workers_json=""
    [ -f "${agent_json}" ] || return 0

    workers_json=$(
        for worker in "${WORKER_ARR[@]}"; do
            worker=$(echo "${worker}" | tr -d ' ')
            [ -z "${worker}" ] && continue
            echo "@${worker}:${MATRIX_DOMAIN}"
        done | jq -R . | jq -s .
    )

    jq --arg room "${ROOM_ID}" --argjson workers "${workers_json}" \
        ".channels.matrix.group_allow_from = ((.channels.matrix.group_allow_from // []) + \$workers | unique)
         | .channels.matrix.groups = (.channels.matrix.groups // {})
         | .channels.matrix.groups[\$room] = {\"allow\": true, \"requireMention\": false, \"autoReply\": true}" \
        "${agent_json}" > /tmp/project-copaw-agent.json
    mv /tmp/project-copaw-agent.json "${agent_json}"
}

for worker in "${WORKER_ARR[@]}"; do
    worker=$(echo "${worker}" | tr -d ' ')
    [ -z "${worker}" ] && continue
    _worker_auto_join "${worker}" "${ROOM_ID}"
done

# ============================================================
# Step 3: Add Workers to Manager's groupAllowFrom
# ============================================================
log "Step 3: Updating Manager groupAllowFrom..."
MANAGER_CONFIG="/root/hiclaw-fs/agents/manager/openclaw.json"
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
# Step 5 (optional): Federated Project CR — repo/access provisioning layer
# ============================================================
# Only runs when --team was given (decision #16). This is IN ADDITION to the
# meta.json/plan.md chat-flow written above, never a replacement — same
# project id, two documents, no schema merge. `hiclaw apply -f` hits the
# controller's Project REST routes (Step 1, /api/v1/projects); the Manager
# image bundles the `hiclaw` CLI (manager/Dockerfile.copaw).
if [ -n "${PROJECT_TEAM}" ]; then
    log "Step 5: Applying federated Project CR (team=${PROJECT_TEAM})..."

    PROJECT_CR_FILE="${PROJECT_DIR}/project-cr.yaml"
    {
        echo "apiVersion: hiclaw.io/v1beta1"
        echo "kind: Project"
        echo "metadata:"
        echo "  name: $(yaml_dq "${PROJECT_ID}")"
        echo "spec:"
        echo "  team: $(yaml_dq "${PROJECT_TEAM}")"
        echo "  description: $(yaml_dq "${PROJECT_TITLE}")"
        echo "  repos:"
        for _repo_spec in "${PROJECT_REPOS[@]}"; do
            _repo_url="${_repo_spec%:*}"
            _repo_access="${_repo_spec##*:}"
            echo "    - url: $(yaml_dq "${_repo_url}")"
            echo "      access: $(yaml_dq "${_repo_access}")"
        done
        if [ -n "${WORKERS_CSV}" ]; then
            echo "  workers:"
            echo "${WORKERS_CSV}" | tr ',' '\n' | while read -r w; do
                w=$(echo "${w}" | tr -d ' ')
                [ -z "${w}" ] && continue
                echo "    - $(yaml_dq "${w}")"
            done
        fi
    } > "${PROJECT_CR_FILE}"

    if hiclaw apply -f "${PROJECT_CR_FILE}"; then
        log "  Project CR applied (${PROJECT_CR_FILE})"
    else
        log "  WARNING: hiclaw apply -f failed for Project CR — repo/access provisioning layer not created; chat-flow project is unaffected"
    fi
fi

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
