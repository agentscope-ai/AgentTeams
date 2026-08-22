#!/bin/bash
# create-team.sh - Create a Team (Leader + Workers + Team Room)
#
# Usage:
#   create-team.sh --name <TEAM_NAME> --leader <LEADER_NAME> --workers <w1,w2,...> \
#     [--leader-model <MODEL>] [--worker-models <m1,m2,...>]
#
# Prerequisites:
#   - SOUL.md must exist for leader and each worker at /root/hiclaw-fs/agents/<NAME>/SOUL.md

set -e
source /opt/hiclaw/scripts/lib/hiclaw-env.sh
source /opt/hiclaw/scripts/lib/gateway-api.sh

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

MATRIX_DOMAIN="${HICLAW_MATRIX_DOMAIN:-matrix-local.hiclaw.io:8080}"
ADMIN_USER="${HICLAW_ADMIN_USER:-admin}"

log "=== Creating Team: ${TEAM_NAME} ==="
log "  Leader: ${LEADER_NAME}"
log "  Workers: ${WORKERS_CSV}"
log "  Team Admin: ${TEAM_ADMIN:-none}"

# ============================================================
# Ensure credentials
# ============================================================
SECRETS_FILE="/data/hiclaw-secrets.env"
if [ -f "${SECRETS_FILE}" ]; then
    source "${SECRETS_FILE}"
fi

if [ -z "${MANAGER_MATRIX_TOKEN:-}" ]; then
    MANAGER_PASSWORD="${HICLAW_MANAGER_PASSWORD:-}"
    if [ -z "${MANAGER_PASSWORD}" ]; then
        _fail "MANAGER_MATRIX_TOKEN not set and HICLAW_MANAGER_PASSWORD not available"
    fi
    MANAGER_MATRIX_TOKEN=$(curl -sf -X POST ${HICLAW_MATRIX_SERVER}/_matrix/client/v3/login \
        -H 'Content-Type: application/json' \
        -d '{"type":"m.login.password","identifier":{"type":"m.id.user","user":"manager"},"password":"'"${MANAGER_PASSWORD}"'"}' \
        2>/dev/null | jq -r '.access_token // empty')
    if [ -z "${MANAGER_MATRIX_TOKEN}" ]; then
        _fail "Failed to obtain Manager Matrix token"
    fi
fi

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

# ============================================================
# Step 1: Create Team Leader
# ============================================================
log "Step 1: Creating Team Leader (${LEADER_NAME})..."
LEADER_ARGS=(--name "${LEADER_NAME}" --role team_leader --team "${TEAM_NAME}")
if [ -n "${LEADER_MODEL}" ]; then
    LEADER_ARGS+=(--model "${LEADER_MODEL}")
fi
if [ -n "${TEAM_ADMIN_MID}" ]; then
    LEADER_ARGS+=(--team-admin-matrix-id "${TEAM_ADMIN_MID}")
fi

LEADER_RESULT=$(bash /opt/hiclaw/agent/skills/worker-management/scripts/create-worker.sh "${LEADER_ARGS[@]}" 2>&1)
LEADER_JSON=$(echo "${LEADER_RESULT}" | sed -n '/---RESULT---/,$ p' | tail -n +2)
LEADER_ROOM_ID=$(echo "${LEADER_JSON}" | jq -r '.room_id // empty')

if [ -z "${LEADER_ROOM_ID}" ]; then
    log "  WARNING: Could not extract leader room_id from result"
    log "  Result: ${LEADER_RESULT}"
fi
log "  Leader created: room=${LEADER_ROOM_ID}"

# ============================================================
# Step 2: Create Team Workers
# ============================================================
log "Step 2: Creating team workers..."
WORKER_ROOM_IDS=()

