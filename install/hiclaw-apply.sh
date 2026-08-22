#!/bin/bash
# hiclaw-apply.sh - Unified entry point for declarative resource management
#
# Thin shell that forwards to `hiclaw apply` inside the Manager container.
# Supports Worker, Team, and Human resources in YAML format.
#
# Usage:
#   ./hiclaw-apply.sh -f resource.yaml              # incremental apply
#   ./hiclaw-apply.sh -f resource.yaml --prune      # full sync (delete extras)
#   ./hiclaw-apply.sh -f resource.yaml --dry-run    # show diff only
#   ./hiclaw-apply.sh -f resource.yaml --watch      # watch file changes
#
# Environment:
#   HICLAW_CONTAINER_CMD   Override container runtime (docker/podman)

set -e

log() {
    echo -e "\033[36m[HiClaw Apply]\033[0m $1"
}

error() {
    echo -e "\033[31m[HiClaw Apply ERROR]\033[0m $1" >&2
    exit 1
}

# ============================================================
# Detect container runtime
# ============================================================
CONTAINER_CMD="${HICLAW_CONTAINER_CMD:-}"
if [ -z "${CONTAINER_CMD}" ]; then
    if command -v docker > /dev/null 2>&1; then
        CONTAINER_CMD="docker"
    elif command -v podman > /dev/null 2>&1; then
        CONTAINER_CMD="podman"
    else
        error "Neither docker nor podman found"
    fi
fi

# ============================================================
# Verify Manager container is running
# ============================================================
if ! ${CONTAINER_CMD} ps --filter name=hiclaw-manager --format '{{.Names}}' 2>/dev/null | grep -q 'hiclaw-manager'; then
    error "hiclaw-manager container is not running"
fi

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
            ${CONTAINER_CMD} cp "${arg}" "hiclaw-manager:/tmp/import/${BASENAME}"
            ARGS+=("/tmp/import/${BASENAME}")
            log "Copied ${arg} → container:/tmp/import/${BASENAME}"
        else
            error "File not found: ${arg}"
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

# Ensure /tmp/import exists
${CONTAINER_CMD} exec hiclaw-manager mkdir -p /tmp/import 2>/dev/null || true

# ============================================================
# Forward to hiclaw CLI inside container
# ============================================================
log "Forwarding to hiclaw apply..."
${CONTAINER_CMD} exec hiclaw-manager hiclaw apply "${ARGS[@]}"
