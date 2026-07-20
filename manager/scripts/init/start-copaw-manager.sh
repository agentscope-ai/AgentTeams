#!/bin/bash
# start-copaw-manager.sh - Start Manager Agent with CoPaw runtime
# Called by start-manager-agent.sh when AGENTTEAMS_MANAGER_RUNTIME=copaw
#
# This script converts an OpenClaw-style workspace to a CoPaw-style workspace
# and then launches the CoPaw application.

source /opt/agentteams/scripts/lib/agentteams-env.sh

# ============================================================
# Path definitions
# Note: In Manager container, HOME is set to /root/manager-workspace
# ============================================================
OPENCLAW_WORKSPACE="${HOME}"
COPAW_WORKING_DIR="${HOME}/.copaw"

# ============================================================
# 1. Create CoPaw directory structure
# ============================================================
log "Creating CoPaw directory structure..."
mkdir -p "${COPAW_WORKING_DIR}/custom_channels"
mkdir -p "${COPAW_WORKING_DIR}/.secret"

# ============================================================
# 2. Bridge openclaw.json -> config.json + providers.json
# ============================================================
OPENCLAW_JSON="${OPENCLAW_WORKSPACE}/openclaw.json"
if [ ! -f "${OPENCLAW_JSON}" ]; then
    log "ERROR: openclaw.json not found at ${OPENCLAW_JSON}"
    exit 1
fi

# One-shot migration: older bridges wrote config.json with
# security.tool_guard.enabled=true, which overrides agent.json and causes
# noisy approval prompts on every tool call. Archive that legacy file so
# the bridge re-seeds a fresh one from the template (security off).
if [ -f "${COPAW_WORKING_DIR}/config.json" ]; then
    if command -v jq >/dev/null 2>&1 && \
       [ "$(jq -r '.security.tool_guard.enabled // false' "${COPAW_WORKING_DIR}/config.json")" = "true" ] && \
       [ ! -f "${COPAW_WORKING_DIR}/.config-migrated-v2" ]; then
        archive="${COPAW_WORKING_DIR}/config.json.legacy-$(date +%Y%m%d-%H%M%S)"
        log "Archiving legacy config.json (tool_guard enabled) -> $(basename "${archive}")"
        mv "${COPAW_WORKING_DIR}/config.json" "${archive}"
        touch "${COPAW_WORKING_DIR}/.config-migrated-v2"
    fi
fi

log "Bridging openclaw.json -> CoPaw config (manager)..."
PYTHONPATH="/opt/agentteams/copaw/src:${PYTHONPATH:-}" \
    python3 -m copaw_worker.bridge \
        --profile manager \
        --openclaw-json "${OPENCLAW_JSON}" \
        --working-dir "${COPAW_WORKING_DIR}"
log "Config bridged from openclaw.json"

# ============================================================
# 3. Sync prompt files into CoPaw paths
# ============================================================
# Canonical AgentTeams layout is OPENCLAW_WORKSPACE ($HOME): SOUL.md, memory/, skills/ etc.
# CoPaw reads from COPAW_WORKING_DIR/workspaces/default/; we sync into that path only.
# Use cp -u / cp -ru so we never overwrite newer files already in workspaces/default/.
# ============================================================
WORKSPACE_DIR="${COPAW_WORKING_DIR}/workspaces/default"
mkdir -p "${WORKSPACE_DIR}"

log "Syncing prompt files (cp -u: update only if source is newer)..."
for _f in AGENTS.md SOUL.md HEARTBEAT.md TOOLS.md; do
    if [ -f "${OPENCLAW_WORKSPACE}/${_f}" ]; then
        cp -u "${OPENCLAW_WORKSPACE}/${_f}" "${WORKSPACE_DIR}/"
    fi
done

if [ -f "${OPENCLAW_WORKSPACE}/USER.md" ]; then
    cp -u "${OPENCLAW_WORKSPACE}/USER.md" "${WORKSPACE_DIR}/PROFILE.md"
    log "  Synced USER.md -> PROFILE.md (if newer)"
fi
if [ -f "${OPENCLAW_WORKSPACE}/MEMORY.md" ]; then
    cp -u "${OPENCLAW_WORKSPACE}/MEMORY.md" "${WORKSPACE_DIR}/"
    log "  Synced MEMORY.md (if newer)"
fi

