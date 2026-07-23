#!/bin/bash
# update-worker-config.sh - Update an existing Worker's configuration
#
# Two modes:
#
# 1. In-place edit (default): regenerate openclaw.json, push skills, reauth
#    MCP, mc mirror to MinIO. Container is NOT recreated; openclaw watches
#    files and picks up live. Memory preserved.
#
# 2. Runtime switch (when --runtime is provided): delegates to
#    `agt update worker --runtime <RUNTIME> ...` and polls until
#    phase=Running. Controller's reconcile destroys the old container and
#    creates a new one with the new runtime image; agent config files are
#    regenerated from the new runtime's templates. Matrix account, room,
#    gateway consumer, MinIO data are preserved; container-local ephemeral
#    state (caches) is lost.
#
# Usage:
#   update-worker-config.sh --name <NAME> [--model <MODEL_ID>] [--skills s1,s2] [--mcp-servers s1,s2] [--package-dir <DIR>]
#   update-worker-config.sh --name <NAME> --runtime <openclaw|copaw|hermes|openhuman> [--model <MODEL_ID>] [--skills s1,s2] [--mcp-servers s1,s2]
#
# Prerequisites:
#   - Worker must already exist (created via create-worker.sh)
#   - Credentials at /data/worker-creds/<NAME>.env (in-place mode only)

set -e
source /opt/agentteams/scripts/lib/agentteams-env.sh

log() {
    local msg="[agentteams $(date '+%Y-%m-%d %H:%M:%S')] $1"
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
WORKER_NAME=""
MODEL_ID=""
MCP_SERVERS=""
WORKER_SKILLS=""
PACKAGE_DIR=""
CHANNEL_POLICY_JSON=""
RUNTIME=""

while [ $# -gt 0 ]; do
    case "$1" in
        --name)        WORKER_NAME="$2"; shift 2 ;;
        --model)       MODEL_ID="$2"; shift 2 ;;
        --skills)      WORKER_SKILLS="$2"; shift 2 ;;
        --mcp-servers) MCP_SERVERS="$2"; shift 2 ;;
        --package-dir) PACKAGE_DIR="$2"; shift 2 ;;
        --channel-policy) CHANNEL_POLICY_JSON="$2"; shift 2 ;;
        --runtime)     RUNTIME="$2"; shift 2 ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

if [ -z "${WORKER_NAME}" ]; then
    echo "Usage: update-worker-config.sh --name <NAME> [--model <MODEL>] [--skills s1,s2] [--mcp-servers s1,s2] [--package-dir <DIR>]"
    echo "       update-worker-config.sh --name <NAME> --runtime <openclaw|copaw|hermes|openhuman> [--model <MODEL>] [--skills s1,s2] [--mcp-servers s1,s2]"
    exit 1
fi

