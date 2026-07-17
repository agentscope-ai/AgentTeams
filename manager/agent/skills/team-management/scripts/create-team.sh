#!/bin/bash
# create-team.sh - Create a Team (Leader + Workers + Team Room)
#
# Default path: delegate to `hiclaw create team` (controller-owned provisioning).
# Set HICLAW_TEAM_CREATE_IMPL=shell to force the legacy Manager-side script
# (create-team-legacy.sh) for Matrix room pre-creation and direct create-worker.sh.
#
# Manager-side-only post-hooks (after hiclaw create team):
#   - Backfill groupAllowFrom + Team Room invites for Humans in ~/humans-registry.json
#     that list this team in accessible_teams but were registered before the team existed.
#
# Usage (same flags as before; --leader is an alias for hiclaw --leader-name):
#   create-team.sh --name <TEAM> --leader <LEADER> --workers <w1,w2,...> \
#     [--description DESC] [--team-name NAME] [--model-provider PROVIDER] \
#     [--leader-model MODEL] [--leader-heartbeat-every 30m] [--worker-idle-timeout 12h] \
#     [--leader-mcp-servers m1,m2] [--worker-models m1,m2,...] [--worker-runtimes r1,r2,...] \
#     [--worker-skills s1:s2] [--worker-mcp-servers m1:m2] \
#     [--team-admin NAME] [--team-admin-matrix-id @user:domain] [--peer-mentions true|false] \
#     [--team-channel-policy JSON] [--team-channel-policy-file PATH] \
#     [--leader-channel-policy JSON] [--leader-channel-policy-file PATH] \
#     [--worker-channel-policies JSON|JSON]

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [ "${HICLAW_TEAM_CREATE_IMPL:-auto}" = "shell" ]; then
    exec bash "${SCRIPT_DIR}/create-team-legacy.sh" "$@"
fi

source /opt/hiclaw/scripts/lib/hiclaw-env.sh

log() {
    local msg="[hiclaw $(date '+%Y-%m-%d %H:%M:%S')] $1"
    echo "${msg}"
    if [ -w /proc/1/fd/1 ]; then
        echo "${msg}" > /proc/1/fd/1
    fi
}

TEAM_NAME=""
LEADER_NAME=""
WORKERS_CSV=""
DESCRIPTION=""
DISPLAY_TEAM_NAME=""
MODEL_PROVIDER=""
LEADER_MODEL=""
LEADER_HEARTBEAT_EVERY=""
WORKER_IDLE_TIMEOUT=""
LEADER_MCP_SERVERS=""
WORKER_MODELS_CSV=""
WORKER_RUNTIMES_CSV=""
WORKER_SKILLS_CSV=""
WORKER_MCP_SERVERS_CSV=""
TEAM_ADMIN=""
TEAM_ADMIN_MATRIX_ID=""
PEER_MENTIONS=""
TEAM_CHANNEL_POLICY_JSON=""
TEAM_CHANNEL_POLICY_FILE=""
LEADER_CHANNEL_POLICY_JSON=""
LEADER_CHANNEL_POLICY_FILE=""
WORKER_CHANNEL_POLICIES_CSV=""