for i in "${!WORKER_NAMES[@]}"; do
    w_name=$(echo "${WORKER_NAMES[$i]}" | tr -d ' ')
    [ -z "${w_name}" ] && continue

    w_model="${WORKER_MODELS[$i]:-}"
    w_skills="${WORKER_SKILLS_ARR[$i]:-}"
    w_mcp="${WORKER_MCP_ARR[$i]:-}"
    log "  Creating worker: ${w_name}..."

    W_ARGS=(--name "${w_name}" --role worker --team "${TEAM_NAME}" --team-leader "${LEADER_NAME}")
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

    W_RESULT=$(bash /opt/hiclaw/agent/skills/worker-management/scripts/create-worker.sh "${W_ARGS[@]}" 2>&1)
    W_JSON=$(echo "${W_RESULT}" | sed -n '/---RESULT---/,$ p' | tail -n +2)
    W_ROOM_ID=$(echo "${W_JSON}" | jq -r '.room_id // empty')
    WORKER_ROOM_IDS+=("${W_ROOM_ID}")
    log "  Worker ${w_name} created: room=${W_ROOM_ID}"
done

# ============================================================
# Step 3: Create Team Room (Leader + Team Admin + all Workers)
# No Global Admin, no Manager — Team Admin is the authority here
# ============================================================
log "Step 3: Creating Team Room..."
MANAGER_MATRIX_ID="@manager:${MATRIX_DOMAIN}"
ADMIN_MATRIX_ID="@${ADMIN_USER}:${MATRIX_DOMAIN}"
LEADER_MATRIX_ID="@${LEADER_NAME}:${MATRIX_DOMAIN}"

# Build invite list: Leader + Team Admin (if set) + all workers
INVITE_LIST="\"${LEADER_MATRIX_ID}\""
if [ -n "${TEAM_ADMIN_MID}" ]; then
    INVITE_LIST="${INVITE_LIST},\"${TEAM_ADMIN_MID}\""
fi
for w_name in "${WORKER_NAMES[@]}"; do
    w_name=$(echo "${w_name}" | tr -d ' ')
    [ -z "${w_name}" ] && continue
    INVITE_LIST="${INVITE_LIST},\"@${w_name}:${MATRIX_DOMAIN}\""
done

# Build power levels: Leader=100, Team Admin=100 (if set), Workers=0
POWER_USERS="\"${MANAGER_MATRIX_ID}\": 100, \"${LEADER_MATRIX_ID}\": 100"
if [ -n "${TEAM_ADMIN_MID}" ]; then
    POWER_USERS="${POWER_USERS}, \"${TEAM_ADMIN_MID}\": 100"
fi
for w_name in "${WORKER_NAMES[@]}"; do
    w_name=$(echo "${w_name}" | tr -d ' ')
    [ -z "${w_name}" ] && continue
    POWER_USERS="${POWER_USERS}, \"@${w_name}:${MATRIX_DOMAIN}\": 0"
done

# E2EE
ROOM_E2EE_INITIAL_STATE=""
if [ "${HICLAW_MATRIX_E2EE:-0}" = "1" ] || [ "${HICLAW_MATRIX_E2EE:-}" = "true" ]; then
    ROOM_E2EE_INITIAL_STATE=',"initial_state":[{"type":"m.room.encryption","state_key":"","content":{"algorithm":"m.megolm.v1.aes-sha2"}}]'
fi

TEAM_ROOM_RESP=$(curl -sf -X POST ${HICLAW_MATRIX_SERVER}/_matrix/client/v3/createRoom \
    -H "Authorization: Bearer ${MANAGER_MATRIX_TOKEN}" \
    -H 'Content-Type: application/json' \
    -d '{
        "name": "Team: '"${TEAM_NAME}"'",
        "topic": "Team room for '"${TEAM_NAME}"' — Leader + Workers coordination",
        "invite": ['"${INVITE_LIST}"'],
        "preset": "trusted_private_chat",
        "power_level_content_override": {
            "users": {'"${POWER_USERS}"'}
        }'"${ROOM_E2EE_INITIAL_STATE}"'
    }' 2>/dev/null) || _fail "Failed to create Team Room"

