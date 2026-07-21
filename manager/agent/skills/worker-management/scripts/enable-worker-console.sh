#!/bin/bash
# enable-worker-console.sh - Enable or disable the CoPaw web console
#
# Recreates the CoPaw worker container with or without AGENTTEAMS_CONSOLE_PORT.
# The host port is randomly assigned (10000-20000) to avoid conflicts.
#
# Usage:
#   enable-worker-console.sh --name <worker> [--action enable|disable] [--port <PORT>]
#
# Actions:
#   enable  - add AGENTTEAMS_CONSOLE_PORT, map a random host port (default)
#   disable - remove AGENTTEAMS_CONSOLE_PORT, free the host port
#
# Output: JSON result after ---RESULT--- with console_host_port

set -eo pipefail

source /opt/agentteams/scripts/lib/container-api.sh

WORKER_NAME=""
ACTION="enable"
CONSOLE_PORT="8088"

while [ $# -gt 0 ]; do
    case "$1" in
        --name)   WORKER_NAME="$2"; shift 2 ;;
        --action) ACTION="$2"; shift 2 ;;
        --port)   CONSOLE_PORT="$2"; shift 2 ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

if [ -z "${WORKER_NAME}" ]; then
    echo "Usage: enable-worker-console.sh --name <NAME> [--action enable|disable] [--port <PORT>]"
    exit 1
fi

# Cloud mode: CoPaw console is only available for local container deployments
if [ "${AGENTTEAMS_RUNTIME:-}" = "aliyun" ]; then
    jq -n '{"error": "console_not_supported", "message": "CoPaw console is only available for local container deployments. On cloud (SAE), use SAE console or SLS logs instead."}'
    exit 1
fi

log() {
    echo "[enable-console $(date '+%Y-%m-%d %H:%M:%S')] $1"
}

CONTAINER_NAME="${WORKER_CONTAINER_PREFIX}${WORKER_NAME}"

# --- Inspect current container ---
log "Inspecting container ${CONTAINER_NAME}..."
INSPECT=$(_api GET "/containers/${CONTAINER_NAME}/json" 2>/dev/null)
if ! echo "${INSPECT}" | grep -q '"Id"' 2>/dev/null; then
    log "ERROR: Container ${CONTAINER_NAME} not found"
    jq -n '{"error": "container_not_found", "message": "Worker container does not exist in the Manager-local container backend."}'
    exit 1
fi

CONTAINER_IMAGE=$(echo "${INSPECT}" | jq -r '.Config.Image')
if ! echo "${CONTAINER_IMAGE}" | grep -qi "copaw" 2>/dev/null; then
    log "ERROR: Container ${CONTAINER_NAME} is not a CoPaw worker (image: ${CONTAINER_IMAGE})"
    jq -n --arg img "${CONTAINER_IMAGE}" '{"error": "not_copaw_worker", "message": ("Only CoPaw workers support console. Image: " + $img)}'
    exit 1
fi

# --- Check current console status ---
CURRENT_ENV=$(echo "${INSPECT}" | jq '.Config.Env')
CURRENT_CONSOLE_PORT=$(echo "${CURRENT_ENV}" | jq -r '.[] | select(startswith("AGENTTEAMS_CONSOLE_PORT=")) | split("=")[1]' 2>/dev/null || true)

if [ "${ACTION}" = "enable" ] && [ -n "${CURRENT_CONSOLE_PORT}" ]; then
    CURRENT_HOST_PORT=$(echo "${INSPECT}" | jq -r ".HostConfig.PortBindings[\"${CURRENT_CONSOLE_PORT}/tcp\"][0].HostPort // empty" 2>/dev/null)
    log "Console is already enabled (container port: ${CURRENT_CONSOLE_PORT}, host port: ${CURRENT_HOST_PORT:-unknown})"
    jq -n --arg port "${CURRENT_HOST_PORT:-unknown}" '{"status": "already_enabled", "console_host_port": $port}'
    exit 0
fi

if [ "${ACTION}" = "disable" ] && [ -z "${CURRENT_CONSOLE_PORT}" ]; then
    log "Console is already disabled"
    jq -n '{"status": "already_disabled"}'
    exit 0
fi

