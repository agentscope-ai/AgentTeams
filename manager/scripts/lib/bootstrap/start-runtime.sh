#!/bin/bash
# bootstrap/start-runtime.sh - OpenClaw vs CoPaw Manager process launch

bootstrap_start_manager_runtime() {
    if [ "${MANAGER_RUNTIME}" = "copaw" ]; then
        exec /opt/hiclaw/scripts/init/start-copaw-manager.sh
    fi

    log "Starting OpenClaw Manager..."

    export OPENCLAW_CONFIG_PATH="/root/manager-workspace/openclaw.json"

    mkdir -p "${HOME}/.openclaw"
    ln -sf "/root/manager-workspace/openclaw.json" "${HOME}/.openclaw/openclaw.json"

    find "${HOME}/.openclaw/agents" -name "*.jsonl.lock" -delete 2>/dev/null || true
    log "Cleaned up any orphaned session write locks"

    rm -rf "${HOME}/.openclaw/matrix" 2>/dev/null || true
    log "Cleaned Matrix crypto storage (will re-establish E2EE sessions)"

    export OPENCLAW_NO_RESPAWN=1

    if [ "${AGENTTEAMS_MATRIX_DEBUG:-}" = "1" ] && [ -z "${OPENCLAW_MATRIX_DEBUG:-}" ]; then
        export OPENCLAW_MATRIX_DEBUG=1
        log "AGENTTEAMS_MATRIX_DEBUG=1 detected; OPENCLAW_MATRIX_DEBUG=1 exported for matrix plugin tracing"
    fi

    exec openclaw gateway run --verbose --force
}
