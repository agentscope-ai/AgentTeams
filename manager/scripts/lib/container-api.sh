#!/bin/bash
# container-api.sh - Container runtime API helper
# Provides functions to create/manage sibling containers via the host's
# container runtime socket (Docker or Podman compatible).
#
# Supports two modes:
#   1. HTTP proxy mode: set HICLAW_CONTAINER_API=http://hiclaw-docker-proxy:2375
#   2. Unix socket mode (legacy): mount docker.sock into the container
#
# Usage:
#   source /opt/hiclaw/scripts/lib/container-api.sh
#   container_api_available           # returns 0 if socket is mounted
#   container_create_worker "alice"   # create and start a worker container
#   container_stop_worker "alice"     # stop a worker container
#   container_remove_worker "alice"   # remove a worker container
#   container_logs_worker "alice"     # get worker container logs

CONTAINER_SOCKET="${HICLAW_CONTAINER_SOCKET:-/var/run/docker.sock}"
CONTAINER_API_BASE="${HICLAW_CONTAINER_API:-}"
if [ -z "${CONTAINER_API_BASE}" ]; then
    CONTAINER_API_BASE="http://localhost"
fi
WORKER_IMAGE="${HICLAW_WORKER_IMAGE:-hiclaw/worker-agent:latest}"
COPAW_WORKER_IMAGE="${HICLAW_COPAW_WORKER_IMAGE:-hiclaw/copaw-worker:latest}"
WORKER_CONTAINER_PREFIX="hiclaw-worker-"

_log() {
    echo "[hiclaw-container $(date '+%Y-%m-%d %H:%M:%S')] $1"
}

_api() {
    local method="$1"
    local path="$2"
    local data="${3:-}"
    if [ -n "${HICLAW_CONTAINER_API}" ]; then
        # HTTP proxy mode
        if [ -n "${data}" ]; then
            curl -s -X "${method}" \
                -H 'Content-Type: application/json' \
                -d "${data}" \
                "${CONTAINER_API_BASE}${path}"
        else
            curl -s -X "${method}" \
                "${CONTAINER_API_BASE}${path}"
        fi
    else
        # Unix socket mode (legacy)
        if [ -n "${data}" ]; then
            curl -s --unix-socket "${CONTAINER_SOCKET}" \
                -X "${method}" \
                -H 'Content-Type: application/json' \
                -d "${data}" \
                "${CONTAINER_API_BASE}${path}"
        else
            curl -s --unix-socket "${CONTAINER_SOCKET}" \
                -X "${method}" \
                "${CONTAINER_API_BASE}${path}"
        fi
    fi
}

_api_code() {
    local method="$1"
    local path="$2"
    local data="${3:-}"
    if [ -n "${HICLAW_CONTAINER_API}" ]; then
        # HTTP proxy mode
        if [ -n "${data}" ]; then
            curl -s -o /dev/null -w '%{http_code}' -X "${method}" \
                -H 'Content-Type: application/json' \
                -d "${data}" \
                "${CONTAINER_API_BASE}${path}"
        else
            curl -s -o /dev/null -w '%{http_code}' -X "${method}" \
                "${CONTAINER_API_BASE}${path}"
        fi
    else
        # Unix socket mode (legacy)
        if [ -n "${data}" ]; then
            curl -s -o /dev/null -w '%{http_code}' --unix-socket "${CONTAINER_SOCKET}" \
                -X "${method}" \
                -H 'Content-Type: application/json' \
                -d "${data}" \
                "${CONTAINER_API_BASE}${path}"
        else
            curl -s -o /dev/null -w '%{http_code}' --unix-socket "${CONTAINER_SOCKET}" \
                -X "${method}" \
                "${CONTAINER_API_BASE}${path}"
        fi
    fi
}

# Check if container runtime API is available
# Supports both HTTP proxy mode (HICLAW_CONTAINER_API) and unix socket mode.
# This function is designed to work correctly in both strict mode (set -euo pipefail)
# and non-strict mode. It uses a subshell for the API check to prevent exit on errors.
container_api_available() {
    if [ -n "${HICLAW_CONTAINER_API}" ]; then
        # HTTP proxy mode: check if proxy is reachable
        local version
        version=$(curl -s "${CONTAINER_API_BASE}/version" 2>/dev/null) || true
        if echo "${version}" | grep -q '"ApiVersion"' 2>/dev/null; then
            return 0
        fi
        return 1
    fi
    # Unix socket mode (legacy)
    if [ ! -S "${CONTAINER_SOCKET}" ]; then
        return 1
    fi
    # Use a subshell to prevent strict mode (set -e) from exiting on curl failures
    # The || true ensures the command substitution doesn't fail in strict mode
    local version
    version=$(_api GET /version 2>/dev/null) || true
    if echo "${version}" | grep -q '"ApiVersion"' 2>/dev/null; then
        return 0
    fi
    return 1
}

