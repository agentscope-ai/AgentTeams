#!/bin/bash
# start-qwenpaw-manager.sh - Start Manager Agent with QwenPaw 2.0 runtime
# Called by start-manager-agent.sh when AGENTTEAMS_MANAGER_RUNTIME=copaw|qwenpaw
#
# This script converts an OpenClaw-style workspace to a QwenPaw-style workspace
# and then launches the QwenPaw application.

source /opt/agentteams/scripts/lib/agentteams-env.sh

# ============================================================
# Path definitions
# Note: In Manager container, HOME is set to /root/manager-workspace
# ============================================================
OPENCLAW_WORKSPACE="${HOME}"
QWENPAW_WORKING_DIR="${HOME}/.qwenpaw"

# ============================================================
# 1. Create CoPaw directory structure
# ============================================================
log "Creating CoPaw directory structure..."
mkdir -p "${QWENPAW_WORKING_DIR}/custom_channels"
mkdir -p "${QWENPAW_WORKING_DIR}/.secret"

# ============================================================
# 1b. Migrate legacy .copaw state to .qwenpaw (idempotent)
# ============================================================
# MUST run BEFORE the bridge (§2) and the startup overlay steps.
# Upgrade contract (shiyiyue1102 review):
#   * Legacy user data (master_key, providers.json, envs.json, sessions,
#     memory, digest, models, custom_channels) is carried forward.
#   * Controller-owned values are re-overlaid AFTER migration by the
#     bridge (§2: Matrix access_token / user_id, providers.json), the
#     prompt sync (§3-§4), the agent.json patch (§6) and the QA disable
#     (§7) — so the current openclaw.json token and QA-disable config
#     always win.
#   * Running migration AFTER the bridge would let legacy .copaw files
#     (old token, old QA state) clobber the Controller values just
#     written — stale Matrix token, Manager cannot reply or collaborate.
# Delegate to the standalone migration script so CI can E2E-test
# the legacy CoPaw upgrade path directly (test-28-migration-e2e.sh).
# The script is copy-then-verify: it only writes .copaw-migrated
# after every critical artifact (including .copaw.secret credentials)
# is verified in the target; a partial copy is NOT marked complete
# and retries on the next startup.
export QWENPAW_WORKING_DIR="${QWENPAW_WORKING_DIR}"
export QWENPAW_SECRET_DIR="${QWENPAW_SECRET_DIR:-${QWENPAW_WORKING_DIR}.secret}"
export WORKSPACE_DIR="${WORKSPACE_DIR:-${QWENPAW_WORKING_DIR}/workspaces/default}"
bash /opt/agentteams/scripts/init/migrate-copaw-state.sh || true

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
if [ -f "${QWENPAW_WORKING_DIR}/config.json" ]; then
    if command -v jq >/dev/null 2>&1 && \
       [ "$(jq -r '.security.tool_guard.enabled // false' "${QWENPAW_WORKING_DIR}/config.json")" = "true" ] && \
       [ ! -f "${QWENPAW_WORKING_DIR}/.config-migrated-v2" ]; then
        archive="${QWENPAW_WORKING_DIR}/config.json.legacy-$(date +%Y%m%d-%H%M%S)"
        log "Archiving legacy config.json (tool_guard enabled) -> $(basename "${archive}")"
        mv "${QWENPAW_WORKING_DIR}/config.json" "${archive}"
        touch "${QWENPAW_WORKING_DIR}/.config-migrated-v2"
    fi
fi

log "Bridging openclaw.json -> CoPaw config (manager)..."
/opt/venv/qwenpaw/bin/python3 -m copaw_worker.bridge \
        --profile manager \
        --openclaw-json "${OPENCLAW_JSON}" \
        --working-dir "${QWENPAW_WORKING_DIR}"
log "Config bridged from openclaw.json"