# --- Extract credentials from current env ---
FS_ACCESS_KEY=$(echo "${CURRENT_ENV}" | jq -r '.[] | select(startswith("AGENTTEAMS_FS_ACCESS_KEY=")) | split("=")[1]')
FS_SECRET_KEY=$(echo "${CURRENT_ENV}" | jq -r '.[] | select(startswith("AGENTTEAMS_FS_SECRET_KEY=")) | split("=")[1]')

# Build extra env: keep all non-base vars, add/remove AGENTTEAMS_CONSOLE_PORT
EXTRA_ENV=$(echo "${CURRENT_ENV}" | jq '[.[] | select(
    (startswith("AGENTTEAMS_WORKER_NAME=") or
     startswith("AGENTTEAMS_FS_ENDPOINT=") or
     startswith("AGENTTEAMS_FS_ACCESS_KEY=") or
     startswith("AGENTTEAMS_FS_SECRET_KEY=") or
     startswith("AGENTTEAMS_CONSOLE_PORT=")) | not)]')

if [ "${ACTION}" = "enable" ]; then
    EXTRA_ENV=$(echo "${EXTRA_ENV}" | jq --arg port "${CONSOLE_PORT}" '. + ["AGENTTEAMS_CONSOLE_PORT=" + $port]')
    log "Enabling console (container port: ${CONSOLE_PORT})"
else
    log "Disabling console"
fi

# --- Recreate container via controller ---
log "Deleting worker ${WORKER_NAME}..."
worker_backend_delete "${WORKER_NAME}" > /dev/null 2>&1 || true
sleep 1

log "Recreating worker..."
# Build env map from the extra env array
ENV_MAP=$(echo "${EXTRA_ENV}" | jq '[.[] | split("=") | {(.[0]): (.[1:] | join("="))}] | add // {}')
ENV_MAP=$(echo "${ENV_MAP}" | jq \
    --arg name "${WORKER_NAME}" \
    --arg fak "${FS_ACCESS_KEY}" \
    --arg fsk "${FS_SECRET_KEY}" \
    '. + {"AGENTTEAMS_WORKER_NAME": $name, "AGENTTEAMS_FS_ACCESS_KEY": $fak, "AGENTTEAMS_FS_SECRET_KEY": $fsk}')

CREATE_BODY=$(jq -cn \
    --arg name "${WORKER_NAME}" \
    --arg image "${CONTAINER_IMAGE}" \
    --argjson env "${ENV_MAP}" \
    '{name: $name, image: $image, runtime: "copaw", env: $env}')

CREATE_OUTPUT=$(worker_backend_create "${CREATE_BODY}" 2>/dev/null) || true
CREATE_STATUS=$(echo "${CREATE_OUTPUT}" | jq -r '.status // "error"' 2>/dev/null)
CONTAINER_ID=$(echo "${CREATE_OUTPUT}" | jq -r '.container_id // empty' 2>/dev/null)
CONSOLE_HOST_PORT=$(echo "${CREATE_OUTPUT}" | jq -r '.console_host_port // empty' 2>/dev/null)

if [ "${CREATE_STATUS}" != "running" ] && [ "${CREATE_STATUS}" != "starting" ]; then
    log "ERROR: Failed to recreate container"
    echo "${CREATE_OUTPUT}" >&2
    jq -n '{"error": "recreate_failed"}'
    exit 1
fi

# --- Wait for ready ---
log "Waiting for CoPaw worker to be ready..."
if worker_backend_wait_ready "${WORKER_NAME}" 120; then
    WORKER_STATUS="ready"
    log "CoPaw Worker is ready!"
else
    WORKER_STATUS="starting"
    log "WARNING: CoPaw Worker not ready within timeout (may still be initializing)"
fi

# --- Output result ---
RESULT=$(jq -n \
    --arg name "${WORKER_NAME}" \
    --arg action "${ACTION}" \
    --arg status "${WORKER_STATUS}" \
    --arg container_id "${CONTAINER_ID}" \
    --arg console_host_port "${CONSOLE_HOST_PORT:-}" \
    '{
        worker_name: $name,
        action: $action,
        status: $status,
        container_id: $container_id,
        console_host_port: (if $console_host_port == "" then null else $console_host_port end)
    }')

echo "---RESULT---"
echo "${RESULT}"
