#!/bin/bash
# base.sh - Shared utilities for AgentTeams startup scripts
# Source this file: source /opt/agentteams/scripts/lib/base.sh

set -e

# Wait for a TCP service to become available
# Usage: waitForService "ServiceName" "host" port [timeout_seconds]
waitForService() {
    local name="$1"
    local host="$2"
    local port="$3"
    local timeout="${4:-120}"
    local elapsed=0

    echo "[hiclaw] Waiting for ${name} at ${host}:${port}..."
    while ! curl -sf "http://${host}:${port}/" > /dev/null 2>&1 && \
          ! nc -z "${host}" "${port}" 2>/dev/null; do
        sleep 2
        elapsed=$((elapsed + 2))
        if [ "${elapsed}" -ge "${timeout}" ]; then
            echo "[hiclaw] ERROR: ${name} did not become available within ${timeout}s"
            exit 1
        fi
    done
    echo "[hiclaw] ${name} is ready (took ${elapsed}s)"
}

# Wait for an HTTP endpoint to return 200
# Usage: waitForHTTP "ServiceName" "url" [timeout_seconds]
waitForHTTP() {
    local name="$1"
    local url="$2"
    local timeout="${3:-120}"
    local elapsed=0

    echo "[hiclaw] Waiting for ${name} HTTP at ${url}..."
    while [ "$(curl -sf -o /dev/null -w '%{http_code}' "${url}" 2>/dev/null)" != "200" ]; do
        sleep 2
        elapsed=$((elapsed + 2))
        if [ "${elapsed}" -ge "${timeout}" ]; then
            echo "[hiclaw] ERROR: ${name} HTTP not ready within ${timeout}s"
            exit 1
        fi
    done
    echo "[hiclaw] ${name} HTTP is ready (took ${elapsed}s)"
}

# Generate a cryptographically secure random key
# Usage: generateKey [length_bytes]
generateKey() {
    local bytes="${1:-32}"
    openssl rand -hex "${bytes}"
}

# Log with timestamp
# Usage: log "message"
log() {
    echo "[hiclaw $(date '+%Y-%m-%d %H:%M:%S')] $1"
}