# ============================================================
# 4. Sync memory/ and skills/ (OpenClaw layout -> CoPaw)
# ============================================================
log "Syncing memory/ and skills/ (cp -ru: recursive, do not overwrite newer dest)..."
if [ -d "${OPENCLAW_WORKSPACE}/memory" ]; then
    mkdir -p "${WORKSPACE_DIR}/memory"
    cp -ru "${OPENCLAW_WORKSPACE}/memory/." "${WORKSPACE_DIR}/memory/" 2>/dev/null || true
    log "  Synced memory/ -> workspaces/default/memory/"
fi
if [ -d "${OPENCLAW_WORKSPACE}/skills" ]; then
    mkdir -p "${WORKSPACE_DIR}/active_skills"
    cp -ru "${OPENCLAW_WORKSPACE}/skills/." "${WORKSPACE_DIR}/active_skills/" 2>/dev/null || true
    log "  Synced skills/ -> workspaces/default/active_skills/"
fi

# ============================================================
# 5. DM room detection and auto-reply config (patches agent.json directly)
# ============================================================
# nio room.users is always 0 after token restore, so all rooms are treated as
# "group" (requireMention=true by default). We detect actual DM rooms via
# Matrix API and mark them as autoReply so they behave like OpenClaw DMs.
#
# Both the access_token we need and the groups map we patch now live in
# agent.json (config.json has been removed from the bridge contract).
log "Detecting DM rooms for auto-reply config..."
AGENT_JSON="${WORKSPACE_DIR}/agent.json"
if [ ! -f "${AGENT_JSON}" ]; then
    log "ERROR: agent.json not found at ${AGENT_JSON} (bridge step must have failed)"
    exit 1
fi
MANAGER_MATRIX_TOKEN_VAL=$(jq -r '.channels.matrix.access_token // ""' "${AGENT_JSON}")
DM_ROOMS_FILE=$(mktemp)
echo '{}' > "${DM_ROOMS_FILE}"
MATRIX_API="http://127.0.0.1:6167"
if [ -n "${MANAGER_MATRIX_TOKEN_VAL}" ] && [ "${MANAGER_MATRIX_TOKEN_VAL}" != "null" ]; then
    # Retry DM room detection in case Tuwunel is not ready yet
    _max_retries=5
    _retry=0
    JOINED_ROOMS=""
    while [ $_retry -lt $_max_retries ]; do
        JOINED_ROOMS=$(curl -sf "${MATRIX_API}/_matrix/client/v3/joined_rooms" \
            -H "Authorization: Bearer ${MANAGER_MATRIX_TOKEN_VAL}" 2>/dev/null \
            | jq -r '.joined_rooms[]' 2>/dev/null)
        if [ -n "${JOINED_ROOMS}" ]; then
            break
        fi
        _retry=$((_retry + 1))
        if [ $_retry -lt $_max_retries ]; then
            log "Retrying DM room detection ($_retry/$_max_retries)..."
            sleep 3
        fi
    done
    if [ -z "${JOINED_ROOMS}" ]; then
        log "WARNING: Could not fetch joined rooms after ${_max_retries} retries (Tuwunel may not be ready)"
    else
        while IFS= read -r ROOM_ID; do
            MEMBER_COUNT=$(curl -sf "${MATRIX_API}/_matrix/client/v3/rooms/${ROOM_ID}/members?membership=join" \
                -H "Authorization: Bearer ${MANAGER_MATRIX_TOKEN_VAL}" 2>/dev/null \
                | jq '[.chunk[] | select(.content.membership=="join")] | length' 2>/dev/null || echo "0")
            if [ "${MEMBER_COUNT}" = "2" ]; then
                jq --arg r "${ROOM_ID}" '. + {($r): {"requireMention": false, "autoReply": true}}' \
                    "${DM_ROOMS_FILE}" > "${DM_ROOMS_FILE}.tmp" && mv "${DM_ROOMS_FILE}.tmp" "${DM_ROOMS_FILE}"
                log "  DM room: ${ROOM_ID} (${MEMBER_COUNT} members, autoReply)"
            fi
        done <<< "${JOINED_ROOMS}"
    fi
fi

# Merge detected DM rooms into agent.json's channels.matrix.groups.
# Existing entries are preserved; newly detected rooms are added.
jq --slurpfile dm_rooms "${DM_ROOMS_FILE}" \
   '.channels.matrix.groups = ((.channels.matrix.groups // {}) + $dm_rooms[0])' \
   "${AGENT_JSON}" > "${AGENT_JSON}.tmp" && mv "${AGENT_JSON}.tmp" "${AGENT_JSON}"
rm -f "${DM_ROOMS_FILE}" "${DM_ROOMS_FILE}.tmp"