# ============================================================
# Runtime switch mode: delegate to `agt update worker` and poll.
#
# Why: changing runtime requires destroying the old container and
# starting a new one from a different image (openclaw vs copaw vs
# hermes vs openhuman). The controller's reconcile loop is the only path that
# does this correctly — see agentteams-controller/internal/controller/
# member_reconcile.go::ensureMemberContainerPresent. Trying to do
# it in-place from the manager would double-write config files and
# leave the running container on the old runtime.
# ============================================================
if [ -n "${RUNTIME}" ]; then
    case "${RUNTIME}" in
        openclaw|copaw|hermes|openhuman) ;;
        *) _fail "Invalid --runtime '${RUNTIME}'. Must be one of: openclaw, copaw, hermes, openhuman." ;;
    esac

    if [ -n "${PACKAGE_DIR}" ]; then
        _fail "--package-dir cannot be combined with --runtime. Run the package update separately (without --runtime) after the runtime switch settles."
    fi
    if [ -n "${CHANNEL_POLICY_JSON}" ]; then
        _fail "--channel-policy cannot be combined with --runtime. Apply the channel-policy separately (without --runtime) after the runtime switch settles."
    fi

    log "=== Switching runtime for Worker: ${WORKER_NAME} -> ${RUNTIME} ==="
    log "  WARNING: existing container will be destroyed and recreated."
    log "  Preserved: Matrix account/room, gateway consumer, MinIO data, persisted credentials."
    log "  Lost: container-local ephemeral state (caches, /tmp, in-memory session)."

    CLI_ARGS=(update worker --name "${WORKER_NAME}" --runtime "${RUNTIME}")
    [ -n "${MODEL_ID}" ]      && CLI_ARGS+=(--model "${MODEL_ID}")
    [ -n "${WORKER_SKILLS}" ] && CLI_ARGS+=(--skills "${WORKER_SKILLS}")
    [ -n "${MCP_SERVERS}" ]   && CLI_ARGS+=(--mcp-servers "${MCP_SERVERS}")

    log "Step 1: Calling: agentteams ${CLI_ARGS[*]}"
    if ! CLI_OUT=$(agentteams "${CLI_ARGS[@]}" 2>&1); then
        _fail "agt update worker failed: ${CLI_OUT}"
    fi
    log "  ${CLI_OUT}"

    # Step 2: Poll for phase=Running. Container recreate typically takes
    # 10-45s (openclaw 10-30, copaw 15-45, hermes 15-45 — see
    # references/create-worker.md Step 2.5). Cap at 120s to absorb image
    # pull on a cold node.
    log "Step 2: Polling phase until Running (timeout 120s)..."
    POLL_DEADLINE=$(( $(date +%s) + 120 ))
    PHASE=""
    MESSAGE=""
    CURRENT_RUNTIME=""
    while [ "$(date +%s)" -lt "${POLL_DEADLINE}" ]; do
        WORKER_JSON=$(agt get workers -o json 2>/dev/null \
            | jq -c --arg n "${WORKER_NAME}" '.workers[]? | select(.name == $n)')
        if [ -n "${WORKER_JSON}" ]; then
            PHASE=$(echo "${WORKER_JSON}" | jq -r '.phase // ""')
            MESSAGE=$(echo "${WORKER_JSON}" | jq -r '.message // ""')
            CURRENT_RUNTIME=$(echo "${WORKER_JSON}" | jq -r '.runtime // ""')
            log "  phase=${PHASE} runtime=${CURRENT_RUNTIME}"
            if [ "${PHASE}" = "Running" ] && [ "${CURRENT_RUNTIME}" = "${RUNTIME}" ]; then
                break
            fi
            if [ "${PHASE}" = "Failed" ]; then
                _fail "Worker entered Failed phase during runtime switch: ${MESSAGE}"
            fi
        fi
        sleep 5
    done

    if [ "${PHASE}" != "Running" ] || [ "${CURRENT_RUNTIME}" != "${RUNTIME}" ]; then
        _fail "Timeout waiting for ${WORKER_NAME} to reach Running on runtime=${RUNTIME} (last phase=${PHASE}, runtime=${CURRENT_RUNTIME}, message=${MESSAGE})."
    fi

    log "  Worker ${WORKER_NAME} is now Running on runtime=${RUNTIME}"

    echo "---RESULT---"
    jq -n \
        --arg name "${WORKER_NAME}" \
        --arg runtime "${RUNTIME}" \
        --arg model "${MODEL_ID:-unchanged}" \
        --arg status "runtime_switched" \
        '{
            worker_name: $name,
            runtime: $runtime,
            model: $model,
            status: $status,
            note: "Container recreated. Matrix account, room, and persisted state preserved. Container-local ephemeral state lost."
        }'
    exit 0
fi

MATRIX_DOMAIN="${AGENTTEAMS_MATRIX_DOMAIN:-matrix-local.agentteams.io:8080}"
ADMIN_USER="${AGENTTEAMS_ADMIN_USER:-admin}"

log "=== Updating Worker: ${WORKER_NAME} ==="
log "  Memory: preserved (not overwritten)"
log "  Skills: merged (existing updated, new added, old kept)"

# ============================================================
# Step 1: Load persisted credentials
# ============================================================
log "Step 1: Loading credentials..."
WORKER_CREDS_FILE="/data/worker-creds/${WORKER_NAME}.env"
if [ ! -f "${WORKER_CREDS_FILE}" ]; then
    _fail "Credentials not found at ${WORKER_CREDS_FILE}. Worker may not have been created yet."
fi
source "${WORKER_CREDS_FILE}"

