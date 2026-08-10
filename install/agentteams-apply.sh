#!/bin/bash
# agentteams-apply.sh - Unified entry point for declarative resource management
#
# Thin shell that forwards to `agt apply` inside the Manager container.
# Supports Worker, Team, and Human resources in YAML format.
#
# Usage:
#   ./agentteams-apply.sh -f resource.yaml              # incremental apply
#   ./agentteams-apply.sh -f resource.yaml --prune      # full sync (delete extras)
#   ./agentteams-apply.sh -f resource.yaml --dry-run    # show diff only
#   ./agentteams-apply.sh -f resource.yaml --watch      # watch file changes
#
# Environment:
#   AGENTTEAMS_CONTAINER_CMD   Override container runtime (docker/podman)

set -e

log() {
    echo -e "\033[36m[AgentTeams Apply]\033[0m $1"
}

error() {
    echo -e "\033[31m[AgentTeams Apply ERROR]\033[0m $1" >&2
}

die() {
    error "$1"
    exit 1
}

# ============================================================
# Detect container runtime
# ============================================================
CONTAINER_CMD="${AGENTTEAMS_CONTAINER_CMD:-}"
if [ -z "${CONTAINER_CMD}" ]; then
    if command -v docker > /dev/null 2>&1; then
        CONTAINER_CMD="docker"
    elif command -v podman > /dev/null 2>&1; then
        CONTAINER_CMD="podman"
    else
        die "Neither docker nor podman found"
    fi
fi

# ============================================================
# Verify Manager container is running
# ============================================================
if ! ${CONTAINER_CMD} ps --filter name=agentteams-manager --format '{{.Names}}' 2>/dev/null | grep -q 'agentteams-manager'; then
    die "agentteams-manager container is not running"
fi

# Ensure /tmp/import exists before copying files into container
${CONTAINER_CMD} exec agentteams-manager mkdir -p /tmp/import 2>/dev/null || true

# ============================================================
# Copy YAML files and referenced packages into container
# ============================================================
ARGS=()
NEXT_IS_FILE=false

for arg in "$@"; do
    if [ "${NEXT_IS_FILE}" = true ]; then
        NEXT_IS_FILE=false
        if [ -f "${arg}" ]; then
            BASENAME=$(basename "${arg}")
            ${CONTAINER_CMD} cp "${arg}" "agentteams-manager:/tmp/import/${BASENAME}"
            ARGS+=("/tmp/import/${BASENAME}")
            log "Copied ${arg} → container:/tmp/import/${BASENAME}"
        else
            die "File not found: ${arg}"
        fi
        continue
    fi

    if [ "${arg}" = "-f" ] || [ "${arg}" = "--file" ]; then
        NEXT_IS_FILE=true
        ARGS+=("-f")
        continue
    fi

    ARGS+=("${arg}")
done

# ============================================================
# Forward to AgentTeams CLI inside container
# ============================================================
log "Forwarding to agt apply..."
${CONTAINER_CMD} exec agentteams-manager agt apply "${ARGS[@]}"
