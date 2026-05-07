#!/bin/bash
# worker-entrypoint.sh - Worker Agent startup
# Pulls config from centralized file system, starts file sync, launches OpenClaw.
#
# HOME is set to the Worker workspace so all agent-generated files are synced to MinIO:
#   ~/ = /root/hiclaw-fs/agents/<WORKER_NAME>/  (SOUL.md, openclaw.json, memory/)
#   /root/hiclaw-fs/shared/                     = Shared tasks, knowledge, collaboration data

set -e
source /opt/hiclaw/scripts/lib/hiclaw-env.sh
source /opt/hiclaw/scripts/lib/merge-openclaw-config.sh

WORKER_NAME="${HICLAW_WORKER_NAME:?HICLAW_WORKER_NAME is required}"
FS_ENDPOINT="${HICLAW_FS_ENDPOINT:-}"
FS_ACCESS_KEY="${HICLAW_FS_ACCESS_KEY:-}"
FS_SECRET_KEY="${HICLAW_FS_SECRET_KEY:-}"

log() {
    echo "[hiclaw-worker $(date '+%Y-%m-%d %H:%M:%S')] $1"
}

# ============================================================
# Step 0: Set timezone from TZ env var
# ============================================================
if [ -n "${TZ}" ] && [ -f "/usr/share/zoneinfo/${TZ}" ]; then
    ln -sf "/usr/share/zoneinfo/${TZ}" /etc/localtime
    echo "${TZ}" > /etc/timezone
    log "Timezone set to ${TZ}"
fi

# Use absolute path because HOME is set to the workspace directory via docker run
HICLAW_ROOT="/root/hiclaw-fs"
WORKSPACE="${HICLAW_ROOT}/agents/${WORKER_NAME}"

# ============================================================
# Step 1: Configure mc alias for centralized file system
# ============================================================
if [ "${HICLAW_RUNTIME}" = "aliyun" ]; then
    log "Configuring mc alias for cloud (RRSA OIDC)..."
    ensure_mc_credentials
else
    log "Configuring mc alias for local MinIO..."
    mc alias set hiclaw "${FS_ENDPOINT:?HICLAW_FS_ENDPOINT is required}" \
        "${FS_ACCESS_KEY:?HICLAW_FS_ACCESS_KEY is required}" \
        "${FS_SECRET_KEY:?HICLAW_FS_SECRET_KEY is required}"
fi

# ============================================================
# Step 2: Pull Worker config and shared data from centralized storage
# ============================================================
mkdir -p "${WORKSPACE}" "${HICLAW_ROOT}/shared"

log "Pulling Worker config from centralized storage..."
ensure_mc_credentials 2>/dev/null || true
mc mirror "${HICLAW_STORAGE_PREFIX}/agents/${WORKER_NAME}/" "${WORKSPACE}/" --overwrite \
    --exclude ".openclaw/matrix/**" --exclude ".openclaw/canvas/**" --exclude "credentials/**"
mc mirror "${HICLAW_STORAGE_PREFIX}/shared/" "${HICLAW_ROOT}/shared/" --overwrite 2>/dev/null || true

# Mark pull completion — the local→remote sync loop uses this marker to avoid
# pushing back files that were just pulled (their mtime is fresh from the pull).
PULL_MARKER="${WORKSPACE}/.last-pull"
touch "${PULL_MARKER}"

# Verify essential files exist, retry if sync is still in progress
RETRY=0
while [ ! -f "${WORKSPACE}/openclaw.json" ] || [ ! -f "${WORKSPACE}/SOUL.md" ] \
      || [ ! -f "${WORKSPACE}/AGENTS.md" ]; do
    RETRY=$((RETRY + 1))
    if [ "${RETRY}" -gt 6 ]; then
        log "ERROR: openclaw.json, SOUL.md or AGENTS.md not found after retries. Manager may not have created this Worker's config yet."
        exit 1
    fi
    log "Waiting for config files to appear in MinIO (attempt ${RETRY}/6)..."
    sleep 5
    mc mirror "${HICLAW_STORAGE_PREFIX}/agents/${WORKER_NAME}/" "${WORKSPACE}/" --overwrite \
        --exclude ".openclaw/matrix/**" --exclude ".openclaw/canvas/**" --exclude "credentials/**" 2>/dev/null || true
    touch "${PULL_MARKER}"