# Get fresh Matrix token via login
WORKER_MATRIX_TOKEN=$(curl -sf -X POST ${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/login \
    -H 'Content-Type: application/json' \
    -d '{
        "type": "m.login.password",
        "identifier": {"type": "m.id.user", "user": "'"${WORKER_NAME}"'"},
        "password": "'"${WORKER_PASSWORD}"'"
    }' 2>/dev/null | jq -r '.access_token // empty')

if [ -z "${WORKER_MATRIX_TOKEN}" ]; then
    log "  WARNING: Could not obtain fresh Matrix token (using placeholder)"
    WORKER_MATRIX_TOKEN="placeholder"
fi

WORKER_KEY="${WORKER_GATEWAY_KEY:-placeholder}"
log "  Credentials loaded"

# ============================================================
# Step 2: Deploy package if specified (SOUL.md, custom skills)
# ============================================================
if [ -n "${PACKAGE_DIR}" ] && [ -d "${PACKAGE_DIR}" ]; then
    log "Step 2: Deploying package contents..."
    AGENT_DIR="/root/agentteams-fs/agents/${WORKER_NAME}"

    # Copy config/ contents (SOUL.md, etc.) — overwrites existing
    # AGENTS.md is handled specially: user content wrapped with builtin markers
    if [ -d "${PACKAGE_DIR}/config" ]; then
        for f in "${PACKAGE_DIR}/config"/*; do
            [ ! -f "$f" ] && continue
            FNAME=$(basename "$f")
            if [ "${FNAME}" = "AGENTS.md" ]; then
                # Wrap user AGENTS.md with builtin markers so merge logic works
                source /opt/agentteams/scripts/lib/builtin-merge.sh
                if ! grep -q 'agentteams-builtin-start' "$f" 2>/dev/null; then
                    {
                        printf '%s\n' "${BUILTIN_HEADER}"
                        printf '%s\n' "${BUILTIN_END}"
                        echo ""
                        cat "$f"
                    } > "${AGENT_DIR}/AGENTS.md"
                else
                    cp "$f" "${AGENT_DIR}/AGENTS.md"
                fi
                log "    Updated: AGENTS.md (with builtin markers)"
            else
                cp "$f" "${AGENT_DIR}/${FNAME}"
                log "    Updated: ${FNAME}"
            fi
        done
    elif [ -f "${PACKAGE_DIR}/SOUL.md" ]; then
        cp "${PACKAGE_DIR}/SOUL.md" "${AGENT_DIR}/SOUL.md"
        log "    Updated: SOUL.md"
    fi

    # Copy custom skills (merged into skills/ alongside builtins)
    if [ -d "${PACKAGE_DIR}/skills" ]; then
        mkdir -p "${AGENT_DIR}/skills"
        cp -r "${PACKAGE_DIR}/skills"/* "${AGENT_DIR}/skills/" 2>/dev/null || true
        log "    Custom skills merged"
    fi

    # Re-merge builtin section into AGENTS.md
    log "  Re-merging builtin AGENTS.md section..."
    source /opt/agentteams/scripts/lib/builtin-merge.sh

    # Determine correct agent source for builtin content
    _worker_json=$(agt get workers "${WORKER_NAME}" -o json)
    _role=$(echo "${_worker_json}" | jq -r '.role // "worker"')
    _runtime=$(echo "${_worker_json}" | jq -r '.runtime // "openclaw"')
    if [ "${_role}" = "team_leader" ] && [ -d "/opt/agentteams/agent/team-leader-agent" ]; then
        _agent_src="/opt/agentteams/agent/team-leader-agent"
    elif [ "${_runtime}" = "copaw" ]; then
        _agent_src="/opt/agentteams/agent/copaw-worker-agent"
    elif [ "${_runtime}" = "hermes" ]; then
        _agent_src="/opt/agentteams/agent/hermes-worker-agent"
    else
        _agent_src="/opt/agentteams/agent/worker-agent"
    fi

    if [ -f "${_agent_src}/AGENTS.md" ]; then
        update_builtin_section "${AGENT_DIR}/AGENTS.md" "${_agent_src}/AGENTS.md"
        log "    Builtin section merged"
    fi

    # Re-inject team-context coordination block
    _team_id=$(echo "${_worker_json}" | jq -r '.team // empty')
    _team_leader=""
    _team_json='{}'
    if [ -n "${_team_id}" ] && [ "${_role}" = "worker" ]; then
        _team_json=$(agt get teams "${_team_id}" -o json)
        _team_leader=$(echo "${_team_json}" | jq -r '.leaderName // empty')
    elif [ -n "${_team_id}" ]; then
        _team_json=$(agt get teams "${_team_id}" -o json)
    fi

    _ctx_tmp=$(mktemp /tmp/team-ctx-update-XXXXXX.md)
    if [ -n "${_team_leader}" ]; then
        cat > "${_ctx_tmp}" <<TEAMCTX

<!-- agentteams-team-context-start -->
## Coordination

- **Coordinator**: @${_team_leader}:${MATRIX_DOMAIN} (Team Leader of ${_team_id})
- Report task completion, blockers, and questions to your coordinator
- Only respond to @mentions from your coordinator and Admin
- Do NOT @mention Manager directly — all communication goes through your Team Leader
<!-- agentteams-team-context-end -->
TEAMCTX
    elif [ "${_role}" = "team_leader" ]; then
        _team_workers=$(echo "${_team_json}" | jq -r '.workerNames // [] | join(", ")')
        _team_room_id=$(echo "${_team_json}" | jq -r '.teamRoomID // empty')
        _leader_dm_room_id=$(echo "${_team_json}" | jq -r '.leaderDMRoomID // empty')
        _team_admin_mid=$(echo "${_team_json}" | jq -r '.admin.matrixUserId // empty')
        _worker_rooms=$(agt get workers --team "${_team_id}" -o json | jq -r '
            [.workers[] | select(.role == "worker") |
             "  - @\(.name):__DOMAIN__ — Room: \(.roomID // "unknown")"] | join("\n")')
        _worker_rooms=$(echo "${_worker_rooms}" | sed "s/__DOMAIN__/${MATRIX_DOMAIN}/g")
        cat > "${_ctx_tmp}" <<LEADERCTX

<!-- agentteams-team-context-start -->
## Coordination

- **Upstream coordinator**: @manager:${MATRIX_DOMAIN} (Manager) — you receive tasks from Manager
$([ -n "${_team_admin_mid}" ] && echo "- **Team Admin**: ${_team_admin_mid} — can assign tasks and make decisions within the team")
- **Team**: ${_team_id}
$([ -n "${_team_room_id}" ] && echo "- **Team Room**: ${_team_room_id} — @mention workers here for task assignment")
$([ -n "${_leader_dm_room_id}" ] && echo "- **Leader DM**: ${_leader_dm_room_id} — Team Admin communicates with you here")
$([ -n "${_worker_rooms}" ] && echo "- **Team Workers**:" && echo "${_worker_rooms}")
- You decompose tasks from Manager or Team Admin and assign sub-tasks to your team workers
- @mention workers in the Team Room for task assignment
- This Coordination block is already loaded into your system prompt; use these room IDs and worker Matrix IDs directly, without narrating topology checks or AGENTS.md reads
- Report results to Manager (in Leader Room) or Team Admin (in Leader DM) based on task source
- @mention Manager only for: task completion, blockers, escalations
<!-- agentteams-team-context-end -->
LEADERCTX
    else
        cat > "${_ctx_tmp}" <<STDCTX

<!-- agentteams-team-context-start -->
## Coordination

- **Coordinator**: @manager:${MATRIX_DOMAIN} (Manager)
- Report task completion, blockers, and questions to your coordinator
- Only respond to @mentions from your coordinator and Admin
<!-- agentteams-team-context-end -->
STDCTX
    fi

    # Remove existing team-context, insert after builtin-end
    sed -i '/<!-- agentteams-team-context-start -->/,/<!-- agentteams-team-context-end -->/d' "${AGENT_DIR}/AGENTS.md" 2>/dev/null || true
    if grep -q 'agentteams-builtin-end' "${AGENT_DIR}/AGENTS.md"; then
        sed -i "/<!-- agentteams-builtin-end -->/r ${_ctx_tmp}" "${AGENT_DIR}/AGENTS.md"
    else
        cat "${_ctx_tmp}" >> "${AGENT_DIR}/AGENTS.md"
    fi
    rm -f "${_ctx_tmp}"
    log "    Team-context block re-injected"
else
    log "Step 2: No package to deploy (skipped)"
fi

# ============================================================
# Step 3: Regenerate openclaw.json if model specified
# ============================================================
if [ -n "${MODEL_ID}" ]; then
    log "Step 3: Regenerating openclaw.json (model=${MODEL_ID})..."

    # Read Team membership from the Controller API.
    TEAM_LEADER=""
    _worker_json=$(agt get workers "${WORKER_NAME}" -o json)
    WORKER_ROLE=$(echo "${_worker_json}" | jq -r '.role // "worker"')
    WORKER_TEAM=$(echo "${_worker_json}" | jq -r '.team // empty')
    if [ "${WORKER_ROLE}" = "worker" ] && [ -n "${WORKER_TEAM}" ]; then
        TEAM_LEADER=$(agt get teams "${WORKER_TEAM}" -o json | jq -r '.leaderName // empty')
    fi

    GEN_ARGS=("${WORKER_NAME}" "${WORKER_MATRIX_TOKEN}" "${WORKER_KEY}" "${MODEL_ID}")
    if [ -n "${TEAM_LEADER}" ]; then
        GEN_ARGS+=("${TEAM_LEADER}")
    fi

    # Persist new comm policy if provided, then export for generate-worker-config.sh
    AGENT_DIR="/root/agentteams-fs/agents/${WORKER_NAME}"
    POLICY_FILE="${AGENT_DIR}/channel-policy.json"
    if [ -n "${CHANNEL_POLICY_JSON}" ]; then
        echo "${CHANNEL_POLICY_JSON}" > "${POLICY_FILE}"
    fi
    if [ -f "${POLICY_FILE}" ]; then
        export WORKER_CHANNEL_POLICY=$(cat "${POLICY_FILE}")
    fi

    bash /opt/agentteams/agent/skills/worker-management/scripts/generate-worker-config.sh "${GEN_ARGS[@]}"
    log "  openclaw.json regenerated"
else
    log "Step 3: No model change (skipped)"
fi

# ============================================================
# Step 4: Push skills (additive)
# ============================================================
if [ -n "${WORKER_SKILLS}" ]; then
    log "Step 4: Pushing skills..."
    bash /opt/agentteams/agent/skills/worker-management/scripts/push-worker-skills.sh \
        --worker "${WORKER_NAME}" --no-notify \
        || log "  WARNING: push-worker-skills.sh returned non-zero"
    log "  Skills pushed"
else
    log "Step 4: No skill changes (skipped)"
fi

# ============================================================
# Step 5: Reauthorize MCP servers if specified
# ============================================================
if [ -n "${MCP_SERVERS}" ]; then
    log "Step 5: Reauthorizing MCP servers..."
    source /opt/agentteams/scripts/lib/gateway-api.sh
    gateway_ensure_session || log "  WARNING: Failed to establish gateway session"
    CONSUMER_NAME="worker-${WORKER_NAME}"
    gateway_authorize_mcp "${CONSUMER_NAME}" "${MCP_SERVERS}" \
        || log "  WARNING: MCP reauthorization failed"
    log "  MCP servers reauthorized"
else
    log "Step 5: No MCP changes (skipped)"
fi

# ============================================================
# Step 6: Sync config to MinIO (exclude memory)
# ============================================================
log "Step 6: Syncing config to MinIO (memory preserved)..."
ensure_mc_credentials 2>/dev/null || true
mc mirror "/root/agentteams-fs/agents/${WORKER_NAME}/" \
    "${AGENTTEAMS_STORAGE_PREFIX}/agents/${WORKER_NAME}/" \
    --overwrite \
    --exclude "memory/*" \
    --exclude "MEMORY.md" \
    2>&1 | tail -3
log "  Config synced (memory excluded)"

# ============================================================
# Output
# ============================================================
echo "---RESULT---"
jq -n \
    --arg name "${WORKER_NAME}" \
    --arg model "${MODEL_ID:-unchanged}" \
    --arg status "updated" \
    '{
        worker_name: $name,
        model: $model,
        status: $status,
        note: "Memory preserved, skills merged"
    }'