# Get the Manager container's own IP (for Worker to connect back)
container_get_manager_ip() {
    hostname -I 2>/dev/null | awk '{print $1}'
}

# Ensure a container image exists locally, pulling it if necessary.
# Usage: _ensure_image <image>
# The Docker/Podman "create image" API streams JSON progress; we wait for
# completion and check the final status.
_ensure_image() {
    local image="$1"
    # Quick check: does the image already exist locally?
    local inspect
    inspect=$(_api GET "/images/${image}/json" 2>/dev/null)
    if echo "${inspect}" | grep -q '"Id"' 2>/dev/null; then
        return 0
    fi

    _log "Image not found locally, pulling: ${image}"
    # POST /images/create?fromImage=<ref> streams progress JSON.
    # curl will block until the pull finishes (or fails).
    local pull_output
    if [ -n "${HICLAW_CONTAINER_API}" ]; then
        pull_output=$(curl -s -X POST "${CONTAINER_API_BASE}/images/create?fromImage=${image}" 2>&1)
    else
        pull_output=$(curl -s --unix-socket "${CONTAINER_SOCKET}" \
            -X POST "${CONTAINER_API_BASE}/images/create?fromImage=${image}" 2>&1)
    fi

    # Verify the image is now available
    inspect=$(_api GET "/images/${image}/json" 2>/dev/null)
    if echo "${inspect}" | grep -q '"Id"' 2>/dev/null; then
        _log "Image pulled successfully: ${image}"
        return 0
    fi

    _log "ERROR: Failed to pull image: ${image}"
    _log "  Pull output (last 500 chars): ${pull_output: -500}"
    return 1
}

# Create and start a Worker container
# Usage: container_create_worker <worker_name> [fs_access_key] [fs_secret_key] [extra_env_json] [custom_image]
#   extra_env_json: optional JSON array of additional environment variables, e.g. '["SKILLS_API_URL=https://example.com"]'
#   custom_image: optional custom Docker image to use instead of the default WORKER_IMAGE
# Returns: container ID on success, empty on failure
container_create_worker() {
    local worker_name="$1"
    local container_name="${WORKER_CONTAINER_PREFIX}${worker_name}"

    # Build environment variables for the Worker
    # Always use the fixed internal domain so workers on hiclaw-net can reach MinIO
    # via the manager's network alias, regardless of user-configured FS domain.
    local fs_endpoint="http://fs-local.hiclaw.io:8080"
    local fs_access_key="${2:-${HICLAW_MINIO_USER:-${HICLAW_ADMIN_USER:-admin}}}"
    local fs_secret_key="${3:-${HICLAW_MINIO_PASSWORD:-${HICLAW_ADMIN_PASSWORD:-admin}}}"
    local extra_env="${4:-[]}"
    local custom_image="${5:-}"
    local image="${custom_image:-${WORKER_IMAGE}}"

    _log "Creating Worker container: ${container_name}"
    _log "  Image: ${image}"
    _log "  FS endpoint: ${fs_endpoint}"

    # Pull image if not available locally
    if ! _ensure_image "${image}"; then
        return 1
    fi

    # Remove existing container with same name (if any)
    local existing
    existing=$(_api GET "/containers/${container_name}/json" 2>/dev/null)
    if echo "${existing}" | grep -q '"Id"' 2>/dev/null; then
        _log "Removing existing container: ${container_name}"
        _api DELETE "/containers/${container_name}?force=true" > /dev/null 2>&1
        sleep 1
    fi

    # Create the container
    # Always use hiclaw-net; Docker DNS resolves *-local.hiclaw.io via manager's network aliases
    local host_config="{\"NetworkMode\":\"hiclaw-net\"}"

    local worker_home="/root/hiclaw-fs/agents/${worker_name}"

    # Build base environment variables
    local base_env='["HOME='"${worker_home}"'","HICLAW_WORKER_NAME='"${worker_name}"'","HICLAW_FS_ENDPOINT='"${fs_endpoint}"'","HICLAW_FS_ACCESS_KEY='"${fs_access_key}"'","HICLAW_FS_SECRET_KEY='"${fs_secret_key}"'"]'

    # Merge with extra environment variables if provided
    local all_env
    if [ "${extra_env}" != "[]" ] && [ -n "${extra_env}" ]; then
        all_env=$(echo "${base_env} ${extra_env}" | jq -s 'add')
    else
        all_env="${base_env}"
    fi

    local create_payload
    create_payload=$(cat <<PAYLOAD
{
    "Image": "${image}",
    "Env": ${all_env},
    "WorkingDir": "${worker_home}",
    "HostConfig": ${host_config}
}
PAYLOAD
)

    local create_resp
    create_resp=$(_api POST "/containers/create?name=${container_name}" "${create_payload}")
    local container_id
    container_id=$(echo "${create_resp}" | jq -r '.Id // empty' 2>/dev/null)

    if [ -z "${container_id}" ]; then
        _log "ERROR: Failed to create container. Response: ${create_resp}"
        return 1
    fi

    _log "Container created: ${container_id:0:12}"

    # Start the container
    local start_code
    start_code=$(_api_code POST "/containers/${container_id}/start")
    if [ "${start_code}" != "204" ] && [ "${start_code}" != "304" ]; then
        _log "ERROR: Failed to start container (HTTP ${start_code})"
        return 1
    fi

    _log "Worker container ${container_name} started successfully"
    echo "${container_id}"
    return 0
}