done

# HOME is already set to WORKSPACE via docker run -e HOME=...
# Symlink to default OpenClaw config path so CLI commands find the config
mkdir -p "${HOME}/.openclaw"
ln -sf "${WORKSPACE}/openclaw.json" "${HOME}/.openclaw/openclaw.json"

# Create symlink for skills CLI: ~/.agents/skills -> ~/skills
# This makes `skills add -g` install skills directly into ~/skills/ (same as file-sync)
# Skills in ~/skills/ will be synced to MinIO and persist across container restarts
mkdir -p "${HOME}/skills"
mkdir -p "${HOME}/.agents"
# Clean up circular symlink from previous buggy ln -sf (which followed
# the existing symlink-to-directory and created skills/skills -> skills inside it).
[ -L "${HOME}/skills/skills" ] && rm -f "${HOME}/skills/skills"
# Use -n (--no-dereference) so ln replaces an existing symlink-to-directory
# instead of creating a nested symlink inside the target directory.
ln -sfn "${HOME}/skills" "${HOME}/.agents/skills"

log "Worker config pulled successfully"

# ============================================================
# Optional: ensure diagnostics-otel npm dependencies are present
# When CMS metrics are enabled, generate-worker-config.sh injects
# diagnostics-otel into openclaw.json.  The plugin ships with
# openclaw-base but node_modules may be absent on first run.
# ============================================================
_diag_plugin_dir="/opt/openclaw/extensions/diagnostics-otel"
if [ -f "${_diag_plugin_dir}/package.json" ] && \
   jq -e --arg dir "${_diag_plugin_dir}" \
        '(.plugins.load.paths // []) | index($dir) != null' \
        "${WORKSPACE}/openclaw.json" > /dev/null 2>&1; then
    if [ ! -d "${_diag_plugin_dir}/node_modules" ]; then
        log "diagnostics-otel: installing npm dependencies (required for metrics)..."
        if (cd "${_diag_plugin_dir}" && npm install --omit=dev --ignore-scripts >/tmp/hiclaw-diag-install.log 2>&1); then
            log "diagnostics-otel dependencies installed"
        else
            log "WARNING: diagnostics-otel npm install failed; metrics may not be reported (see /tmp/hiclaw-diag-install.log)"
        fi
    else
        log "diagnostics-otel dependencies already present"
    fi
fi
unset _diag_plugin_dir

# Restore skills from MinIO if skills directory is empty but skills-lock.json exists
if [ -f "${WORKSPACE}/skills-lock.json" ] && [ -z "$(ls -A ${WORKSPACE}/skills 2>/dev/null | grep -v file-sync)" ]; then
    log "Found skills-lock.json but skills directory is empty, restoring skills..."
    cd "${WORKSPACE}" && skills experimental_install -y 2>/dev/null || log "Warning: skills restore failed, will need to reinstall"
fi

# Ensure hiclaw-sync wrapper is functional
# Use /bin/sh to invoke the script so it works even without +x permission
# (MinIO object storage does not preserve Unix permission bits)
printf '#!/bin/bash\nexec /bin/sh "%s/skills/file-sync/scripts/hiclaw-sync.sh" "$@"\n' \
    "${WORKSPACE}" > /usr/local/bin/hiclaw-sync
chmod +x /usr/local/bin/hiclaw-sync

# Defensive symlink: /opt/hiclaw/agent/skills -> actual skills directory
mkdir -p /opt/hiclaw/agent
ln -sfn "${WORKSPACE}/skills" /opt/hiclaw/agent/skills

log "HOME set to ${HOME} (workspace files will be synced to MinIO)"

