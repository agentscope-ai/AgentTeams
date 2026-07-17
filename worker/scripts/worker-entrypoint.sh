#!/bin/bash
# worker-entrypoint.sh - Worker Agent startup
# Pulls config from centralized file system, starts file sync, launches OpenClaw.
#
# HOME is set to the Worker workspace so all agent-generated files are synced to MinIO:
#   ~/ = /root/hiclaw-fs/agents/<WORKER_NAME>/  (SOUL.md, openclaw.json, memory/)
#   /root/hiclaw-fs/shared/                     = Shared tasks, knowledge, collaboration data

set -e
source /opt/hiclaw/scripts/lib/hiclaw-env.sh

WORKER_NAME="${AGENTTEAMS_WORKER_NAME:?AGENTTEAMS_WORKER_NAME is required}"
FS_ENDPOINT="${AGENTTEAMS_FS_ENDPOINT:-}"
FS_ACCESS_KEY="${AGENTTEAMS_FS_ACCESS_KEY:-}"
FS_SECRET_KEY="${AGENTTEAMS_FS_SECRET_KEY:-}"

log() {
    echo "[agentteams-worker $(date '+%Y-%m-%d %H:%M:%S')] $1"
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
AGENTTEAMS_ROOT="/root/hiclaw-fs"
WORKSPACE="${AGENTTEAMS_ROOT}/agents/${WORKER_NAME}"

# ============================================================
# Step 1: Configure mc alias for centralized file system
# ============================================================
if ensure_mc_credentials && agentteams_mc_host_configured; then
    log "Configuring mc alias via controller-issued storage credentials (${AGENTTEAMS_STORAGE_ALIAS})..."
else
    if [ "${AGENTTEAMS_STORAGE_PROVIDER:-minio}" = "oss" ]; then
        log "ERROR: OSS storage requires controller-issued storage credentials, but $(agentteams_mc_host_var) is not configured"
        exit 1
    fi
    log "Configuring mc alias for static storage credentials (${AGENTTEAMS_STORAGE_ALIAS})..."
    mc alias set "${AGENTTEAMS_STORAGE_ALIAS}" "${FS_ENDPOINT:?AGENTTEAMS_FS_ENDPOINT is required}" \
        "${FS_ACCESS_KEY:?AGENTTEAMS_FS_ACCESS_KEY is required}" \
        "${FS_SECRET_KEY:?AGENTTEAMS_FS_SECRET_KEY is required}"
fi

# ============================================================
# Step 2: Pull Worker config and shared data from centralized storage
# ============================================================
mkdir -p "${WORKSPACE}" "${AGENTTEAMS_ROOT}/shared"

log "Pulling Worker config from centralized storage..."
ensure_mc_credentials 2>/dev/null || true
RETRY=0
until mc mirror "${AGENTTEAMS_STORAGE_PREFIX}/agents/${WORKER_NAME}/" "${WORKSPACE}/" --overwrite \
    --exclude ".openclaw/matrix/**" --exclude ".openclaw/canvas/**" --exclude "credentials/**"; do
    RETRY=$((RETRY + 1))
    if [ "${RETRY}" -gt 6 ]; then
        log "ERROR: failed to pull Worker config from MinIO after retries"
        exit 1
    fi
    log "Waiting for Worker config prefix in MinIO (attempt ${RETRY}/6)..."
    sleep 5
done
mc mirror "${AGENTTEAMS_STORAGE_PREFIX}/shared/" "${AGENTTEAMS_ROOT}/shared/" --overwrite 2>/dev/null || true

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
    mc mirror "${AGENTTEAMS_STORAGE_PREFIX}/agents/${WORKER_NAME}/" "${WORKSPACE}/" --overwrite \
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
# Step 3: Start file sync (background daemon)
# ============================================================
python3 -m agentteams_sync daemon --contract=openclaw &
log "Background sync daemon started (PID: $!, contract=openclaw)"

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

# Matrix E2EE crypto wipe + re-login (O13.5 — agentteams_sync openclaw-matrix CLI)
python3 -m agentteams_sync openclaw-matrix --contract=openclaw

# Disable full-process respawn so the CLI uses its internal restart loop.
# Without this, config reload spawns a detached child and exits, killing the container.
export OPENCLAW_NO_RESPAWN=1

# Optional matrix-plugin trace logging — when AGENTTEAMS_MATRIX_DEBUG=1 is set in
# the worker environment (propagated by the controller / install script), turn
# on OPENCLAW_MATRIX_DEBUG so the matrix plugin emits structured INFO-level
# lifecycle traces (sync.state transitions, room.invite/join, message handler
# arrival + filter outcomes). Useful when diagnosing "worker never joined the
# room" / "manager never replied" hangs without rebuilding the image.
if [ "${AGENTTEAMS_MATRIX_DEBUG:-}" = "1" ] && [ -z "${OPENCLAW_MATRIX_DEBUG:-}" ]; then
    export OPENCLAW_MATRIX_DEBUG=1
    log "AGENTTEAMS_MATRIX_DEBUG=1 detected; OPENCLAW_MATRIX_DEBUG=1 exported for matrix plugin tracing"
fi

# ============================================================
# Step 5c: Background readiness reporter
# ============================================================
# Wait for local gateway health, then report ready via hiclaw CLI.
if [ -n "${AGENTTEAMS_CONTROLLER_URL:-}" ]; then
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
        hiclaw worker report-ready --name "${AGENTTEAMS_WORKER_CR_NAME:-${WORKER_NAME}}"
    ) &
    log "Background readiness reporter started (PID: $!)"
fi

# Disable openclaw's observe-recovery to prevent stale baseline from overwriting
# user-customized openclaw.json on gateway restart. .bak is preserved as backup.
rm -f "${HOME}/.openclaw/logs/config-health.json" 2>/dev/null || true

exec openclaw gateway run --verbose --force
