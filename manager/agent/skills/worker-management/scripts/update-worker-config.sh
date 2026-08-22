#!/bin/bash
# update-worker-config.sh - Update an existing Worker's configuration
#
# Reads persisted credentials, regenerates openclaw.json, pushes skills,
# and syncs config to MinIO. Memory is preserved.
#
# Usage:
#   update-worker-config.sh --name <NAME> [--model <MODEL_ID>] [--skills s1,s2] [--mcp-servers s1,s2] [--package-dir <DIR>]
#
# Prerequisites:
#   - Worker must already exist (created via create-worker.sh)
#   - Credentials at /data/worker-creds/<NAME>.env

set -e
source /opt/hiclaw/scripts/lib/hiclaw-env.sh

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
WORKER_NAME=""
MODEL_ID=""
MCP_SERVERS=""
WORKER_SKILLS=""
PACKAGE_DIR=""

while [ $# -gt 0 ]; do
    case "$1" in
        --name)        WORKER_NAME="$2"; shift 2 ;;
        --model)       MODEL_ID="$2"; shift 2 ;;
        --skills)      WORKER_SKILLS="$2"; shift 2 ;;
        --mcp-servers) MCP_SERVERS="$2"; shift 2 ;;
        --package-dir) PACKAGE_DIR="$2"; shift 2 ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

if [ -z "${WORKER_NAME}" ]; then
    echo "Usage: update-worker-config.sh --name <NAME> [--model <MODEL>] [--skills s1,s2] [--mcp-servers s1,s2] [--package-dir <DIR>]"
    exit 1
fi