# ============================================================
# 3. Sync prompt files into CoPaw paths
# ============================================================
# Canonical AgentTeams layout is OPENCLAW_WORKSPACE ($HOME): SOUL.md, memory/, skills/ etc.
# CoPaw reads from QWENPAW_WORKING_DIR/workspaces/default/; we sync into that path only.
# Use cp -u / cp -ru so we never overwrite newer files already in workspaces/default/.
# ============================================================
WORKSPACE_DIR="${QWENPAW_WORKING_DIR}/workspaces/default"
mkdir -p "${WORKSPACE_DIR}"
# QwenPaw's Matrix channel writes incoming attachments directly under media/
# and currently assumes the directory already exists.
mkdir -p "${WORKSPACE_DIR}/media"

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
# 5. Inject session file privacy policy into prompt files
# ============================================================
# Aligned with qwenpaw Worker (_ensure_session_file_prompt_policy):
# prevents the agent from reading/exposing files under sessions/.
SESSION_FILE_POLICY="Do not read, list, grep, glob, summarize, copy, or expose files under sessions/.
Session files are runtime-private state and may contain private conversation history.
This rule applies to all channels, users, and sessions, not only DingTalk."
SESSION_FILE_POLICY_MARKER="Session files are runtime-private state"
for _pf in AGENTS.md SOUL.md; do
    _target="${WORKSPACE_DIR}/${_pf}"
    if [ -f "${_target}" ]; then
        if ! grep -q "${SESSION_FILE_POLICY_MARKER}" "${_target}" 2>/dev/null; then
            printf '\n%s\n' "${SESSION_FILE_POLICY}" >> "${_target}"
            log "  Injected session file policy into ${_pf}"
        fi
    fi
done

# ============================================================
# 6. DM room detection and auto-reply config (patches agent.json directly)
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
    log "ERROR: agent.json not found at ${AGENT_JSON} (bridge steps must have failed)"
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
# 7. Disable built-in QA Agent (QwenPaw_QA_Agent_0.2)
# ============================================================
# Worker does this via api_client.disable_agent_if_present().
# Manager runs QwenPaw in-process (no API client), so we set
# enabled=false in config.json's agents.profiles before startup.
# QwenPaw 2.0 start_all_configured_agents() skips enabled=false agents.
CONFIG_JSON="${QWENPAW_WORKING_DIR}/config.json"
if [ -f "${CONFIG_JSON}" ]; then
    # AgentProfileRef requires id + workspace_dir (both mandatory).
    # Writing only {"enabled": false} causes a Pydantic ValidationError
    # that _remove_bad_field() strips, then ensure_qa_agent_exists()
    # recreates it with enabled=True — defeating the disable.
    #
    # CRITICAL: must also inject the "default" profile alongside QA.
    # If profiles contains ONLY the QA entry, QwenPaw's
    # migrate_legacy_workspace_to_default_agent() sees len(profiles)==1
    # and "default" not in profiles → runs legacy migration → replaces
    # config.agents entirely → QA profile lost → ensure_qa_agent_exists()
    # re-creates it with enabled=True.  Injecting "default" makes
    # len(profiles)>=2 so migration is skipped.
    _DEFAULT_WD="${QWENPAW_WORKING_DIR}/workspaces/default"
    _QA_WD="${QWENPAW_WORKING_DIR}/workspaces/QwenPaw_QA_Agent_0.2"
    jq --arg wd "${_QA_WD}" --arg dwd "${_DEFAULT_WD}" \
       '.agents.profiles = ((.agents.profiles // {}) + {
           "default": {
               "id": "default",
               "workspace_dir": $dwd
           },
           "QwenPaw_QA_Agent_0.2": {
               "id": "QwenPaw_QA_Agent_0.2",
               "workspace_dir": $wd,
               "enabled": false
           }
       })' \
        "${CONFIG_JSON}" > "${CONFIG_JSON}.tmp" && mv "${CONFIG_JSON}.tmp" "${CONFIG_JSON}"
    log "Disabled built-in QA Agent in config.json"
fi