# ============================================================
# Step 3: Start file sync
# ============================================================
#
# ── File Sync Design Principle ──────────────────────────────────────────────
#
#   The party that writes a file is responsible for:
#     1. Pushing it to MinIO immediately (Local -> Remote)
#     2. Notifying the other side via Matrix @mention so they can pull on demand
#
#   Local -> Remote: change-triggered push of Worker-managed content
#     - Uses find to detect files modified after the last pull; only runs mc mirror when needed
#     - Avoids mc mirror --watch TOCTOU bug (crashes on atomic ops like npm install)
#     - The bulk mirror excludes openclaw.json (local-first field merge; see merge-openclaw-config.sh),
#       SOUL.md/AGENTS.md/HEARTBEAT.md (handled by the per-file loop below
#       with an mtime guard), and various caches.
#     - The per-file `mc cp`-if-newer loop pushes SOUL.md/AGENTS.md/HEARTBEAT.md
#       only when the local copy was modified after the last pull. This lets
#       the agent persist its own self-edits (HEARTBEAT.md checklist tweaks,
#       SOUL.md "personality evolution") without pushing back the unmodified
#       package content that was just pulled. mc mirror is run before the
#       touch ${PULL_MARKER} on every pull path, so package content always
#       has mtime <= PULL_MARKER and the -nt check stays false until the
#       agent itself writes.
#
#   Remote -> Local: on-demand pull via file-sync skill (triggered by Manager @mention)
#     + 5-minute fallback pull of Manager-managed paths as safety net
#       The fallback refreshes ${PULL_MARKER} so the change-triggered loop
#       does not misinterpret freshly-pulled openclaw.json/skills mtimes as
#       agent edits and spin forever on no-op pushes.
#
# ────────────────────────────────────────────────────────────────────────────
(
    while true; do
        # Only push files modified AFTER the last pull (avoids pushing back freshly-pulled files)
        CHANGED=$(find "${WORKSPACE}/" -type f -newer "${PULL_MARKER}" 2>/dev/null | head -1)
        if [ -n "${CHANGED}" ]; then
            ensure_mc_credentials 2>/dev/null || true
            if ! mc mirror "${WORKSPACE}/" "${HICLAW_STORAGE_PREFIX}/agents/${WORKER_NAME}/" --overwrite \
                --exclude "openclaw.json" \
                --exclude "config/mcporter.json" --exclude "mcporter-servers.json" --exclude ".agents/**" \
                --exclude "credentials/**" \
                --exclude ".cache/**" --exclude ".npm/**" \
                --exclude ".local/**" --exclude ".mc/**" --exclude "*.lock" \
                --exclude ".last-pull" \
                --exclude ".openclaw/matrix/**" --exclude ".openclaw/canvas/**" \
                --exclude "SOUL.md" --exclude "AGENTS.md" --exclude "HEARTBEAT.md" 2>&1; then
                log "WARNING: Local->Remote sync failed"
            fi
            # Per-file push for agent-self-modifiable files: only when locally
            # modified after the last pull. See block comment above for design.
            for _mf in SOUL.md AGENTS.md HEARTBEAT.md; do
                if [ -f "${WORKSPACE}/${_mf}" ] && [ "${WORKSPACE}/${_mf}" -nt "${PULL_MARKER}" ]; then
                    mc cp "${WORKSPACE}/${_mf}" "${HICLAW_STORAGE_PREFIX}/agents/${WORKER_NAME}/${_mf}" 2>/dev/null || true
                fi
            done
        fi
        sleep 5
    done
) &
log "Local->Remote change-triggered sync started (PID: $!)"

# Remote -> Local: fallback pull of Manager-managed files (safety net, every 5m)
# Normal operation relies on on-demand pulls via file-sync skill when Manager @mentions.
# openclaw.json uses local-first merge (see merge-openclaw-config.sh): existing
# workspace config is the base; MinIO only overlays models, gateway, channels, plugins rules.
(
    while true; do
        sleep 300
        ensure_mc_credentials 2>/dev/null || true
        mc cp "${HICLAW_STORAGE_PREFIX}/agents/${WORKER_NAME}/openclaw.json" /tmp/openclaw-remote.json 2>/dev/null || true
        merge_openclaw_config /tmp/openclaw-remote.json "${WORKSPACE}/openclaw.json"
        rm -f /tmp/openclaw-remote.json
        mc cp "${HICLAW_STORAGE_PREFIX}/agents/${WORKER_NAME}/config/mcporter.json" "${WORKSPACE}/config/mcporter.json" 2>/dev/null || true
        mc mirror "${HICLAW_STORAGE_PREFIX}/agents/${WORKER_NAME}/skills/" "${WORKSPACE}/skills/" --overwrite 2>/dev/null || true
        find "${WORKSPACE}/skills" -name '*.sh' -exec chmod +x {} + 2>/dev/null || true
        mc mirror "${HICLAW_STORAGE_PREFIX}/shared/" "${HICLAW_ROOT}/shared/" --overwrite --newer-than "5m" 2>/dev/null || true
        # Refresh PULL_MARKER so the change-triggered push loop doesn't
        # re-trigger forever on freshly-pulled openclaw.json/skills mtimes,
        # and so the per-file -nt guard correctly classifies post-pull edits.
        touch "${PULL_MARKER}"
    done
) &
log "Remote->Local fallback sync started (Manager-managed files only, every 5m, PID: $!)"

