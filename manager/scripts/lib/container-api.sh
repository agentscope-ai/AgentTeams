#!/bin/bash
# container-api.sh - Worker lifecycle API client
#
# Thin client for the agentteams-controller REST API.
# All worker CRUD operations go through the controller's unified API.
# Docker exec/logs operations still use Docker API passthrough.
#
# Required:
#   AGENTTEAMS_CONTROLLER_URL  - controller URL (e.g. http://agentteams-controller:8090)
#
# Usage:
#   source /opt/agentteams/scripts/lib/container-api.sh
#   worker_backend_create '{"name":"alice","image":"agentteams/worker-agent:latest"}'
#   worker_backend_status "alice"
#   worker_backend_delete "alice"

CONTAINER_API_BASE="${AGENTTEAMS_CONTROLLER_URL:-http://localhost:8090}"
WORKER_CONTAINER_PREFIX="agentteams-worker-"

# Resolve bearer token: AGENTTEAMS_AUTH_TOKEN > AGENTTEAMS_AUTH_TOKEN_FILE > none
_AGENTTEAMS_CONTROLLER_TOKEN=""
_resolve_controller_token() {
    # Re-read token each call (projected SA tokens are auto-rotated by kubelet)
    if [ -n "${AGENTTEAMS_AUTH_TOKEN:-}" ]; then
        _AGENTTEAMS_CONTROLLER_TOKEN="${AGENTTEAMS_AUTH_TOKEN}"
    elif [ -n "${AGENTTEAMS_AUTH_TOKEN_FILE:-}" ] && [ -f "${AGENTTEAMS_AUTH_TOKEN_FILE}" ]; then
        _AGENTTEAMS_CONTROLLER_TOKEN=$(cat "${AGENTTEAMS_AUTH_TOKEN_FILE}")
    fi
}

_log() {
    echo "[hiclaw-container $(date '+%Y-%m-%d %H:%M:%S')] $1"
}

# ============================================================
# Controller API client
# ============================================================

_orch_api() {
    local method="$1" path="$2" body="${3:-}"
    local url="${CONTAINER_API_BASE}${path}"
    _resolve_controller_token
    local auth_args=()
    if [ -n "${_AGENTTEAMS_CONTROLLER_TOKEN}" ]; then
        auth_args=(-H "Authorization: Bearer ${_AGENTTEAMS_CONTROLLER_TOKEN}")
    fi
    if [ -n "$body" ]; then
        curl -s -X "$method" "$url" "${auth_args[@]}" \
            -H "Content-Type: application/json" -d "$body"
    else
        curl -s -X "$method" "$url" "${auth_args[@]}"
    fi
}

_orch_api_code() {
    local method="$1" path="$2" body="${3:-}"
    local url="${CONTAINER_API_BASE}${path}"
    _resolve_controller_token
    local auth_args=()
    if [ -n "${_AGENTTEAMS_CONTROLLER_TOKEN}" ]; then
        auth_args=(-H "Authorization: Bearer ${_AGENTTEAMS_CONTROLLER_TOKEN}")
    fi
    if [ -n "$body" ]; then
        curl -s -o /dev/null -w '%{http_code}' -X "$method" "$url" "${auth_args[@]}" \
            -H "Content-Type: application/json" -d "$body"
    else
        curl -s -o /dev/null -w '%{http_code}' -X "$method" "$url" "${auth_args[@]}"
    fi
}

# Like _orch_api but appends "\n<http_code>" to the body, so callers can
# branch on status code (e.g. 404 vs 200) rather than only inspecting JSON.
_orch_api_full() {
    local method="$1" path="$2" body="${3:-}"
    local url="${CONTAINER_API_BASE}${path}"
    _resolve_controller_token
    local auth_args=()
    if [ -n "${_AGENTTEAMS_CONTROLLER_TOKEN}" ]; then
        auth_args=(-H "Authorization: Bearer ${_AGENTTEAMS_CONTROLLER_TOKEN}")
    fi
    if [ -n "$body" ]; then
        curl -s -w $'\n%{http_code}' -X "$method" "$url" "${auth_args[@]}" \
            -H "Content-Type: application/json" -d "$body"
    else
        curl -s -w $'\n%{http_code}' -X "$method" "$url" "${auth_args[@]}"
    fi
}

# ============================================================
# Worker Backend API (unified — controller handles Docker/SAE dispatch)
# ============================================================

# Create a worker. Accepts JSON body with name, image, runtime, env, etc.
# Usage: worker_backend_create '{"name":"alice","image":"img:latest","env":{...}}'
worker_backend_create() {
    local body="$1"
    _orch_api POST /api/v1/workers "$body"
}

