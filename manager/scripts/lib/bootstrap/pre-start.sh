#!/bin/bash
# bootstrap/pre-start.sh - Render agent docs and optional mcporter auto-setup

bootstrap_pre_start() {
    log "Starting Manager Agent (${MANAGER_RUNTIME})..."

    cd "${HOME}"

    if [ -d "/host-share" ]; then
        [ -f "/host-share/.gitconfig" ] && ln -sf "/host-share/.gitconfig" "${HOME}/.gitconfig"
    fi

    log "HOME=${HOME} (manager-workspace, host-mounted)"

    export MANAGER_MATRIX_TOKEN MANAGER_TOKEN HIGRESS_COOKIE_FILE
    local RENDER=/opt/hiclaw/scripts/lib/render-skills.sh
    log "Rendering agent doc templates..."
    bash "$RENDER" /root/manager-workspace/skills
    bash "$RENDER" /root/manager-workspace/skills-alpha
    bash "$RENDER" /root/manager-workspace AGENTS.md TOOLS.md HEARTBEAT.md SOUL.md
    bash "$RENDER" /root/manager-workspace/worker-skills
    bash "$RENDER" /root/manager-workspace/worker-agent
    bash "$RENDER" /root/manager-workspace/copaw-worker-agent
    bash "$RENDER" /root/manager-workspace/hermes-worker-agent
    bash "$RENDER" /opt/hiclaw/agent/worker-skills
    bash "$RENDER" /opt/hiclaw/agent/worker-agent
    bash "$RENDER" /opt/hiclaw/agent/copaw-worker-agent
    bash "$RENDER" /opt/hiclaw/agent/hermes-worker-agent
    log "Agent doc templates rendered"

    if [ -n "${AGENTTEAMS_GITHUB_TOKEN}" ] && is_local_runtime; then
        if [ ! -f "${HOME}/config/mcporter.json" ]; then
            log "Auto-generating Manager mcporter config for GitHub MCP (AGENTTEAMS_GITHUB_TOKEN set)..."
            bash /opt/hiclaw/agent/skills/mcp-server-management/scripts/setup-mcp-server.sh \
                github "${AGENTTEAMS_GITHUB_TOKEN}" 2>&1 | while IFS= read -r line; do log "  [setup-mcp] ${line}"; done || \
                log "WARNING: setup-mcp-server.sh failed — Agent may need to configure GitHub MCP manually"
        else
            log "Manager mcporter config already exists, skipping auto-generate"
        fi
    fi
}