# ============================================================
# Step 4: Configure mcporter (MCP tool CLI)
# Config at ./config/mcporter.json (mcporter default path, no --config needed)
# Symlink at ~/mcporter-servers.json for backward compatibility
# The file may not exist at startup but will appear when Manager
# configures MCP servers and Worker runs file-sync.
# ============================================================
MCPORTER_DEFAULT="${WORKSPACE}/config/mcporter.json"
MCPORTER_COMPAT="${WORKSPACE}/mcporter-servers.json"
mkdir -p "${WORKSPACE}/config"
if [ -f "${MCPORTER_DEFAULT}" ]; then
    log "mcporter configured: ${MCPORTER_DEFAULT}"
elif [ -f "${MCPORTER_COMPAT}" ] && [ ! -L "${MCPORTER_COMPAT}" ]; then
    # Migrate legacy mcporter-servers.json to new default path
    mv "${MCPORTER_COMPAT}" "${MCPORTER_DEFAULT}"
    log "mcporter config migrated to ${MCPORTER_DEFAULT}"
else
    log "mcporter config not yet available (will be pulled via file-sync when MCP servers are configured)"
fi
# Backward-compatible symlink (always recreate to ensure correctness)
ln -sfn "${MCPORTER_DEFAULT}" "${MCPORTER_COMPAT}"
# Keep MCPORTER_CONFIG for any scripts that still reference it
export MCPORTER_CONFIG="${MCPORTER_DEFAULT}"

# ============================================================
# Step 5: Launch OpenClaw Worker Agent
# ============================================================
log "Starting Worker Agent: ${WORKER_NAME}"
export OPENCLAW_CONFIG_PATH="${WORKSPACE}/openclaw.json"
cd "${WORKSPACE}"

# Clean orphaned session write locks (e.g. from SIGKILL or crash before exit handlers)
# Prevents "session file locked (timeout 10000ms)" when PID was reused
find "${HOME}/.openclaw/agents" -name "*.jsonl.lock" -delete 2>/dev/null || true
log "Cleaned up any orphaned session write locks"

# Clean Matrix crypto storage (SQLite WAL may be corrupted after unclean shutdown)
# Crypto state is re-negotiated on startup; losing it only means re-establishing E2EE sessions
rm -rf "${HOME}/.openclaw/matrix" 2>/dev/null || true
log "Cleaned Matrix crypto storage (will re-establish E2EE sessions)"