# ============================================================
# 8. Configure CMS observability plugin (LoongSuite)
# ============================================================
# Aligned with qwenpaw Worker entrypoint: heredoc + env exports.
CMS_TRACES_ENABLED="$(echo "${AGENTTEAMS_CMS_TRACES_ENABLED:-false}" | tr '[:upper:]' '[:lower:]')"
if [ "${CMS_TRACES_ENABLED}" = "true" ]; then
    log "Configuring CMS observability plugin..."

    LOONGSUITE_DIR="${HOME}/.loongsuite"
    mkdir -p "${LOONGSUITE_DIR}"

    cat > "${LOONGSUITE_DIR}/bootstrap-config.json" <<EOF
{
  "OTEL_EXPORTER_OTLP_ENDPOINT": "${AGENTTEAMS_CMS_ENDPOINT}",
  "OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
  "OTEL_EXPORTER_OTLP_HEADERS": "x-arms-license-key=${AGENTTEAMS_CMS_LICENSE_KEY},x-arms-project=${AGENTTEAMS_CMS_PROJECT},x-cms-workspace=${AGENTTEAMS_CMS_WORKSPACE}",
  "OTEL_SERVICE_NAME": "${AGENTTEAMS_CMS_SERVICE_NAME:-agentteams-manager}",
  "OTEL_SEMCONV_STABILITY_OPT_IN": "http,gen_ai_latest_experimental",
  "OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT": "SPAN_AND_EVENT",
  "LOONGSUITE_PYTHON_SITE_BOOTSTRAP": "true",
  "LOONGSUITE_PYTHON_SITE_BOOTSTRAP_LOG_SUCCESS": "false"
}
EOF

    log "CMS observability plugin configured at ${LOONGSUITE_DIR}/bootstrap-config.json"
    export LOONGSUITE_PYTHON_SITE_BOOTSTRAP=true
    export LOONGSUITE_PYTHON_SITE_BOOTSTRAP_LOG_SUCCESS=false
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
            _bridge_out=$(/opt/venv/qwenpaw/bin/python3 -m copaw_worker.bridge \
                    --profile manager \
                    --openclaw-json "${OPENCLAW_JSON}" \
                    --working-dir "${QWENPAW_WORKING_DIR}" 2>&1)
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
# 10. Copy AgentTeams plugins into working dir
# ============================================================
# The Dockerfile copies plugins to /opt/agentteams/plugins/ (image-local).
# /root/manager-workspace is a host-mounted volume at runtime, so
# build-time files there are hidden. Copy plugins to the QwenPaw
# working dir's plugins/ directory before startup so PluginLoader
# discovers them.
PLUGINS_TARGET="${QWENPAW_WORKING_DIR}/plugins"
mkdir -p "${PLUGINS_TARGET}"
for _plugin_src in /opt/agentteams/plugins/*/; do
    _plugin_name=$(basename "${_plugin_src}")
    if [ -f "${_plugin_src}plugin.json" ]; then
        rm -rf "${PLUGINS_TARGET}/${_plugin_name}"
        cp -a "${_plugin_src}" "${PLUGINS_TARGET}/${_plugin_name}"
        log "Installed plugin: ${_plugin_name}"
    fi
done

# ============================================================
export QWENPAW_WORKING_DIR="${QWENPAW_WORKING_DIR}"
export QWENPAW_SECRET_DIR="${QWENPAW_SECRET_DIR:-${QWENPAW_WORKING_DIR}.secret}"
export QWENPAW_RUNNING_IN_CONTAINER=true
export QWENPAW_LOG_LEVEL="${COPAW_LOG_LEVEL:-info}"

# YOLO mode: AGENTTEAMS_YOLO=1 → set approval_level=OFF in agent.json
# (QwenPaw 2.0 equivalent of OpenClaw's tools.exec.ask=off).
# start-manager-agent.sh promotes the yolo-mode marker file to
# AGENTTEAMS_YOLO=1 before calling this script.
if [ "${AGENTTEAMS_YOLO:-}" = "1" ] && [ -f "${AGENT_JSON}" ]; then
    jq '.approval_level = "OFF"' "${AGENT_JSON}" > "${AGENT_JSON}.tmp" \
        && mv "${AGENT_JSON}.tmp" "${AGENT_JSON}"
    log "YOLO mode: approval_level set to OFF"
fi

log "Starting QwenPaw 2.0 Manager (app mode)..."

# Keep canonical Manager skills under $HOME/skills synchronized with the
# QwenPaw native workspace after startup. QwenPaw does not watch the OpenClaw
# layout itself, so merely adding a SKILL.md there would otherwise require a
# Manager restart. The sync process waits for the API, refreshes the scanner,
# and enables newly added or updated skills.
python3 /opt/agentteams/scripts/init/qwenpaw_manager_skill_sync.py \
    --source-dir "${OPENCLAW_WORKSPACE}/skills" \
    --workspace-dir "${WORKSPACE_DIR}" \
    --state-file "${QWENPAW_WORKING_DIR}/agentteams-manager-skills.json" \
    --interval "${AGENTTEAMS_QWENPAW_SKILL_SYNC_INTERVAL_SECONDS:-1}" &
log "QwenPaw Manager skill watcher started (PID: $!)"

# run_copaw_app.py starts qwenpaw app (tools registered via agentteams-manager-tools plugin)
exec python3 -m copaw_worker.run_copaw_app app --host 0.0.0.0 --port 18799