# Start an existing stopped Worker container
# Use this to wake up a container that was previously stopped (preserves container config).
# Different from container_create_worker which creates a new container from scratch.
container_start_worker() {
    local worker_name="$1"
    local container_name="${WORKER_CONTAINER_PREFIX}${worker_name}"
    local code
    code=$(_api_code POST "/containers/${container_name}/start")
    if [ "${code}" = "204" ] || [ "${code}" = "304" ]; then
        _log "Worker ${container_name} started"
        return 0
    fi
    _log "WARNING: Start returned HTTP ${code}"
    return 1
}

# Stop a Worker container
container_stop_worker() {
    local worker_name="$1"
    local container_name="${WORKER_CONTAINER_PREFIX}${worker_name}"
    local code
    code=$(_api_code POST "/containers/${container_name}/stop?t=10")
    if [ "${code}" = "204" ] || [ "${code}" = "304" ]; then
        _log "Worker ${container_name} stopped"
        return 0
    fi
    _log "WARNING: Stop returned HTTP ${code}"
    return 1
}

# Remove a Worker container (force)
container_remove_worker() {
    local worker_name="$1"
    local container_name="${WORKER_CONTAINER_PREFIX}${worker_name}"
    _api DELETE "/containers/${container_name}?force=true" > /dev/null 2>&1
    _log "Worker ${container_name} removed"
}

# Get Worker container logs
container_logs_worker() {
    local worker_name="$1"
    local tail="${2:-50}"
    local container_name="${WORKER_CONTAINER_PREFIX}${worker_name}"
    _api GET "/containers/${container_name}/logs?stdout=true&stderr=true&tail=${tail}"
}

# Get Worker container status
# Returns: "running", "exited", "created", or "not_found"
container_status_worker() {
    local worker_name="$1"
    local container_name="${WORKER_CONTAINER_PREFIX}${worker_name}"
    local inspect
    inspect=$(_api GET "/containers/${container_name}/json" 2>/dev/null)
    if echo "${inspect}" | grep -q '"Id"' 2>/dev/null; then
        echo "${inspect}" | jq -r '.State.Status // "unknown"' 2>/dev/null
    else
        echo "not_found"
    fi
}

# Execute a command inside a Worker container via Docker exec API
# Usage: container_exec_worker <worker_name> <cmd> [args...]
# Returns: command output (raw Docker stream; contains binary framing prefix per chunk)
container_exec_worker() {
    local worker_name="$1"
    shift
    local container_name="${WORKER_CONTAINER_PREFIX}${worker_name}"

    # Build JSON array from args using jq for proper escaping
    local cmd_json
    cmd_json=$(jq -cn --args '$ARGS.positional' -- "$@")

    # Create exec instance
    local exec_create
    exec_create=$(_api POST "/containers/${container_name}/exec" \
        "{\"AttachStdout\":true,\"AttachStderr\":true,\"Tty\":false,\"Cmd\":${cmd_json}}")

    local exec_id
    exec_id=$(echo "${exec_create}" | jq -r '.Id // empty' 2>/dev/null)

    if [ -z "${exec_id}" ]; then
        return 1
    fi

    # Start exec and stream output (binary-framed; callers can grep the raw bytes)
    _api POST "/exec/${exec_id}/start" '{"Detach":false,"Tty":false}'
    return 0
}