# ============================================================
# Step 5b: Re-login to Matrix to get fresh access token + device ID
# ============================================================
# Under E2EE, reusing the old access token (same device_id) with a new
# identity key (crypto storage was just wiped) causes other clients to
# reject key distribution. Re-login creates a new device_id, matching
# the Manager's behavior and allowing clean E2EE session establishment.
MATRIX_PASSWORD_FILE="${HICLAW_STORAGE_PREFIX}/agents/${WORKER_NAME}/credentials/matrix/password"
MATRIX_PASSWORD=$(mc cat "${MATRIX_PASSWORD_FILE}" 2>/dev/null) || true
if [ -n "${MATRIX_PASSWORD}" ]; then
    # Read homeserver URL from openclaw.json (already pulled from MinIO)
    MATRIX_SERVER=$(jq -r '.channels.matrix.homeserver // empty' "${WORKSPACE}/openclaw.json" 2>/dev/null)

    if [ -n "${MATRIX_SERVER}" ]; then
        log "Re-logging into Matrix to get fresh access token and device ID..."
        LOGIN_RESP=$(curl -sf -X POST "${MATRIX_SERVER}/_matrix/client/v3/login" \
            -H 'Content-Type: application/json' \
            -d '{
                "type": "m.login.password",
                "identifier": {"type": "m.id.user", "user": "'"${WORKER_NAME}"'"},
                "password": "'"${MATRIX_PASSWORD}"'"
            }' 2>/dev/null) || true

        NEW_TOKEN=$(echo "${LOGIN_RESP}" | jq -r '.access_token // empty' 2>/dev/null)
        NEW_DEVICE=$(echo "${LOGIN_RESP}" | jq -r '.device_id // empty' 2>/dev/null)

        if [ -n "${NEW_TOKEN}" ] && [ "${NEW_TOKEN}" != "null" ]; then
            # Update openclaw.json with the fresh token
            jq --arg token "${NEW_TOKEN}" '.channels.matrix.accessToken = $token' \
                "${WORKSPACE}/openclaw.json" > /tmp/openclaw-relogin.json \
                && mv /tmp/openclaw-relogin.json "${WORKSPACE}/openclaw.json"
            log "Matrix re-login successful (new device: ${NEW_DEVICE}, token prefix: ${NEW_TOKEN:0:10}...)"
        else
            log "WARNING: Matrix re-login failed, using existing access token (E2EE may not work with Element Web)"
            log "  Response: ${LOGIN_RESP}"
        fi
    else
        log "WARNING: Missing homeserver URL in openclaw.json, skipping Matrix re-login"
    fi
    # Clear password from memory
    MATRIX_PASSWORD=""
else
    log "No Matrix password found in MinIO, skipping re-login (E2EE may not work after restart)"
fi

# Disable full-process respawn so the CLI uses its internal restart loop.
# Without this, config reload spawns a detached child and exits, killing the container.
export OPENCLAW_NO_RESPAWN=1

# Optional matrix-plugin trace logging — when HICLAW_MATRIX_DEBUG=1 is set in
# the worker environment (propagated by the controller / install script), turn
# on OPENCLAW_MATRIX_DEBUG so the matrix plugin emits structured INFO-level
# lifecycle traces (sync.state transitions, room.invite/join, message handler
# arrival + filter outcomes). Useful when diagnosing "worker never joined the
# room" / "manager never replied" hangs without rebuilding the image.
if [ "${HICLAW_MATRIX_DEBUG:-}" = "1" ] && [ -z "${OPENCLAW_MATRIX_DEBUG:-}" ]; then
    export OPENCLAW_MATRIX_DEBUG=1
    log "HICLAW_MATRIX_DEBUG=1 detected; OPENCLAW_MATRIX_DEBUG=1 exported for matrix plugin tracing"
fi

# ============================================================
# Step 5c: Background readiness reporter
# ============================================================
# Wait for local gateway health, then report ready via hiclaw CLI.
if [ -n "${HICLAW_CONTROLLER_URL:-}" ]; then
(
        # Phase 1: Wait for gateway to be healthy (with timeout)
        TIMEOUT=120; ELAPSED=0
        while [ "${ELAPSED}" -lt "${TIMEOUT}" ]; do
            if openclaw gateway health --json 2>/dev/null | grep -q '"ok"' 2>/dev/null; then
                break
            fi
            sleep 5; ELAPSED=$((ELAPSED + 5))
        done

        if [ "${ELAPSED}" -ge "${TIMEOUT}" ]; then
            log "WARNING: readiness reporter timed out waiting for gateway after ${TIMEOUT}s"
            exit 1
        fi

        # Report ready to controller via hiclaw CLI
        hiclaw worker report-ready
    ) &
    log "Background readiness reporter started (PID: $!)"
fi

# Disable openclaw's observe-recovery to prevent stale baseline from overwriting
# user-customized openclaw.json on gateway restart. .bak is preserved as backup.
rm -f "${HOME}/.openclaw/logs/config-health.json" 2>/dev/null || true

exec openclaw gateway run --verbose --force