while [ $# -gt 0 ]; do
    case "$1" in
        --name)           TEAM_NAME="$2"; shift 2 ;;
        --leader|--leader-name) LEADER_NAME="$2"; shift 2 ;;
        --workers)        WORKERS_CSV="$2"; shift 2 ;;
        --description)    DESCRIPTION="$2"; shift 2 ;;
        --team-name)      DISPLAY_TEAM_NAME="$2"; shift 2 ;;
        --model-provider) MODEL_PROVIDER="$2"; shift 2 ;;
        --leader-model)   LEADER_MODEL="$2"; shift 2 ;;
        --leader-heartbeat-every) LEADER_HEARTBEAT_EVERY="$2"; shift 2 ;;
        --worker-idle-timeout) WORKER_IDLE_TIMEOUT="$2"; shift 2 ;;
        --leader-mcp-servers) LEADER_MCP_SERVERS="$2"; shift 2 ;;
        --worker-models)  WORKER_MODELS_CSV="$2"; shift 2 ;;
        --worker-runtimes) WORKER_RUNTIMES_CSV="$2"; shift 2 ;;
        --worker-skills)  WORKER_SKILLS_CSV="$2"; shift 2 ;;
        --worker-mcp-servers) WORKER_MCP_SERVERS_CSV="$2"; shift 2 ;;
        --team-admin)     TEAM_ADMIN="$2"; shift 2 ;;
        --team-admin-matrix-id) TEAM_ADMIN_MATRIX_ID="$2"; shift 2 ;;
        --peer-mentions)  PEER_MENTIONS="$2"; shift 2 ;;
        --team-channel-policy) TEAM_CHANNEL_POLICY_JSON="$2"; shift 2 ;;
        --team-channel-policy-file) TEAM_CHANNEL_POLICY_FILE="$2"; shift 2 ;;
        --leader-channel-policy) LEADER_CHANNEL_POLICY_JSON="$2"; shift 2 ;;
        --leader-channel-policy-file) LEADER_CHANNEL_POLICY_FILE="$2"; shift 2 ;;
        --worker-channel-policies) WORKER_CHANNEL_POLICIES_CSV="$2"; shift 2 ;;
        *) echo "Unknown option: $1" >&2; exit 1 ;;
    esac
done

if [ -z "${TEAM_NAME}" ] || [ -z "${LEADER_NAME}" ] || [ -z "${WORKERS_CSV}" ]; then
    echo "Usage: create-team.sh --name <TEAM> --leader <LEADER> --workers <w1,w2,...> [options]" >&2
    echo "Prefer: hiclaw create team --name ... --leader-name ... --workers ..." >&2
    exit 1
fi

if ! command -v hiclaw >/dev/null 2>&1; then
    echo "ERROR: hiclaw not found; set HICLAW_TEAM_CREATE_IMPL=shell for legacy path" >&2
    exit 1
fi

_append_flag() {
    local -n _arr=$1
    local flag="$2"
    local value="$3"
    if [ -n "${value}" ]; then
        _arr+=("${flag}" "${value}")
    fi
}

HICLAW_ARGS=(create team --name "${TEAM_NAME}" --leader-name "${LEADER_NAME}" --workers "${WORKERS_CSV}")
_append_flag HICLAW_ARGS --description "${DESCRIPTION}"
_append_flag HICLAW_ARGS --team-name "${DISPLAY_TEAM_NAME}"
_append_flag HICLAW_ARGS --model-provider "${MODEL_PROVIDER}"
_append_flag HICLAW_ARGS --leader-model "${LEADER_MODEL}"
_append_flag HICLAW_ARGS --leader-heartbeat-every "${LEADER_HEARTBEAT_EVERY}"
_append_flag HICLAW_ARGS --worker-idle-timeout "${WORKER_IDLE_TIMEOUT}"
_append_flag HICLAW_ARGS --leader-mcp-servers "${LEADER_MCP_SERVERS}"
_append_flag HICLAW_ARGS --worker-models "${WORKER_MODELS_CSV}"
_append_flag HICLAW_ARGS --worker-runtimes "${WORKER_RUNTIMES_CSV}"
_append_flag HICLAW_ARGS --worker-skills "${WORKER_SKILLS_CSV}"
_append_flag HICLAW_ARGS --worker-mcp-servers "${WORKER_MCP_SERVERS_CSV}"
_append_flag HICLAW_ARGS --team-admin "${TEAM_ADMIN}"
_append_flag HICLAW_ARGS --team-admin-matrix-id "${TEAM_ADMIN_MATRIX_ID}"
_append_flag HICLAW_ARGS --peer-mentions "${PEER_MENTIONS}"
_append_flag HICLAW_ARGS --team-channel-policy "${TEAM_CHANNEL_POLICY_JSON}"
_append_flag HICLAW_ARGS --team-channel-policy-file "${TEAM_CHANNEL_POLICY_FILE}"
_append_flag HICLAW_ARGS --leader-channel-policy "${LEADER_CHANNEL_POLICY_JSON}"
_append_flag HICLAW_ARGS --leader-channel-policy-file "${LEADER_CHANNEL_POLICY_FILE}"
_append_flag HICLAW_ARGS --worker-channel-policies "${WORKER_CHANNEL_POLICIES_CSV}"