# Wait for Worker agent (OpenClaw gateway) to become ready
# Mirrors the wait_manager_ready logic in hiclaw-install.sh
# Usage: container_wait_worker_ready <worker_name> [timeout_seconds]
# Returns: 0 if ready, 1 if timed out or container stopped unexpectedly
container_wait_worker_ready() {
    local worker_name="$1"
    local timeout="${2:-120}"
    local elapsed=0

    _log "Waiting for Worker ${worker_name} to be ready (timeout: ${timeout}s)..."

    while [ "${elapsed}" -lt "${timeout}" ]; do
        # Bail early if the container is no longer running
        local cstatus
        cstatus=$(container_status_worker "${worker_name}")
        if [ "${cstatus}" != "running" ]; then
            _log "Worker container ${worker_name} stopped unexpectedly (status: ${cstatus})"
            return 1
        fi

        # Check OpenClaw gateway health inside the worker container.
        # The Docker exec API returns a binary-framed stream, but grep -q still
        # finds the string inside the payload bytes.
        if container_exec_worker "${worker_name}" openclaw gateway health --json 2>/dev/null \
                | grep -q '"ok"' 2>/dev/null; then
            _log "Worker ${worker_name} is ready!"
            return 0
        fi

        sleep 5
        elapsed=$((elapsed + 5))
        _log "Waiting for Worker ${worker_name}... (${elapsed}s/${timeout}s)"
    done

    _log "Worker ${worker_name} did not become ready within ${timeout}s"
    return 1
}