TEAM_ROOM_ID=$(echo "${TEAM_ROOM_RESP}" | jq -r '.room_id // empty')
if [ -z "${TEAM_ROOM_ID}" ]; then
    _fail "Failed to create Team Room: ${TEAM_ROOM_RESP}"
fi
log "  Team Room created: ${TEAM_ROOM_ID}"

# ============================================================
# Step 4: Update Leader's groupAllowFrom to include team workers + Team Admin
# ============================================================
log "Step 4: Updating Leader's groupAllowFrom..."
LEADER_CONFIG="/root/hiclaw-fs/agents/${LEADER_NAME}/openclaw.json"
if [ -f "${LEADER_CONFIG}" ]; then
    for w_name in "${WORKER_NAMES[@]}"; do
        w_name=$(echo "${w_name}" | tr -d ' ')
        [ -z "${w_name}" ] && continue
        W_MATRIX_ID="@${w_name}:${MATRIX_DOMAIN}"
        jq --arg w "${W_MATRIX_ID}" \
            'if (.channels.matrix.groupAllowFrom | index($w)) then .
             else .channels.matrix.groupAllowFrom += [$w]
             end' \
            "${LEADER_CONFIG}" > /tmp/leader-config-tmp.json
        mv /tmp/leader-config-tmp.json "${LEADER_CONFIG}"
    done

    # Add Team Admin to Leader's groupAllowFrom
    if [ -n "${TEAM_ADMIN_MID}" ]; then
        jq --arg a "${TEAM_ADMIN_MID}" \
            'if (.channels.matrix.groupAllowFrom | index($a)) then .
             else .channels.matrix.groupAllowFrom += [$a]
             end
             | if (.channels.matrix.dm.allowFrom | index($a)) then .
               else .channels.matrix.dm.allowFrom += [$a]
               end' \
            "${LEADER_CONFIG}" > /tmp/leader-config-tmp.json
        mv /tmp/leader-config-tmp.json "${LEADER_CONFIG}"
        log "  Added Team Admin to Leader's groupAllowFrom + dm.allowFrom"
    fi

    log "  Leader groupAllowFrom updated"

    # Push updated config to MinIO
    ensure_mc_credentials 2>/dev/null || true
    mc cp "${LEADER_CONFIG}" "${HICLAW_STORAGE_PREFIX}/agents/${LEADER_NAME}/openclaw.json" 2>/dev/null \
        || log "  WARNING: Failed to push leader config to MinIO"
fi

# Add Team Admin to each Worker's groupAllowFrom
if [ -n "${TEAM_ADMIN_MID}" ]; then
    log "  Adding Team Admin to Workers' groupAllowFrom..."
    for w_name in "${WORKER_NAMES[@]}"; do
        w_name=$(echo "${w_name}" | tr -d ' ')
        [ -z "${w_name}" ] && continue
        W_CONFIG="/root/hiclaw-fs/agents/${w_name}/openclaw.json"
        if [ -f "${W_CONFIG}" ]; then
            jq --arg a "${TEAM_ADMIN_MID}" \
                'if (.channels.matrix.groupAllowFrom | index($a)) then .
                 else .channels.matrix.groupAllowFrom += [$a]
                 end' \
                "${W_CONFIG}" > /tmp/worker-config-tmp.json
            mv /tmp/worker-config-tmp.json "${W_CONFIG}"
            mc cp "${W_CONFIG}" "${HICLAW_STORAGE_PREFIX}/agents/${w_name}/openclaw.json" 2>/dev/null \
                || log "  WARNING: Failed to push ${w_name} config to MinIO"
        fi
    done
    log "  Team Admin added to all Workers' groupAllowFrom"
fi