log "=== Creating Team via hiclaw: ${TEAM_NAME} ==="
if ! hiclaw "${HICLAW_ARGS[@]}"; then
    echo '{"error": "hiclaw create team failed"}'
    exit 1
fi

_poll_team_provision() {
    local team_name="$1"
    local leader_name="$2"
    local workers_csv="$3"
    local timeout_secs="${4:-120}"

    local deadline=$(( $(date +%s) + timeout_secs ))
    local team_room leader_dm all_ready=false

    log "Polling team provision (timeout ${timeout_secs}s)..."
    while [ "$(date +%s)" -lt "${deadline}" ]; do
        local team_json workers_json
        team_json=$(hiclaw get teams "${team_name}" -o json 2>/dev/null || echo '{}')
        workers_json=$(hiclaw get workers --team "${team_name}" -o json 2>/dev/null || echo '{"workers":[]}')

        team_room=$(echo "${team_json}" | jq -r '.teamRoomID // empty')
        leader_dm=$(echo "${team_json}" | jq -r '.leaderDMRoomID // empty')

        all_ready=true
        local leader_phase leader_room
        leader_phase=$(echo "${workers_json}" | jq -r --arg n "${leader_name}" '.workers[]? | select(.name == $n) | .phase // empty' | head -n 1)
        leader_room=$(echo "${workers_json}" | jq -r --arg n "${leader_name}" '.workers[]? | select(.name == $n) | .roomID // empty' | head -n 1)
        if [ -z "${leader_room}" ] || [ "${leader_phase}" != "Running" ]; then
            all_ready=false
        fi

        IFS=',' read -ra _poll_workers <<< "${workers_csv}"
        for w_name in "${_poll_workers[@]}"; do
            w_name=$(echo "${w_name}" | tr -d ' ')
            [ -z "${w_name}" ] && continue
            local w_phase w_room
            w_phase=$(echo "${workers_json}" | jq -r --arg n "${w_name}" '.workers[]? | select(.name == $n) | .phase // empty' | head -n 1)
            w_room=$(echo "${workers_json}" | jq -r --arg n "${w_name}" '.workers[]? | select(.name == $n) | .roomID // empty' | head -n 1)
            if [ -z "${w_room}" ] || [ "${w_phase}" != "Running" ]; then
                all_ready=false
                break
            fi
        done

        if [ -n "${team_room}" ] && [ "${all_ready}" = true ]; then
            log "Team provision ready (team room + Running members)"
            return 0
        fi
        sleep 5
    done

    log "WARNING: Team provision poll timed out after ${timeout_secs}s (best-effort continue)"
    return 0
}

_update_teams_registry() {
    local team_name="$1"
    local leader_name="$2"
    local workers_csv="$3"
    local team_room_id="$4"
    local leader_dm_room_id="$5"
    local team_admin_name="$6"
    local team_admin_mid="$7"

    local registry_script="/opt/hiclaw/agent/skills/team-management/scripts/manage-teams-registry.sh"
    if [ ! -x "${registry_script}" ] && [ ! -f "${registry_script}" ]; then
        log "WARNING: manage-teams-registry.sh not found; skipping registry update"
        return 0
    fi

    local registry_args=(
        --action add
        --team-name "${team_name}"
        --leader "${leader_name}"
        --workers "${workers_csv}"
    )
    [ -n "${team_room_id}" ] && registry_args+=(--team-room-id "${team_room_id}")
    [ -n "${team_admin_name}" ] && registry_args+=(--team-admin "${team_admin_name}")
    [ -n "${team_admin_mid}" ] && registry_args+=(--team-admin-matrix-id "${team_admin_mid}")
    [ -n "${leader_dm_room_id}" ] && registry_args+=(--leader-dm-room-id "${leader_dm_room_id}")

    log "Updating teams-registry.json for ${team_name}..."
    bash "${registry_script}" "${registry_args[@]}"
}