# Create and start a CoPaw Worker container
# Uses the CoPaw worker image and sets appropriate working directory.
# Usage: container_create_copaw_worker <worker_name> [fs_access_key] [fs_secret_key] [extra_env_json] [custom_image]
container_create_copaw_worker() {
    local worker_name="$1"
    local container_name="${WORKER_CONTAINER_PREFIX}${worker_name}"

    # Always use the fixed internal domain so workers on hiclaw-net can reach MinIO
    # via the manager's network alias, regardless of user-configured FS domain.
    local fs_endpoint="http://fs-local.hiclaw.io:8080"
    local fs_access_key="${2:-${HICLAW_MINIO_USER:-${HICLAW_ADMIN_USER:-admin}}}"
    local fs_secret_key="${3:-${HICLAW_MINIO_PASSWORD:-${HICLAW_ADMIN_PASSWORD:-admin}}}"
    local extra_env="${4:-[]}"
    local custom_image="${5:-}"
    local image="${custom_image:-${COPAW_WORKER_IMAGE}}"

    _log "Creating CoPaw Worker container: ${container_name}"
    _log "  Image: ${image}"
    _log "  FS endpoint: ${fs_endpoint}"

    # Pull image if not available locally
    if ! _ensure_image "${image}"; then
        return 1
    fi

    # Remove existing container with same name (if any)
    local existing
    existing=$(_api GET "/containers/${container_name}/json" 2>/dev/null)
    if echo "${existing}" | grep -q '"Id"' 2>/dev/null; then
        _log "Removing existing container: ${container_name}"
        _api DELETE "/containers/${container_name}?force=true" > /dev/null 2>&1
        sleep 1
    fi

    # CoPaw uses /root/.copaw-worker as install dir (not /root/hiclaw-fs/agents/<name>)
    local base_env='["HICLAW_WORKER_NAME='"${worker_name}"'","HICLAW_FS_ENDPOINT='"${fs_endpoint}"'","HICLAW_FS_ACCESS_KEY='"${fs_access_key}"'","HICLAW_FS_SECRET_KEY='"${fs_secret_key}"'"]'

    local all_env
    if [ "${extra_env}" != "[]" ] && [ -n "${extra_env}" ]; then
        all_env=$(echo "${base_env} ${extra_env}" | jq -s 'add')
    else
        all_env="${base_env}"
    fi

    # Detect HICLAW_CONSOLE_PORT in env to set up port binding
    local console_port=""
    console_port=$(echo "${all_env}" | jq -r '.[] | select(startswith("HICLAW_CONSOLE_PORT=")) | split("=")[1]' 2>/dev/null || true)

    if [ -n "${console_port}" ]; then
        _log "  Console port: ${console_port}"
    fi

    # ExposedPorts tells Docker which ports the container listens on
    local exposed_ports="{}"
    if [ -n "${console_port}" ]; then
        exposed_ports="{\"${console_port}/tcp\":{}}"
    fi

    # Pick a random host port (10000-20000) to minimize conflicts across workers
    local host_port="${console_port}"
    if [ -n "${console_port}" ]; then
        host_port=$(( (RANDOM % 10001) + 10000 ))
        _log "  Host port: ${host_port} (random)"
    fi
    local max_port_retries=10
    local port_attempt=0

    while true; do
        # Build HostConfig with NetworkMode (hiclaw-net) and optional PortBindings
        # Docker DNS resolves *-local.hiclaw.io via manager's network aliases; no ExtraHosts needed
        local host_config
        if [ -n "${console_port}" ]; then
            host_config="{\"NetworkMode\":\"hiclaw-net\",\"PortBindings\":{\"${console_port}/tcp\":[{\"HostPort\":\"${host_port}\"}]}}"
        else
            host_config="{\"NetworkMode\":\"hiclaw-net\"}"
        fi

        local create_payload
        create_payload=$(cat <<PAYLOAD
{
    "Image": "${image}",
    "Env": ${all_env},
    "WorkingDir": "/root/.copaw-worker",
    "ExposedPorts": ${exposed_ports},
    "HostConfig": ${host_config}
}
PAYLOAD
)

        local create_resp
        create_resp=$(_api POST "/containers/create?name=${container_name}" "${create_payload}")
        local container_id
        container_id=$(echo "${create_resp}" | jq -r '.Id // empty' 2>/dev/null)

        if [ -z "${container_id}" ]; then
            _log "ERROR: Failed to create CoPaw container. Response: ${create_resp}"
            return 1
        fi

        _log "CoPaw container created: ${container_id:0:12}"

        # Start the container — capture both HTTP status code and response body
        local start_output
        if [ -n "${HICLAW_CONTAINER_API}" ]; then
            start_output=$(curl -s -w '\n%{http_code}' \
                -X POST "${CONTAINER_API_BASE}/containers/${container_id}/start")
        else
            start_output=$(curl -s -w '\n%{http_code}' --unix-socket "${CONTAINER_SOCKET}" \
                -X POST "${CONTAINER_API_BASE}/containers/${container_id}/start")
        fi
        local start_code
        start_code=$(echo "${start_output}" | tail -1)
        local start_body
        start_body=$(echo "${start_output}" | sed '$d')

        if [ "${start_code}" = "204" ] || [ "${start_code}" = "304" ]; then
            if [ -n "${console_port}" ]; then
                _log "Console: container port ${console_port} -> host port ${host_port}"
                _log "CONSOLE_HOST_PORT=${host_port}"
            fi
            _log "CoPaw Worker container ${container_name} started successfully"
            echo "${container_id}"
            return 0
        fi

        # Start failed — check if it's a port conflict we can retry
        local err_msg
        err_msg=$(echo "${start_body}" | jq -r '.message // empty' 2>/dev/null)

        if [ -n "${console_port}" ] && echo "${err_msg}" | grep -qi "already allocated\|address already in use\|port is already" 2>/dev/null; then
            port_attempt=$((port_attempt + 1))
            if [ "${port_attempt}" -ge "${max_port_retries}" ]; then
                _log "ERROR: Could not find available port after ${max_port_retries} attempts (tried ${console_port}-${host_port})"
                return 1
            fi
            _log "Host port ${host_port} is in use, trying $((host_port + 1))..."
            host_port=$((host_port + 1))
            _api DELETE "/containers/${container_name}?force=true" > /dev/null 2>&1
            sleep 1
            continue
        fi

        # Non-port-conflict error — fail immediately
        _log "ERROR: Failed to start CoPaw container (HTTP ${start_code}): ${err_msg:-${start_body}}"
        return 1
    done
}