MATRIX_DOMAIN="${HICLAW_MATRIX_DOMAIN:-matrix-local.hiclaw.io:8080}"
ADMIN_USER="${HICLAW_ADMIN_USER:-admin}"

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
WORKER_MATRIX_TOKEN=$(curl -sf -X POST ${HICLAW_MATRIX_SERVER}/_matrix/client/v3/login \
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
    AGENT_DIR="/root/hiclaw-fs/agents/${WORKER_NAME}"

    # Copy config/ contents (SOUL.md, etc.) — overwrites existing
    # AGENTS.md is handled specially: user content wrapped with builtin markers
    if [ -d "${PACKAGE_DIR}/config" ]; then
        for f in "${PACKAGE_DIR}/config"/*; do
            [ ! -f "$f" ] && continue
            FNAME=$(basename "$f")
            if [ "${FNAME}" = "AGENTS.md" ]; then
                # Wrap user AGENTS.md with builtin markers so merge logic works
                source /opt/hiclaw/scripts/lib/builtin-merge.sh
                if ! grep -q 'hiclaw-builtin-start' "$f" 2>/dev/null; then
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
    source /opt/hiclaw/scripts/lib/builtin-merge.sh

    # Determine correct agent source for builtin content
    REGISTRY_FILE="${HOME}/workers-registry.json"
    _role=$(jq -r --arg w "${WORKER_NAME}" '.workers[$w].role // "worker"' "${REGISTRY_FILE}" 2>/dev/null || echo "worker")
    _runtime=$(jq -r --arg w "${WORKER_NAME}" '.workers[$w].runtime // "openclaw"' "${REGISTRY_FILE}" 2>/dev/null || echo "openclaw")
    if [ "${_role}" = "team_leader" ] && [ -d "/opt/hiclaw/agent/team-leader-agent" ]; then
        _agent_src="/opt/hiclaw/agent/team-leader-agent"
    elif [ "${_runtime}" = "copaw" ]; then
        _agent_src="/opt/hiclaw/agent/copaw-worker-agent"
    else
        _agent_src="/opt/hiclaw/agent/worker-agent"
    fi

    if [ -f "${_agent_src}/AGENTS.md" ]; then
        update_builtin_section "${AGENT_DIR}/AGENTS.md" "${_agent_src}/AGENTS.md"
        log "    Builtin section merged"
    fi

    # Re-inject team-context coordination block
    _team_id=$(jq -r --arg w "${WORKER_NAME}" '.workers[$w].team_id // empty' "${REGISTRY_FILE}" 2>/dev/null)
    _team_leader=""
    if [ -n "${_team_id}" ] && [ "${_role}" = "worker" ]; then
        TEAMS_REGISTRY="${HOME}/teams-registry.json"
        if [ -f "${TEAMS_REGISTRY}" ]; then
            _team_leader=$(jq -r --arg t "${_team_id}" '.teams[$t].leader // empty' "${TEAMS_REGISTRY}" 2>/dev/null)
        fi
    fi

    _ctx_tmp=$(mktemp /tmp/team-ctx-update-XXXXXX.md)
    if [ -n "${_team_leader}" ]; then
        cat > "${_ctx_tmp}" <<TEAMCTX

<!-- hiclaw-team-context-start -->
## Coordination

- **Coordinator**: @${_team_leader}:${MATRIX_DOMAIN} (Team Leader of ${_team_id})
- Report task completion, blockers, and questions to your coordinator
- Only respond to @mentions from your coordinator and Admin
- Do NOT @mention Manager directly — all communication goes through your Team Leader
<!-- hiclaw-team-context-end -->
TEAMCTX
    elif [ "${_role}" = "team_leader" ]; then
        _team_workers=$(jq -r --arg t "${_team_id}" '.teams[$t].workers // [] | join(", ")' "${HOME}/teams-registry.json" 2>/dev/null)
        cat > "${_ctx_tmp}" <<LEADERCTX

<!-- hiclaw-team-context-start -->
## Coordination

- **Upstream coordinator**: @manager:${MATRIX_DOMAIN} (Manager) — you receive tasks from Manager
- **Team**: ${_team_id}
- **Workers**: ${_team_workers}
- You decompose tasks from Manager and assign sub-tasks to your team workers
- Report aggregated results to Manager when all sub-tasks complete
<!-- hiclaw-team-context-end -->
LEADERCTX
    else
        cat > "${_ctx_tmp}" <<STDCTX

<!-- hiclaw-team-context-start -->
## Coordination

- **Coordinator**: @manager:${MATRIX_DOMAIN} (Manager)
- Report task completion, blockers, and questions to your coordinator
- Only respond to @mentions from your coordinator and Admin
<!-- hiclaw-team-context-end -->
STDCTX
    fi

    # Remove existing team-context, insert after builtin-end
    sed -i '/<!-- hiclaw-team-context-start -->/,/<!-- hiclaw-team-context-end -->/d' "${AGENT_DIR}/AGENTS.md" 2>/dev/null || true
    if grep -q 'hiclaw-builtin-end' "${AGENT_DIR}/AGENTS.md"; then
        sed -i "/<!-- hiclaw-builtin-end -->/r ${_ctx_tmp}" "${AGENT_DIR}/AGENTS.md"
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

    # Read team-leader from registry if this is a team worker
    TEAM_LEADER=""
    REGISTRY_FILE="${HOME}/workers-registry.json"
    if [ -f "${REGISTRY_FILE}" ]; then
        WORKER_ROLE=$(jq -r --arg w "${WORKER_NAME}" '.workers[$w].role // "worker"' "${REGISTRY_FILE}" 2>/dev/null)
        WORKER_TEAM=$(jq -r --arg w "${WORKER_NAME}" '.workers[$w].team_id // empty' "${REGISTRY_FILE}" 2>/dev/null)
        if [ "${WORKER_ROLE}" = "worker" ] && [ -n "${WORKER_TEAM}" ]; then
            # Find team leader from teams-registry
            TEAMS_REGISTRY="${HOME}/teams-registry.json"
            if [ -f "${TEAMS_REGISTRY}" ]; then
                TEAM_LEADER=$(jq -r --arg t "${WORKER_TEAM}" '.teams[$t].leader // empty' "${TEAMS_REGISTRY}" 2>/dev/null)
            fi
        fi
    fi

    GEN_ARGS=("${WORKER_NAME}" "${WORKER_MATRIX_TOKEN}" "${WORKER_KEY}" "${MODEL_ID}")
    if [ -n "${TEAM_LEADER}" ]; then
        GEN_ARGS+=("${TEAM_LEADER}")
    fi

    bash /opt/hiclaw/agent/skills/worker-management/scripts/generate-worker-config.sh "${GEN_ARGS[@]}"
    log "  openclaw.json regenerated"
else
    log "Step 3: No model change (skipped)"
fi

# ============================================================
# Step 4: Push skills (additive)
# ============================================================
if [ -n "${WORKER_SKILLS}" ]; then
    log "Step 4: Pushing skills..."
    bash /opt/hiclaw/agent/skills/worker-management/scripts/push-worker-skills.sh \
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
    source /opt/hiclaw/scripts/lib/gateway-api.sh
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
mc mirror "/root/hiclaw-fs/agents/${WORKER_NAME}/" \
    "${HICLAW_STORAGE_PREFIX}/agents/${WORKER_NAME}/" \
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