_backfill_human_permissions() {
    local team_name="$1"
    local leader_name="$2"
    local team_room_id="$3"
    shift 3
    local -a worker_names=("$@")

    local humans_registry="${HOME}/humans-registry.json"
    if [ ! -f "${humans_registry}" ]; then
        log "No humans-registry.json (skipped human backfill)"
        return 0
    fi

    local pending_humans
    pending_humans=$(jq -r --arg t "${team_name}" \
        '.humans | to_entries[] | select(.value.accessible_teams // [] | index($t)) | .key' \
        "${humans_registry}" 2>/dev/null || true)
    if [ -z "${pending_humans}" ]; then
        log "No pending humans for ${team_name} (skipped human backfill)"
        return 0
    fi

    log "Backfilling permissions for humans referencing ${team_name}..."
    ensure_mc_credentials 2>/dev/null || true

    if [ -z "${MANAGER_MATRIX_TOKEN:-}" ] && [ -f /data/hiclaw-secrets.env ]; then
        # shellcheck disable=SC1091
        source /data/hiclaw-secrets.env
    fi
    if [ -z "${MANAGER_MATRIX_TOKEN:-}" ] && [ -n "${AGENTTEAMS_MANAGER_PASSWORD:-}" ]; then
        MANAGER_MATRIX_TOKEN=$(curl -sf -X POST "${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/login" \
            -H 'Content-Type: application/json' \
            -d '{"type":"m.login.password","identifier":{"type":"m.id.user","user":"manager"},"password":"'"${AGENTTEAMS_MANAGER_PASSWORD}"'"}' \
            2>/dev/null | jq -r '.access_token // empty') || true
    fi

    for _human_name in ${pending_humans}; do
        local _human_mid
        _human_mid=$(jq -r --arg h "${_human_name}" '.humans[$h].matrix_user_id // empty' "${humans_registry}" 2>/dev/null)
        [ -z "${_human_mid}" ] && continue
        log "  Configuring permissions for human: ${_human_name} (${_human_mid})"

        local leader_config="/root/hiclaw-fs/agents/${leader_name}/openclaw.json"
        if [ -f "${leader_config}" ]; then
            jq --arg h "${_human_mid}" \
                'if (.channels.matrix.groupAllowFrom | index($h)) then .
                 else .channels.matrix.groupAllowFrom += [$h]
                 end' \
                "${leader_config}" > /tmp/leader-human-tmp.json
            mv /tmp/leader-human-tmp.json "${leader_config}"
            mc cp "${leader_config}" "${AGENTTEAMS_STORAGE_PREFIX}/agents/${leader_name}/openclaw.json" 2>/dev/null || true
        fi

        for w_name in "${worker_names[@]}"; do
            w_name=$(echo "${w_name}" | tr -d ' ')
            [ -z "${w_name}" ] && continue
            local w_config="/root/hiclaw-fs/agents/${w_name}/openclaw.json"
            if [ -f "${w_config}" ]; then
                jq --arg h "${_human_mid}" \
                    'if (.channels.matrix.groupAllowFrom | index($h)) then .
                     else .channels.matrix.groupAllowFrom += [$h]
                     end' \
                    "${w_config}" > /tmp/worker-human-tmp.json
                mv /tmp/worker-human-tmp.json "${w_config}"
                mc cp "${w_config}" "${AGENTTEAMS_STORAGE_PREFIX}/agents/${w_name}/openclaw.json" 2>/dev/null || true
            fi
        done

        if [ -n "${team_room_id}" ] && [ -n "${MANAGER_MATRIX_TOKEN:-}" ]; then
            local room_enc
            room_enc=$(echo "${team_room_id}" | sed 's/!/%21/g')
            curl -sf -X POST "${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/rooms/${room_enc}/invite" \
                -H "Authorization: Bearer ${MANAGER_MATRIX_TOKEN}" \
                -H 'Content-Type: application/json' \
                -d '{"user_id": "'"${_human_mid}"'"}' 2>/dev/null || true
        fi
    done
}