# Wait for CoPaw Worker to become ready
# CoPaw writes config.json after bridge completes; we check for that file.
# Usage: container_wait_copaw_worker_ready <worker_name> [timeout_seconds]
container_wait_copaw_worker_ready() {
    local worker_name="$1"
    local timeout="${2:-120}"
    local elapsed=0
    local config_file="/root/.copaw-worker/${worker_name}/.copaw/config.json"

    _log "Waiting for CoPaw Worker ${worker_name} to be ready (timeout: ${timeout}s)..."

    while [ "${elapsed}" -lt "${timeout}" ]; do
        local cstatus
        cstatus=$(container_status_worker "${worker_name}")
        if [ "${cstatus}" != "running" ]; then
            _log "CoPaw Worker container ${worker_name} stopped unexpectedly (status: ${cstatus})"
            return 1
        fi

        # Check if CoPaw bridge has completed (config.json with channels key exists)
        if container_exec_worker "${worker_name}" cat "${config_file}" 2>/dev/null \
                | grep -q '"channels"' 2>/dev/null; then
            _log "CoPaw Worker ${worker_name} is ready!"
            return 0
        fi

        sleep 5
        elapsed=$((elapsed + 5))
        _log "Waiting for CoPaw Worker ${worker_name}... (${elapsed}s/${timeout}s)"
    done

    _log "CoPaw Worker ${worker_name} did not become ready within ${timeout}s"
    return 1
}

# List all HiClaw Worker containers
container_list_workers() {
    _api GET "/containers/json?all=true&filters=%7B%22name%22%3A%5B%22${WORKER_CONTAINER_PREFIX}%22%5D%7D" 2>/dev/null | \
        jq -r '.[] | "\(.Names[0] | ltrimstr("/") | ltrimstr("'"${WORKER_CONTAINER_PREFIX}"'"))\t\(.State)\t\(.Status)"' 2>/dev/null
}


# ============================================================
# Cloud Provider Extensions
# ============================================================
# Load cloud providers (additive — does not modify upstream functions above).
# Each provider file defines its own *_available() check and lifecycle functions.
for _provider_file in /opt/hiclaw/scripts/lib/cloud/*.sh; do
    [ -f "${_provider_file}" ] && source "${_provider_file}"
done
unset _provider_file

# ============================================================
# Unified Worker Backend API
# ============================================================
# Auto-detects Docker vs cloud vs none and dispatches to the right backend.
# All skill scripts should use these instead of calling Docker/SAE directly.

_detect_worker_backend() {
    if container_api_available 2>/dev/null; then
        echo "docker"
    elif [ "${HICLAW_RUNTIME:-}" = "aliyun" ]; then
        echo "aliyun"
    elif type cloud_sae_available &>/dev/null && cloud_sae_available; then
        echo "aliyun"
    else
        echo "none"
    fi
}

worker_backend_create() {
    local worker_name="$1"
    local fs_access_key="${2:-}"
    local fs_secret_key="${3:-}"
    local extra_env_json="${4:-[]}"
    local backend
    backend=$(_detect_worker_backend)

    case "${backend}" in
        docker)
            container_create_worker "${worker_name}" "${fs_access_key}" "${fs_secret_key}" "${extra_env_json}"
            ;;
        aliyun)
            local envs_obj="{}"
            if [ "${extra_env_json}" != "[]" ] && [ -n "${extra_env_json}" ]; then
                envs_obj=$(echo "${extra_env_json}" | jq '[.[] | split("=") | {(.[0]): (.[1:] | join("="))}] | add // {}')
            fi
            sae_create_worker "${worker_name}" "${envs_obj}"
            ;;
        none)
            _log "No worker backend available (no Docker socket, no cloud config)"
            echo '{"error": "no_backend"}'
            return 1
            ;;
    esac
}

worker_backend_status() {
    local worker_name="$1"
    local backend
    backend=$(_detect_worker_backend)

    case "${backend}" in
        docker)       container_status_worker "${worker_name}" ;;
        aliyun) sae_status_worker "${worker_name}" ;;
        none)         echo "unknown" ;;
    esac
}

worker_backend_stop() {
    local worker_name="$1"
    local backend
    backend=$(_detect_worker_backend)

    case "${backend}" in
        docker)       container_stop_worker "${worker_name}" ;;
        aliyun) sae_stop_worker "${worker_name}" ;;
        none)         return 1 ;;
    esac
}

worker_backend_start() {
    local worker_name="$1"
    local backend
    backend=$(_detect_worker_backend)

    case "${backend}" in
        docker)       container_start_worker "${worker_name}" ;;
        aliyun) sae_start_worker "${worker_name}" ;;
        none)         return 1 ;;
    esac
}

worker_backend_delete() {
    local worker_name="$1"
    local backend
    backend=$(_detect_worker_backend)

    case "${backend}" in
        docker)       container_remove_worker "${worker_name}" ;;
        aliyun) sae_delete_worker "${worker_name}" ;;
        none)         return 1 ;;
    esac
}