# Delete a worker by name.
worker_backend_delete() {
    local worker_name="$1"
    _orch_api DELETE "/api/v1/workers/${worker_name}"
}

# Start a stopped worker. Returns 0 on success.
# Maps to controller's "wake" lifecycle action (sets spec.state=Running).
worker_backend_start() {
    local worker_name="$1"
    local code
    code=$(_orch_api_code POST "/api/v1/workers/${worker_name}/wake")
    [ "${code}" -ge 200 ] && [ "${code}" -lt 300 ]
}

# Stop a running worker. Returns 0 on success.
# Maps to controller's "sleep" lifecycle action (sets spec.state=Sleeping).
worker_backend_stop() {
    local worker_name="$1"
    local code
    code=$(_orch_api_code POST "/api/v1/workers/${worker_name}/sleep")
    [ "${code}" -ge 200 ] && [ "${code}" -lt 300 ]
}

# Get worker status as a lowercase phase string.
# Possible values mirror the controller's WorkerResponse.Phase, lowercased:
# pending | running | ready | sleeping | stopped | not_found | unknown.
# Uses /status endpoint so that a worker that has self-reported readiness shows
# up as "ready" rather than just "running". Callers should treat both
# "running" and "ready" as live states.
worker_backend_status() {
    local worker_name="$1"
    local response code body phase
    response=$(_orch_api_full GET "/api/v1/workers/${worker_name}/status")
    code="${response##*$'\n'}"
    body="${response%$'\n'*}"
    case "${code}" in
        2[0-9][0-9])
            phase=$(echo "${body}" | jq -r '.phase // "unknown"' 2>/dev/null | tr '[:upper:]' '[:lower:]')
            echo "${phase:-unknown}"
            ;;
        404)
            echo "not_found"
            ;;
        *)
            echo "unknown"
            ;;
    esac
}

# List all workers. Returns JSON with .workers array.
worker_backend_list() {
    _orch_api GET /api/v1/workers
}

# Check if controller API is reachable.
container_api_available() {
    local code
    code=$(_orch_api_code GET /api/v1/workers 2>/dev/null) || true
    [ "${code}" = "200" ]
}

# ============================================================
# Docker API passthrough (for exec, logs, inspect)
# ============================================================
# These operations require raw Docker API access and go through
# the controller's Docker API passthrough (catch-all route).
# Reuses _orch_api/_orch_api_code since they hit the same endpoint.

_api() { _orch_api "$@"; }

# Get Worker container logs (Docker API passthrough)
container_logs_worker() {
    local worker_name="$1"
    local tail="${2:-50}"
    local container_name="${WORKER_CONTAINER_PREFIX}${worker_name}"
    _api GET "/containers/${container_name}/logs?stdout=true&stderr=true&tail=${tail}"
}

# Get Worker container status via Docker inspect (for readiness checks)
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
container_exec_worker() {
    local worker_name="$1"
    shift
    local container_name="${WORKER_CONTAINER_PREFIX}${worker_name}"

    local cmd_json
    cmd_json=$(jq -cn --args '$ARGS.positional' -- "$@")

    local exec_create
    exec_create=$(_api POST "/containers/${container_name}/exec" \
        "{\"AttachStdout\":true,\"AttachStderr\":true,\"Tty\":false,\"Cmd\":${cmd_json}}")

    local exec_id
    exec_id=$(echo "${exec_create}" | jq -r '.Id // empty' 2>/dev/null)

    if [ -z "${exec_id}" ]; then
        return 1
    fi

    _api POST "/exec/${exec_id}/start" '{"Detach":false,"Tty":false}'
    return 0
}

# Get the Manager container's own IP (for Worker to connect back)
container_get_manager_ip() {
    hostname -I 2>/dev/null | awk '{print $1}'
}

# Wait for a worker to report ready via controller.
# Usage: worker_backend_wait_ready <worker_name> [timeout_seconds]
worker_backend_wait_ready() {
    local worker_name="$1"
    local timeout="${2:-120}"
    local elapsed=0

    _log "Waiting for Worker ${worker_name} to be ready (timeout: ${timeout}s)..."

    while [ "${elapsed}" -lt "${timeout}" ]; do
        local status
        status=$(worker_backend_status "${worker_name}")
        case "${status}" in
            ready)
                _log "Worker ${worker_name} is ready!"
                return 0
                ;;
            not_found|stopped|sleeping|unknown)
                _log "Worker ${worker_name} status: ${status} — aborting wait"
                return 1
                ;;
        esac
        sleep 5
        elapsed=$((elapsed + 5))
        _log "Waiting for Worker ${worker_name}... (${elapsed}s/${timeout}s, status=${status})"
    done

    _log "Worker ${worker_name} did not become ready within ${timeout}s"
    return 1
}