_emit_result_json() {
    local team_json workers_json
    team_json=$(hiclaw get teams "${TEAM_NAME}" -o json 2>/dev/null || echo '{}')
    workers_json=$(hiclaw get workers --team "${TEAM_NAME}" -o json 2>/dev/null || echo '{"workers":[]}')

    local team_room leader_dm team_admin team_admin_mid leader_room
    team_room=$(echo "${team_json}" | jq -r '.teamRoomID // empty')
    leader_dm=$(echo "${team_json}" | jq -r '.leaderDMRoomID // empty')
    team_admin=$(echo "${team_json}" | jq -r '.admin.name // empty')
    team_admin_mid=$(echo "${team_json}" | jq -r '.admin.matrixUserId // empty')
    leader_room=$(echo "${workers_json}" | jq -r --arg n "${LEADER_NAME}" '.workers[] | select(.name == $n) | .roomID // empty' | head -n 1)

    local workers_result="[]"
    IFS=',' read -ra WORKER_NAMES <<< "${WORKERS_CSV}"
    for w_name in "${WORKER_NAMES[@]}"; do
        w_name=$(echo "${w_name}" | tr -d ' ')
        [ -z "${w_name}" ] && continue
        local w_room
        w_room=$(echo "${workers_json}" | jq -r --arg n "${w_name}" '.workers[] | select(.name == $n) | .roomID // empty' | head -n 1)
        workers_result=$(echo "${workers_result}" | jq --arg n "${w_name}" --arg r "${w_room}" '. += [{name: $n, room_id: $r}]')
    done

    jq -n \
        --arg team "${TEAM_NAME}" \
        --arg leader "${LEADER_NAME}" \
        --arg leader_room "${leader_room}" \
        --arg team_room "${team_room}" \
        --arg leader_dm_room "${leader_dm}" \
        --arg team_admin "${team_admin}" \
        --arg team_admin_mid "${team_admin_mid}" \
        --argjson workers "${workers_result}" \
        '{
            team_name: $team,
            leader: $leader,
            leader_room_id: $leader_room,
            team_room_id: $team_room,
            leader_dm_room_id: (if $leader_dm_room == "" then null else $leader_dm_room end),
            team_admin: (if $team_admin == "" then null else $team_admin end),
            team_admin_matrix_id: (if $team_admin_mid == "" then null else $team_admin_mid end),
            workers: $workers
        }'
}

IFS=',' read -ra WORKER_NAMES <<< "${WORKERS_CSV}"
_poll_team_provision "${TEAM_NAME}" "${LEADER_NAME}" "${WORKERS_CSV}" 120

TEAM_JSON=$(hiclaw get teams "${TEAM_NAME}" -o json 2>/dev/null || echo '{}')
TEAM_ROOM_ID=$(echo "${TEAM_JSON}" | jq -r '.teamRoomID // empty')
LEADER_DM_ROOM_ID=$(echo "${TEAM_JSON}" | jq -r '.leaderDMRoomID // empty')
TEAM_ADMIN_NAME=$(echo "${TEAM_JSON}" | jq -r '.admin.name // empty')
TEAM_ADMIN_MID=$(echo "${TEAM_JSON}" | jq -r '.admin.matrixUserId // empty')
[ -n "${TEAM_ADMIN}" ] && TEAM_ADMIN_NAME="${TEAM_ADMIN}"
[ -n "${TEAM_ADMIN_MATRIX_ID}" ] && TEAM_ADMIN_MID="${TEAM_ADMIN_MATRIX_ID}"

_update_teams_registry \
    "${TEAM_NAME}" \
    "${LEADER_NAME}" \
    "${WORKERS_CSV}" \
    "${TEAM_ROOM_ID}" \
    "${LEADER_DM_ROOM_ID}" \
    "${TEAM_ADMIN_NAME}" \
    "${TEAM_ADMIN_MID}"

_backfill_human_permissions "${TEAM_NAME}" "${LEADER_NAME}" "${TEAM_ROOM_ID}" "${WORKER_NAMES[@]}"

echo "---RESULT---"
_emit_result_json
