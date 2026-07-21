#!/bin/bash
# create-team.sh - Create a Team (Leader + Workers + Team Room)
#
# Usage:
#   create-team.sh --name <TEAM_NAME> --leader <LEADER_NAME> --workers <w1,w2,...> \
#     [--leader-model <MODEL>] [--worker-models <m1,m2,...>]
#
# Prerequisites:
#   - SOUL.md must exist for leader and each worker at /root/agentteams-fs/agents/<NAME>/SOUL.md

set -e
source /opt/agentteams/scripts/lib/agentteams-env.sh
source /opt/agentteams/scripts/lib/gateway-api.sh

log() {
    local msg="[hiclaw $(date '+%Y-%m-%d %H:%M:%S')] $1"
    echo "${msg}"
    if [ -w /proc/1/fd/1 ]; then
        echo "${msg}" > /proc/1/fd/1
    fi
}

_fail() {
    echo '{"error": "'"$1"'"}'
    exit 1
}

# ============================================================
# Parse arguments
# ============================================================
TEAM_NAME=""
LEADER_NAME=""
WORKERS_CSV=""
LEADER_MODEL=""
WORKER_MODELS_CSV=""
WORKER_SKILLS_CSV=""
WORKER_MCP_SERVERS_CSV=""
TEAM_ADMIN=""
TEAM_ADMIN_MATRIX_ID=""
PEER_MENTIONS="true"            # default: team workers can @mention each other
TEAM_CHANNEL_POLICY_JSON=""     # team-level ChannelPolicySpec JSON
LEADER_CHANNEL_POLICY_JSON=""   # leader-specific ChannelPolicySpec JSON
WORKER_CHANNEL_POLICIES_CSV=""  # per-worker ChannelPolicySpec JSONs, ":" separated

while [ $# -gt 0 ]; do
    case "$1" in
        --name)           TEAM_NAME="$2"; shift 2 ;;
        --leader)         LEADER_NAME="$2"; shift 2 ;;
        --workers)        WORKERS_CSV="$2"; shift 2 ;;
        --leader-model)   LEADER_MODEL="$2"; shift 2 ;;
        --worker-models)  WORKER_MODELS_CSV="$2"; shift 2 ;;
        --worker-skills)  WORKER_SKILLS_CSV="$2"; shift 2 ;;
        --worker-mcp-servers) WORKER_MCP_SERVERS_CSV="$2"; shift 2 ;;
        --team-admin)     TEAM_ADMIN="$2"; shift 2 ;;
        --team-admin-matrix-id) TEAM_ADMIN_MATRIX_ID="$2"; shift 2 ;;
        --peer-mentions)  PEER_MENTIONS="$2"; shift 2 ;;
        --team-channel-policy) TEAM_CHANNEL_POLICY_JSON="$2"; shift 2 ;;
        --leader-channel-policy) LEADER_CHANNEL_POLICY_JSON="$2"; shift 2 ;;
        --worker-channel-policies) WORKER_CHANNEL_POLICIES_CSV="$2"; shift 2 ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

if [ -z "${TEAM_NAME}" ] || [ -z "${LEADER_NAME}" ] || [ -z "${WORKERS_CSV}" ]; then
    echo "Usage: create-team.sh --name <TEAM> --leader <LEADER> --workers <w1,w2,...> [--leader-model MODEL] [--worker-models m1,m2,...] [--team-admin NAME] [--team-admin-matrix-id @user:domain]"
    exit 1
fi

# Parse workers list
IFS=',' read -ra WORKER_NAMES <<< "${WORKERS_CSV}"
IFS=',' read -ra WORKER_MODELS <<< "${WORKER_MODELS_CSV:-}"
# Per-worker skills/mcpServers use : as separator between workers
IFS=':' read -ra WORKER_SKILLS_ARR <<< "${WORKER_SKILLS_CSV:-}"
IFS=':' read -ra WORKER_MCP_ARR <<< "${WORKER_MCP_SERVERS_CSV:-}"
# Per-worker comm policies use | as separator (: would break JSON)
IFS='|' read -ra WORKER_CHANNEL_POLICIES_ARR <<< "${WORKER_CHANNEL_POLICIES_CSV:-}"

MATRIX_DOMAIN="${AGENTTEAMS_MATRIX_DOMAIN:-matrix-local.agentteams.io:8080}"
ADMIN_USER="${AGENTTEAMS_ADMIN_USER:-admin}"

log "=== Creating Team: ${TEAM_NAME} ==="
log "  Leader: ${LEADER_NAME}"
log "  Workers: ${WORKERS_CSV}"
log "  Team Admin: ${TEAM_ADMIN:-none}"

# ============================================================
# Ensure credentials
# ============================================================
SECRETS_FILE="/data/agentteams-secrets.env"
if [ -f "${SECRETS_FILE}" ]; then
    source "${SECRETS_FILE}"