# ============================================================
# 8. Configure CoPaw CMS plugin (LoongSuite observability)
# ============================================================
CMS_TRACES_ENABLED="$(echo "${AGENTTEAMS_CMS_TRACES_ENABLED:-false}" | tr '[:upper:]' '[:lower:]')"
if [ "${CMS_TRACES_ENABLED}" = "true" ]; then
    log "Configuring CoPaw CMS plugin..."

    # Create bootstrap config directory
    BOOTSTRAP_CONFIG_DIR="${HOME}/.loongsuite"
    mkdir -p "${BOOTSTRAP_CONFIG_DIR}"
    BOOTSTRAP_CONFIG="${BOOTSTRAP_CONFIG_DIR}/bootstrap-config.json"

    # Generate bootstrap-config.json
    python3 - "${BOOTSTRAP_CONFIG}" <<'PYEOF'
import json
import sys
import os
from pathlib import Path

cfg_path = Path(sys.argv[1])
endpoint = os.getenv("AGENTTEAMS_CMS_ENDPOINT", "")
license_key = os.getenv("AGENTTEAMS_CMS_LICENSE_KEY", "")
arms_project = os.getenv("AGENTTEAMS_CMS_PROJECT", "")
cms_workspace = os.getenv("AGENTTEAMS_CMS_WORKSPACE", "")
service_name = os.getenv("AGENTTEAMS_CMS_SERVICE_NAME", "agentteams-manager")
protocol = "http/protobuf"  # Default OTLP protocol

config = {
    "OTEL_EXPORTER_OTLP_ENDPOINT": endpoint,
    "OTEL_EXPORTER_OTLP_PROTOCOL": protocol,
    "OTEL_EXPORTER_OTLP_HEADERS": f"x-arms-license-key={license_key},x-arms-project={arms_project},x-cms-workspace={cms_workspace}",
    "OTEL_SERVICE_NAME": service_name,
    "OTEL_SEMCONV_STABILITY_OPT_IN": "http,gen_ai_latest_experimental",
    "OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT": "SPAN_AND_EVENT",
    "LOONGSUITE_PYTHON_SITE_BOOTSTRAP": "true",
}

cfg_path.parent.mkdir(parents=True, exist_ok=True)
with open(cfg_path, "w") as f:
    json.dump(config, f, indent=2)

print(f"Bootstrap config written to: {cfg_path}")
PYEOF

    log "CoPaw CMS plugin configured at ${BOOTSTRAP_CONFIG}"
fi

# ============================================================
# 9. Background: watch openclaw.json for changes and re-bridge
# ============================================================
(
    _prev_hash=$(md5sum "${OPENCLAW_JSON}" 2>/dev/null | awk '{print $1}')
    while true; do
        sleep 60
        _curr_hash=$(md5sum "${OPENCLAW_JSON}" 2>/dev/null | awk '{print $1}')
        if [ -n "${_curr_hash}" ] && [ "${_curr_hash}" != "${_prev_hash}" ]; then
            log "openclaw.json changed, re-bridging..."
            _bridge_out=$(PYTHONPATH="/opt/agentteams/copaw/src:${PYTHONPATH:-}" \
                python3 -m copaw_worker.bridge \
                    --profile manager \
                    --openclaw-json "${OPENCLAW_JSON}" \
                    --working-dir "${COPAW_WORKING_DIR}" 2>&1)
            if [ $? -eq 0 ]; then
                _prev_hash="${_curr_hash}"
                log "Re-bridge complete"
            else
                log "Re-bridge failed, will retry on next cycle: ${_bridge_out}"
            fi
        fi
    done
) &
log "openclaw.json watcher started (PID: $!)"

# ============================================================
# 10. Launch CoPaw Manager (app mode with hot-reload)
# ============================================================
export COPAW_WORKING_DIR="${COPAW_WORKING_DIR}"

log "Starting CoPaw Manager (app mode)..."
COPAW_LOG_LEVEL="${COPAW_LOG_LEVEL:-info}"
export COPAW_LOG_LEVEL

# Set PYTHONPATH to include copaw_worker module
export PYTHONPATH="/opt/agentteams/copaw/src:${PYTHONPATH:-}"

# Use uvicorn to run CoPaw FastAPI app (enables AgentConfigWatcher for hot-reload)
# The wrapper installs AgentTeams-owned tools before CoPaw creates any agents.
exec python3 -m copaw_worker.run_copaw_app app --host 0.0.0.0 --port 18799