# ============================================================
# Step 4b: Create Leader DM room (Team Admin ↔ Leader)
# ============================================================
LEADER_DM_ROOM_ID=""
if [ -n "${TEAM_ADMIN_MID}" ]; then
    log "Step 4b: Creating Leader DM room (Team Admin ↔ Leader)..."
    LEADER_DM_RESP=$(curl -sf -X POST ${HICLAW_MATRIX_SERVER}/_matrix/client/v3/createRoom \
        -H "Authorization: Bearer ${MANAGER_MATRIX_TOKEN}" \
        -H 'Content-Type: application/json' \
        -d '{
            "name": "Team Admin DM: '"${TEAM_NAME}"'",
            "topic": "Direct channel between Team Admin and Leader of '"${TEAM_NAME}"'",
            "invite": ["'"${TEAM_ADMIN_MID}"'", "'"${LEADER_MATRIX_ID}"'"],
            "preset": "trusted_private_chat",
            "power_level_content_override": {
                "users": {
                    "'"${MANAGER_MATRIX_ID}"'": 100,
                    "'"${TEAM_ADMIN_MID}"'": 100,
                    "'"${LEADER_MATRIX_ID}"'": 0
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
    log "Step 4b: No Team Admin specified, skipping Leader DM room"
fi

# ============================================================
# Step 5: Update teams-registry.json
# ============================================================
log "Step 5: Updating teams-registry.json..."
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
bash /opt/hiclaw/agent/skills/team-management/scripts/manage-teams-registry.sh "${REGISTRY_ARGS[@]}"

# ============================================================
# Step 6: Backfill permissions for humans that reference this team
# If a Human was created before this team, their permissions were
# skipped. Now that the team exists, configure them.
# ============================================================
HUMANS_REGISTRY="${HOME}/humans-registry.json"
if [ -f "${HUMANS_REGISTRY}" ]; then
    PENDING_HUMANS=$(jq -r --arg t "${TEAM_NAME}" \
        '.humans | to_entries[] | select(.value.accessible_teams // [] | index($t)) | .key' \
        "${HUMANS_REGISTRY}" 2>/dev/null)

    if [ -n "${PENDING_HUMANS}" ]; then
        log "Step 6: Backfilling permissions for humans referencing ${TEAM_NAME}..."
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
                mc cp "${LEADER_CONFIG}" "${HICLAW_STORAGE_PREFIX}/agents/${LEADER_NAME}/openclaw.json" 2>/dev/null || true
            fi

            # Add human to each Worker's groupAllowFrom
            for w_name in "${WORKER_NAMES[@]}"; do
                w_name=$(echo "${w_name}" | tr -d ' ')
                [ -z "${w_name}" ] && continue
                W_CONFIG="/root/hiclaw-fs/agents/${w_name}/openclaw.json"
                if [ -f "${W_CONFIG}" ]; then
                    jq --arg h "${_human_mid}" \
                        'if (.channels.matrix.groupAllowFrom | index($h)) then .
                         else .channels.matrix.groupAllowFrom += [$h]
                         end' \
                        "${W_CONFIG}" > /tmp/worker-human-tmp.json
                    mv /tmp/worker-human-tmp.json "${W_CONFIG}"
                    mc cp "${W_CONFIG}" "${HICLAW_STORAGE_PREFIX}/agents/${w_name}/openclaw.json" 2>/dev/null || true
                fi
            done

            # Invite human to Team Room
            if [ -n "${TEAM_ROOM_ID}" ]; then
                ROOM_ENC=$(echo "${TEAM_ROOM_ID}" | sed 's/!/%21/g')
                curl -sf -X POST "${HICLAW_MATRIX_SERVER}/_matrix/client/v3/rooms/${ROOM_ENC}/invite" \
                    -H "Authorization: Bearer ${MANAGER_MATRIX_TOKEN}" \
                    -H 'Content-Type: application/json' \
                    -d '{"user_id": "'"${_human_mid}"'"}' 2>/dev/null || true
            fi

            log "    Permissions configured for ${_human_name}"
        done
    else
        log "Step 6: No pending humans for ${TEAM_NAME} (skipped)"
    fi
else
    log "Step 6: No humans-registry.json found (skipped)"
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