fi

if [ -z "${MANAGER_MATRIX_TOKEN:-}" ]; then
    MANAGER_PASSWORD="${AGENTTEAMS_MANAGER_PASSWORD:-}"
    if [ -z "${MANAGER_PASSWORD}" ]; then
        _fail "MANAGER_MATRIX_TOKEN not set and AGENTTEAMS_MANAGER_PASSWORD not available"
    fi
    MANAGER_MATRIX_TOKEN=$(curl -sf -X POST ${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/login \
        -H 'Content-Type: application/json' \
        -d '{"type":"m.login.password","identifier":{"type":"m.id.user","user":"manager"},"password":"'"${MANAGER_PASSWORD}"'"}' \
        2>/dev/null | jq -r '.access_token // empty')
    if [ -z "${MANAGER_MATRIX_TOKEN}" ]; then
        _fail "Failed to obtain Manager Matrix token"
    fi
fi

# Obtain Team Admin's Matrix token so we can auto-join rooms on their behalf.
# This only works when Team Admin is the Global Admin (we have the password).
TEAM_ADMIN_TOKEN=""
_obtain_team_admin_token() {
    local _admin_name="${1:-${ADMIN_USER}}"
    if [ "${_admin_name}" = "${ADMIN_USER}" ] && [ -n "${AGENTTEAMS_ADMIN_PASSWORD:-}" ]; then
        TEAM_ADMIN_TOKEN=$(curl -sf -X POST ${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/login \
            -H 'Content-Type: application/json' \
            -d '{"type":"m.login.password","identifier":{"type":"m.id.user","user":"'"${_admin_name}"'"},"password":"'"${AGENTTEAMS_ADMIN_PASSWORD}"'"}' \
            2>/dev/null | jq -r '.access_token // empty')
        if [ -n "${TEAM_ADMIN_TOKEN}" ]; then
            log "  Obtained Team Admin token for auto-join"
        else
            log "  WARNING: Failed to obtain Team Admin token — admin will need to accept invites manually"
        fi
    else
        log "  WARNING: Custom Team Admin (${_admin_name}) — cannot auto-join, admin will need to accept invites manually"
    fi
}

# Auto-join a room on behalf of Team Admin
_admin_auto_join() {
    local _room_id="$1"
    if [ -z "${TEAM_ADMIN_TOKEN}" ] || [ -z "${_room_id}" ]; then
        return 0
    fi
    local _room_enc
    _room_enc=$(echo "${_room_id}" | sed 's/!/%21/g')
    if curl -sf -X POST "${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/rooms/${_room_enc}/join" \
        -H "Authorization: Bearer ${TEAM_ADMIN_TOKEN}" \
        -H 'Content-Type: application/json' \
        -d '{}' > /dev/null 2>&1; then
        log "  Team Admin auto-joined room ${_room_id}"
    else
        log "  WARNING: Team Admin failed to auto-join room ${_room_id}"
    fi
}

# ============================================================
# Resolve Team Admin Matrix ID (before creating workers so they get it)
# ============================================================
TEAM_ADMIN_MID=""
if [ -n "${TEAM_ADMIN}" ]; then
    if [ -n "${TEAM_ADMIN_MATRIX_ID}" ]; then
        TEAM_ADMIN_MID="${TEAM_ADMIN_MATRIX_ID}"
    else
        TEAM_ADMIN_MID="@${TEAM_ADMIN}:${MATRIX_DOMAIN}"
    fi
else
    # Default: use Global Admin as Team Admin
    TEAM_ADMIN="${ADMIN_USER}"
    TEAM_ADMIN_MID="@${ADMIN_USER}:${MATRIX_DOMAIN}"
    log "  No --team-admin specified, defaulting to Global Admin (${ADMIN_USER})"
fi

_obtain_team_admin_token "${TEAM_ADMIN}"

# ============================================================
# Step 1: Create Team Room and Leader DM (empty — members invited later)
# Rooms are created first so room IDs can be passed to create-worker.sh,
# ensuring Leader's AGENTS.md has full team-context from the start.
# ============================================================
log "Step 1: Creating Team Room and Leader DM..."
MANAGER_MATRIX_ID="@manager:${MATRIX_DOMAIN}"
ADMIN_MATRIX_ID="@${ADMIN_USER}:${MATRIX_DOMAIN}"
LEADER_MATRIX_ID="@${LEADER_NAME}:${MATRIX_DOMAIN}"

# E2EE
ROOM_E2EE_INITIAL_STATE=""
if [ "${AGENTTEAMS_MATRIX_E2EE:-0}" = "1" ] || [ "${AGENTTEAMS_MATRIX_E2EE:-}" = "true" ]; then
    ROOM_E2EE_INITIAL_STATE=',"initial_state":[{"type":"m.room.encryption","state_key":"","content":{"algorithm":"m.megolm.v1.aes-sha2"}}]'
fi

# Create Team Room (Manager-only, members invited in Step 4)
TEAM_ROOM_RESP=$(curl -sf -X POST ${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/createRoom \
    -H "Authorization: Bearer ${MANAGER_MATRIX_TOKEN}" \
    -H 'Content-Type: application/json' \
    -d '{
        "name": "Team: '"${TEAM_NAME}"'",
        "topic": "Team room for '"${TEAM_NAME}"' — Leader + Workers coordination",
        "preset": "trusted_private_chat",
        "power_level_content_override": {
            "users": {
                "'"${MANAGER_MATRIX_ID}"'": 100
            }
        }'"${ROOM_E2EE_INITIAL_STATE}"'
    }' 2>/dev/null) || _fail "Failed to create Team Room"

TEAM_ROOM_ID=$(echo "${TEAM_ROOM_RESP}" | jq -r '.room_id // empty')
if [ -z "${TEAM_ROOM_ID}" ]; then
    _fail "Failed to create Team Room: ${TEAM_ROOM_RESP}"
fi
log "  Team Room created: ${TEAM_ROOM_ID}"

# Create Leader DM (Manager-only, members invited in Step 4)
LEADER_DM_ROOM_ID=""
if [ -n "${TEAM_ADMIN_MID}" ]; then
    LEADER_DM_RESP=$(curl -sf -X POST ${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/createRoom \
        -H "Authorization: Bearer ${MANAGER_MATRIX_TOKEN}" \
        -H 'Content-Type: application/json' \
        -d '{
            "name": "Team Admin DM: '"${TEAM_NAME}"'",
            "topic": "Direct channel between Team Admin and Leader of '"${TEAM_NAME}"'",
            "preset": "trusted_private_chat",
            "power_level_content_override": {
                "users": {
                    "'"${MANAGER_MATRIX_ID}"'": 100
                }
            }'"${ROOM_E2EE_INITIAL_STATE}"'
        }' 2>/dev/null) || log "  WARNING: Failed to create Leader DM room"

    LEADER_DM_ROOM_ID=$(echo "${LEADER_DM_RESP}" | jq -r '.room_id // empty')
    if [ -n "${LEADER_DM_ROOM_ID}" ]; then
        log "  Leader DM room created: ${LEADER_DM_ROOM_ID}"
    else
        log "  WARNING: Could not extract Leader DM room_id"
    fi
else
    log "  No Team Admin specified, skipping Leader DM room"
fi

# ============================================================
# Step 2: Create Team Leader (with room IDs for AGENTS.md team-context)
# ============================================================
log "Step 2: Creating Team Leader (${LEADER_NAME})..."
LEADER_ARGS=(--name "${LEADER_NAME}" --role team_leader --team "${TEAM_NAME}" --runtime copaw)
if [ -n "${LEADER_MODEL}" ]; then
    LEADER_ARGS+=(--model "${LEADER_MODEL}")
fi
if [ -n "${TEAM_ADMIN_MID}" ]; then
    LEADER_ARGS+=(--team-admin-matrix-id "${TEAM_ADMIN_MID}")
fi
# Pass pre-created room IDs so Leader's AGENTS.md gets full team-context
if [ -n "${TEAM_ROOM_ID}" ]; then
    LEADER_ARGS+=(--team-room-id "${TEAM_ROOM_ID}")
fi
if [ -n "${LEADER_DM_ROOM_ID}" ]; then
    LEADER_ARGS+=(--leader-dm-room-id "${LEADER_DM_ROOM_ID}")
fi
# Build channel policy: include all workers + Team Admin in groupAllowExtra
# so Leader's groupAllowFrom is correct from the start (no post-hoc patching)
LEADER_GROUP_ALLOW_EXTRA="[]"
for w_name in "${WORKER_NAMES[@]}"; do
    w_name=$(echo "${w_name}" | tr -d ' ')
    [ -z "${w_name}" ] && continue
    LEADER_GROUP_ALLOW_EXTRA=$(echo "${LEADER_GROUP_ALLOW_EXTRA}" | jq --arg w "${w_name}" '. += [$w]')
done
if [ -n "${TEAM_ADMIN_MID}" ]; then
    LEADER_GROUP_ALLOW_EXTRA=$(echo "${LEADER_GROUP_ALLOW_EXTRA}" | jq --arg a "${TEAM_ADMIN_MID}" '. += [$a]')
fi
# Merge with team-level + leader-specific comm policy
LEADER_MERGED_POLICY=$(jq -n \
    --argjson team "${TEAM_CHANNEL_POLICY_JSON:-null}" \
    --argjson member "${LEADER_CHANNEL_POLICY_JSON:-null}" \
    --argjson workers "${LEADER_GROUP_ALLOW_EXTRA}" \
    '{
        groupAllowExtra: ((($team.groupAllowExtra // []) + ($member.groupAllowExtra // []) + $workers) | unique),
        groupDenyExtra:  ((($team.groupDenyExtra // [])  + ($member.groupDenyExtra // []))  | unique),
        dmAllowExtra:    ((($team.dmAllowExtra // [])    + ($member.dmAllowExtra // []))    | unique),
        dmDenyExtra:     ((($team.dmDenyExtra // [])     + ($member.dmDenyExtra // []))     | unique)
    } | with_entries(select(.value | length > 0))')
if [ -n "${LEADER_MERGED_POLICY}" ] && [ "${LEADER_MERGED_POLICY}" != "{}" ]; then
    # Add Team Admin to dm.allowFrom
    if [ -n "${TEAM_ADMIN_MID}" ]; then
        LEADER_MERGED_POLICY=$(echo "${LEADER_MERGED_POLICY}" | jq --arg a "${TEAM_ADMIN_MID}" \
            '.dmAllowExtra = ((.dmAllowExtra // []) + [$a] | unique)')
    fi
    LEADER_ARGS+=(--channel-policy "${LEADER_MERGED_POLICY}")
fi

log "  Leader channel-policy: ${LEADER_MERGED_POLICY:-none}"
LEADER_RESULT=$(bash /opt/agentteams/agent/skills/worker-management/scripts/create-worker.sh "${LEADER_ARGS[@]}" 2>&1)
LEADER_JSON=$(echo "${LEADER_RESULT}" | sed -n '/---RESULT---/,$ p' | tail -n +2)
LEADER_ROOM_ID=$(echo "${LEADER_JSON}" | jq -r '.room_id // empty')

if [ -z "${LEADER_ROOM_ID}" ]; then
    log "  WARNING: Could not extract leader room_id from result"
    log "  Result: ${LEADER_RESULT}"
fi
log "  Leader created: room=${LEADER_ROOM_ID}"

# ============================================================
# Step 3: Create Team Workers
# ============================================================
log "Step 3: Creating team workers..."
WORKER_ROOM_IDS=()

for i in "${!WORKER_NAMES[@]}"; do
    w_name=$(echo "${WORKER_NAMES[$i]}" | tr -d ' ')
    [ -z "${w_name}" ] && continue

    w_model="${WORKER_MODELS[$i]:-}"
    w_skills="${WORKER_SKILLS_ARR[$i]:-}"
    w_mcp="${WORKER_MCP_ARR[$i]:-}"
    log "  Creating worker: ${w_name}..."

    W_ARGS=(--name "${w_name}" --role worker --team "${TEAM_NAME}" --team-leader "${LEADER_NAME}" --runtime copaw)
    if [ -n "${w_model}" ]; then
        W_ARGS+=(--model "${w_model}")
    fi
    if [ -n "${w_skills}" ]; then
        W_ARGS+=(--skills "${w_skills}")
    fi
    if [ -n "${w_mcp}" ]; then
        W_ARGS+=(--mcp-servers "${w_mcp}")
    fi
    if [ -n "${TEAM_ADMIN_MID}" ]; then
        W_ARGS+=(--team-admin-matrix-id "${TEAM_ADMIN_MID}")
    fi
    # Build channel policy: include Team Admin + peer workers in groupAllowExtra
    # so worker's groupAllowFrom is correct from the start
    W_GROUP_ALLOW_EXTRA="[]"
    # Add Team Admin
    if [ -n "${TEAM_ADMIN_MID}" ]; then
        W_GROUP_ALLOW_EXTRA=$(echo "${W_GROUP_ALLOW_EXTRA}" | jq --arg a "${TEAM_ADMIN_MID}" '. += [$a]')
    fi
    # Add peer workers (if peer mentions enabled)
    if [ "${PEER_MENTIONS}" = "true" ]; then
        for j in "${!WORKER_NAMES[@]}"; do
            peer=$(echo "${WORKER_NAMES[$j]}" | tr -d ' ')
            [ -z "${peer}" ] && continue
            [ "${peer}" = "${w_name}" ] && continue
            W_GROUP_ALLOW_EXTRA=$(echo "${W_GROUP_ALLOW_EXTRA}" | jq --arg w "${peer}" '. += [$w]')
        done
    fi
    # Merge with team-level + per-worker comm policy
    w_channel_policy="${WORKER_CHANNEL_POLICIES_ARR[$i]:-}"
    W_MERGED_POLICY=$(jq -n \
        --argjson team "${TEAM_CHANNEL_POLICY_JSON:-null}" \
        --argjson member "${w_channel_policy:-null}" \
        --argjson extra "${W_GROUP_ALLOW_EXTRA}" \
        '{
            groupAllowExtra: ((($team.groupAllowExtra // []) + ($member.groupAllowExtra // []) + $extra) | unique),
            groupDenyExtra:  ((($team.groupDenyExtra // [])  + ($member.groupDenyExtra // []))  | unique),
            dmAllowExtra:    ((($team.dmAllowExtra // [])    + ($member.dmAllowExtra // []))    | unique),
            dmDenyExtra:     ((($team.dmDenyExtra // [])     + ($member.dmDenyExtra // []))     | unique)
        } | with_entries(select(.value | length > 0))')
    if [ -n "${W_MERGED_POLICY}" ] && [ "${W_MERGED_POLICY}" != "{}" ]; then
        W_ARGS+=(--channel-policy "${W_MERGED_POLICY}")
    fi

    W_RESULT=$(bash /opt/agentteams/agent/skills/worker-management/scripts/create-worker.sh "${W_ARGS[@]}" 2>&1)
    W_JSON=$(echo "${W_RESULT}" | sed -n '/---RESULT---/,$ p' | tail -n +2)
    W_ROOM_ID=$(echo "${W_JSON}" | jq -r '.room_id // empty')
    WORKER_ROOM_IDS+=("${W_ROOM_ID}")
    log "  Worker ${w_name} created: room=${W_ROOM_ID}"
done

# ============================================================
# Step 4: Invite members into Team Room + Leader DM, then Manager leaves
# Rooms were created empty in Step 1. Now that all workers exist, invite them.
# ============================================================
log "Step 4: Inviting members into Team Room and Leader DM..."

# Set power levels for Team Room: Leader=100, Team Admin=100, Workers=0
POWER_USERS_JSON=$(jq -n --arg leader "${LEADER_MATRIX_ID}" '{($leader): 100}')
if [ -n "${TEAM_ADMIN_MID}" ]; then
    POWER_USERS_JSON=$(echo "${POWER_USERS_JSON}" | jq --arg a "${TEAM_ADMIN_MID}" '. + {($a): 100}')
fi
for w_name in "${WORKER_NAMES[@]}"; do
    w_name=$(echo "${w_name}" | tr -d ' ')
    [ -z "${w_name}" ] && continue
    POWER_USERS_JSON=$(echo "${POWER_USERS_JSON}" | jq --arg w "@${w_name}:${MATRIX_DOMAIN}" '. + {($w): 0}')
done
# Keep Manager at 100 (creator) — will leave after invites
POWER_USERS_JSON=$(echo "${POWER_USERS_JSON}" | jq --arg m "${MANAGER_MATRIX_ID}" '. + {($m): 100}')

# Update Team Room power levels
TEAM_ROOM_ENC=$(echo "${TEAM_ROOM_ID}" | sed 's/!/%21/g')
curl -sf -X PUT "${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/rooms/${TEAM_ROOM_ENC}/state/m.room.power_levels/" \
    -H "Authorization: Bearer ${MANAGER_MATRIX_TOKEN}" \
    -H 'Content-Type: application/json' \
    -d '{"users": '"${POWER_USERS_JSON}"'}' > /dev/null 2>&1 \
    || log "  WARNING: Failed to update Team Room power levels"

# Invite Leader + Team Admin + Workers into Team Room
for _invite_id in "${LEADER_MATRIX_ID}" ${TEAM_ADMIN_MID:+"${TEAM_ADMIN_MID}"}; do
    curl -sf -X POST "${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/rooms/${TEAM_ROOM_ENC}/invite" \
        -H "Authorization: Bearer ${MANAGER_MATRIX_TOKEN}" \
        -H 'Content-Type: application/json' \
        -d '{"user_id": "'"${_invite_id}"'"}' > /dev/null 2>&1 || true
done
for w_name in "${WORKER_NAMES[@]}"; do
    w_name=$(echo "${w_name}" | tr -d ' ')
    [ -z "${w_name}" ] && continue
    curl -sf -X POST "${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/rooms/${TEAM_ROOM_ENC}/invite" \
        -H "Authorization: Bearer ${MANAGER_MATRIX_TOKEN}" \
        -H 'Content-Type: application/json' \
        -d '{"user_id": "@'"${w_name}"':'"${MATRIX_DOMAIN}"'"}' > /dev/null 2>&1 || true
done
log "  Members invited to Team Room"

# Manager leaves Team Room (delegation boundary)
curl -sf -X POST "${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/rooms/${TEAM_ROOM_ENC}/leave" \
    -H "Authorization: Bearer ${MANAGER_MATRIX_TOKEN}" \
    -H 'Content-Type: application/json' -d '{}' > /dev/null 2>&1 \
    && log "  Manager left Team Room (delegation boundary)" \
    || log "  WARNING: Manager failed to leave Team Room"

# Auto-join Team Admin into Team Room
_admin_auto_join "${TEAM_ROOM_ID}"

# Invite Leader + Team Admin into Leader DM, then Manager leaves
if [ -n "${LEADER_DM_ROOM_ID}" ]; then
    LEADER_DM_ENC=$(echo "${LEADER_DM_ROOM_ID}" | sed 's/!/%21/g')
    # Update power levels
    curl -sf -X PUT "${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/rooms/${LEADER_DM_ENC}/state/m.room.power_levels/" \
        -H "Authorization: Bearer ${MANAGER_MATRIX_TOKEN}" \
        -H 'Content-Type: application/json' \
        -d '{"users": {"'"${MANAGER_MATRIX_ID}"'": 100, "'"${TEAM_ADMIN_MID}"'": 100, "'"${LEADER_MATRIX_ID}"'": 0}}' > /dev/null 2>&1 || true
    # Invite
    curl -sf -X POST "${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/rooms/${LEADER_DM_ENC}/invite" \
        -H "Authorization: Bearer ${MANAGER_MATRIX_TOKEN}" \
        -H 'Content-Type: application/json' \
        -d '{"user_id": "'"${TEAM_ADMIN_MID}"'"}' > /dev/null 2>&1 || true
    curl -sf -X POST "${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/rooms/${LEADER_DM_ENC}/invite" \
        -H "Authorization: Bearer ${MANAGER_MATRIX_TOKEN}" \
        -H 'Content-Type: application/json' \
        -d '{"user_id": "'"${LEADER_MATRIX_ID}"'"}' > /dev/null 2>&1 || true
    log "  Members invited to Leader DM"
    # Manager leaves
    curl -sf -X POST "${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/rooms/${LEADER_DM_ENC}/leave" \
        -H "Authorization: Bearer ${MANAGER_MATRIX_TOKEN}" \
        -H 'Content-Type: application/json' -d '{}' > /dev/null 2>&1 \
        && log "  Manager left Leader DM" \
        || log "  WARNING: Manager failed to leave Leader DM"
    _admin_auto_join "${LEADER_DM_ROOM_ID}"
fi

# ============================================================
# Step 5: Initialize team storage space in MinIO
# Each team gets an isolated storage prefix: teams/{team-name}/
# ============================================================
log "Step 5: Initializing team storage space..."
TEAM_STORAGE_DIR="/root/agentteams-fs/teams/${TEAM_NAME}"
mkdir -p "${TEAM_STORAGE_DIR}/shared/tasks"
mkdir -p "${TEAM_STORAGE_DIR}/shared/projects"
mkdir -p "${TEAM_STORAGE_DIR}/shared/knowledge"
touch "${TEAM_STORAGE_DIR}/shared/tasks/.keep"
touch "${TEAM_STORAGE_DIR}/shared/projects/.keep"
touch "${TEAM_STORAGE_DIR}/shared/knowledge/.keep"
ensure_mc_credentials 2>/dev/null || true
mc mirror "${TEAM_STORAGE_DIR}/" "${AGENTTEAMS_STORAGE_PREFIX}/teams/${TEAM_NAME}/" --overwrite 2>&1 | tail -3
log "  Team storage initialized at ${AGENTTEAMS_STORAGE_PREFIX}/teams/${TEAM_NAME}/"

# ============================================================
# Step 6: Update teams-registry.json
# ============================================================
log "Step 6: Updating teams-registry.json..."
REGISTRY_ARGS=(
    --action add
    --team-name "${TEAM_NAME}"
    --leader "${LEADER_NAME}"
    --workers "${WORKERS_CSV}"
    --team-room-id "${TEAM_ROOM_ID}"
)
if [ -n "${TEAM_ADMIN}" ]; then
    REGISTRY_ARGS+=(--team-admin "${TEAM_ADMIN}")
fi
if [ -n "${TEAM_ADMIN_MID}" ]; then
    REGISTRY_ARGS+=(--team-admin-matrix-id "${TEAM_ADMIN_MID}")
fi
if [ -n "${LEADER_DM_ROOM_ID}" ]; then
    REGISTRY_ARGS+=(--leader-dm-room-id "${LEADER_DM_ROOM_ID}")
fi
bash /opt/agentteams/agent/skills/team-management/scripts/manage-teams-registry.sh "${REGISTRY_ARGS[@]}"

# ============================================================
# Step 6b: Re-inject Leader's team-context with worker room IDs
# Leader was created before workers, so its AGENTS.md team-context
# is missing worker room IDs. Now that all workers are registered,
# re-inject the full context.
# ============================================================
log "Step 6b: Re-injecting Leader team-context with worker room IDs..."
_leader_agents_minio="${AGENTTEAMS_STORAGE_PREFIX}/agents/${LEADER_NAME}/AGENTS.md"
_leader_agents_tmp=$(mktemp /tmp/leader-agents-XXXXXX.md)
_leader_ctx_tmp=$(mktemp /tmp/leader-ctx-XXXXXX.md)

# Build worker list with room IDs
_worker_lines=""
for i in "${!WORKER_NAMES[@]}"; do
    _wn=$(echo "${WORKER_NAMES[$i]}" | tr -d ' ')
    [ -z "${_wn}" ] && continue
    _wr="${WORKER_ROOM_IDS[$i]:-unknown}"
    _worker_lines="${_worker_lines}
  - @${_wn}:${MATRIX_DOMAIN} — Room: ${_wr}"
done

{
    echo ""
    echo "<!-- hiclaw-team-context-start -->"
    echo "## Coordination"
    echo ""
    echo "- **Upstream coordinator**: @manager:${MATRIX_DOMAIN} (Manager) — you receive tasks from Manager"
    [ -n "${TEAM_ADMIN_MID}" ] && echo "- **Team Admin**: ${TEAM_ADMIN_MID} — can assign tasks and make decisions within the team"
    echo "- **Team**: ${TEAM_NAME}"
    echo "- **Team Room**: ${TEAM_ROOM_ID} — @mention workers here for task assignment"
    [ -n "${LEADER_DM_ROOM_ID}" ] && echo "- **Leader DM**: ${LEADER_DM_ROOM_ID} — Team Admin communicates with you here"
    echo "- **Team Workers**:${_worker_lines}"
    echo "- You decompose tasks from Manager or Team Admin and assign sub-tasks to your team workers"
    echo "- @mention workers in the Team Room for task assignment"
    echo "- This Coordination block is already loaded into your system prompt; use these room IDs and worker Matrix IDs directly, without narrating topology checks or AGENTS.md reads"
    echo "- Report results to Manager (in Leader Room) or Team Admin (in Leader DM) based on task source"
    echo "- @mention Manager only for: task completion, blockers, escalations"
    echo "<!-- hiclaw-team-context-end -->"
} > "${_leader_ctx_tmp}"

if mc cp "${_leader_agents_minio}" "${_leader_agents_tmp}" 2>/dev/null; then
    _leader_clean=$(mktemp /tmp/leader-clean-XXXXXX.md)
    awk '/<!-- hiclaw-team-context-start -->/{skip=1; next} /<!-- hiclaw-team-context-end -->/{skip=0; next} !skip' \
        "${_leader_agents_tmp}" > "${_leader_clean}"

    _leader_final=$(mktemp /tmp/leader-final-XXXXXX.md)
    if grep -q '^<!-- hiclaw-builtin-end -->' "${_leader_clean}"; then
        awk -v ctx_file="${_leader_ctx_tmp}" '
            {print}
            /^<!-- hiclaw-builtin-end -->$/ {
                while ((getline line < ctx_file) > 0) print line
                close(ctx_file)
            }
        ' "${_leader_clean}" > "${_leader_final}"
    else
        cat "${_leader_clean}" "${_leader_ctx_tmp}" > "${_leader_final}"
    fi

    mc cp "${_leader_final}" "${_leader_agents_minio}" 2>/dev/null \
        && log "  Leader team-context updated with room IDs in MinIO" \
        || log "  WARNING: Failed to update Leader team-context in MinIO"

    # Also push directly into the running Leader container (FileSync won't pull AGENTS.md)
    LEADER_CONTAINER="agentteams-worker-${LEADER_NAME}"
    LEADER_COPAW_DIR="/root/.copaw-worker/${LEADER_NAME}"
    if docker exec "${LEADER_CONTAINER}" true 2>/dev/null; then
        docker cp "${_leader_final}" "${LEADER_CONTAINER}:${LEADER_COPAW_DIR}/AGENTS.md" 2>/dev/null \
            && log "  Leader AGENTS.md pushed to container working dir" \
            || log "  WARNING: Failed to push AGENTS.md to Leader container"
        docker cp "${_leader_final}" "${LEADER_CONTAINER}:${LEADER_COPAW_DIR}/.copaw/AGENTS.md" 2>/dev/null \
            || true  # .copaw/ copy is best-effort
    else
        log "  WARNING: Leader container not running, skipping direct push"
    fi

    rm -f "${_leader_clean}" "${_leader_final}"
fi
rm -f "${_leader_agents_tmp}" "${_leader_ctx_tmp}"

# ============================================================
# Step 7: Backfill permissions for humans that reference this team
# If a Human was created before this team, their permissions were
# skipped. Now that the team exists, configure them.
# ============================================================
HUMANS_REGISTRY="${HOME}/humans-registry.json"
if [ -f "${HUMANS_REGISTRY}" ]; then
    PENDING_HUMANS=$(jq -r --arg t "${TEAM_NAME}" \
        '.humans | to_entries[] | select(.value.accessible_teams // [] | index($t)) | .key' \
        "${HUMANS_REGISTRY}" 2>/dev/null)

    if [ -n "${PENDING_HUMANS}" ]; then
        log "Step 7: Backfilling permissions for humans referencing ${TEAM_NAME}..."
        ensure_mc_credentials 2>/dev/null || true

        for _human_name in ${PENDING_HUMANS}; do
            _human_mid=$(jq -r --arg h "${_human_name}" '.humans[$h].matrix_user_id // empty' "${HUMANS_REGISTRY}" 2>/dev/null)
            [ -z "${_human_mid}" ] && continue
            log "  Configuring permissions for human: ${_human_name} (${_human_mid})"

            # Add human to Leader's groupAllowFrom
            if [ -f "${LEADER_CONFIG}" ]; then
                jq --arg h "${_human_mid}" \
                    'if (.channels.matrix.groupAllowFrom | index($h)) then .
                     else .channels.matrix.groupAllowFrom += [$h]
                     end' \
                    "${LEADER_CONFIG}" > /tmp/leader-human-tmp.json
                mv /tmp/leader-human-tmp.json "${LEADER_CONFIG}"
                mc cp "${LEADER_CONFIG}" "${AGENTTEAMS_STORAGE_PREFIX}/agents/${LEADER_NAME}/openclaw.json" 2>/dev/null || true
            fi

            # Add human to each Worker's groupAllowFrom
            for w_name in "${WORKER_NAMES[@]}"; do
                w_name=$(echo "${w_name}" | tr -d ' ')
                [ -z "${w_name}" ] && continue
                W_CONFIG="/root/agentteams-fs/agents/${w_name}/openclaw.json"
                if [ -f "${W_CONFIG}" ]; then
                    jq --arg h "${_human_mid}" \
                        'if (.channels.matrix.groupAllowFrom | index($h)) then .
                         else .channels.matrix.groupAllowFrom += [$h]
                         end' \
                        "${W_CONFIG}" > /tmp/worker-human-tmp.json
                    mv /tmp/worker-human-tmp.json "${W_CONFIG}"
                    mc cp "${W_CONFIG}" "${AGENTTEAMS_STORAGE_PREFIX}/agents/${w_name}/openclaw.json" 2>/dev/null || true
                fi
            done

            # Invite human to Team Room
            if [ -n "${TEAM_ROOM_ID}" ]; then
                ROOM_ENC=$(echo "${TEAM_ROOM_ID}" | sed 's/!/%21/g')
                curl -sf -X POST "${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/rooms/${ROOM_ENC}/invite" \
                    -H "Authorization: Bearer ${MANAGER_MATRIX_TOKEN}" \
                    -H 'Content-Type: application/json' \
                    -d '{"user_id": "'"${_human_mid}"'"}' 2>/dev/null || true
            fi

            log "    Permissions configured for ${_human_name}"
        done
    else
        log "Step 7: No pending humans for ${TEAM_NAME} (skipped)"
    fi
else
    log "Step 7: No humans-registry.json found (skipped)"
fi

# ============================================================
# Output JSON result
# ============================================================
WORKERS_JSON="[]"
for i in "${!WORKER_NAMES[@]}"; do
    w_name=$(echo "${WORKER_NAMES[$i]}" | tr -d ' ')
    [ -z "${w_name}" ] && continue
    w_room="${WORKER_ROOM_IDS[$i]:-}"
    WORKERS_JSON=$(echo "${WORKERS_JSON}" | jq --arg n "${w_name}" --arg r "${w_room}" '. += [{name: $n, room_id: $r}]')
done

RESULT=$(jq -n \
    --arg team "${TEAM_NAME}" \
    --arg leader "${LEADER_NAME}" \
    --arg leader_room "${LEADER_ROOM_ID}" \
    --arg team_room "${TEAM_ROOM_ID}" \
    --arg leader_dm_room "${LEADER_DM_ROOM_ID:-}" \
    --arg team_admin "${TEAM_ADMIN:-}" \
    --arg team_admin_mid "${TEAM_ADMIN_MID:-}" \
    --argjson workers "${WORKERS_JSON}" \
    '{
        team_name: $team,
        leader: $leader,
        leader_room_id: $leader_room,
        team_room_id: $team_room,
        leader_dm_room_id: (if $leader_dm_room == "" then null else $leader_dm_room end),
        team_admin: (if $team_admin == "" then null else $team_admin end),
        team_admin_matrix_id: (if $team_admin_mid == "" then null else $team_admin_mid end),
        workers: $workers
    }')

echo "---RESULT---"
echo "${RESULT}"
