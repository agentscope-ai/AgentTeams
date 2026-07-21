#!/bin/bash
# test-helpers.sh - Common test utilities: assertions, lifecycle, logging
# Source this file in each test script.

# NOTE: Do NOT use "set -e" here. Tests use assertions (log_pass/log_fail)
# for results, not exit codes. set -e would abort the test script on the
# first failing curl or command, hiding remaining test results.

# ============================================================
# Configuration
# ============================================================

# Auto-detect infrastructure container (embedded controller or legacy manager)
if [ -z "${TEST_CONTROLLER_CONTAINER}" ]; then
    export TEST_CONTROLLER_CONTAINER="$(docker ps --format '{{.Names}}' 2>/dev/null | grep -E '^agentteams-controller$' | head -1 || true)"
    if [ -z "${TEST_CONTROLLER_CONTAINER}" ]; then
        # Fallback: legacy all-in-one manager container name
        export TEST_CONTROLLER_CONTAINER="$(docker ps --format '{{.Names}}' 2>/dev/null | grep -E '^hiclaw-manager$' | head -1 || true)"
    fi
    export TEST_CONTROLLER_CONTAINER="${TEST_CONTROLLER_CONTAINER:-agentteams-controller}"
fi

# Auto-detect Manager Agent container (separate container in embedded-controller mode)
if [ -z "${TEST_AGENT_CONTAINER}" ]; then
    export TEST_AGENT_CONTAINER="$(docker ps --format '{{.Names}}' 2>/dev/null | grep -E '^agentteams-manager(-|$)' | head -1 || true)"
    if [ -z "${TEST_AGENT_CONTAINER}" ]; then
        export TEST_AGENT_CONTAINER="$(docker ps --format '{{.Names}}' 2>/dev/null | grep -E '^hiclaw-manager(-|$)' | head -1 || true)"
    fi
    export TEST_AGENT_CONTAINER="${TEST_AGENT_CONTAINER:-${TEST_CONTROLLER_CONTAINER}}"
fi

# Host where the Manager container's exposed ports are reachable
export TEST_MANAGER_HOST="127.0.0.1"

# External host ports — auto-detected from container env in detect_manager_config()
export TEST_GATEWAY_PORT="${TEST_GATEWAY_PORT:-18080}"
export TEST_CONSOLE_PORT="${TEST_CONSOLE_PORT:-18001}"
export TEST_ELEMENT_PORT="${TEST_ELEMENT_PORT:-18088}"

# Internal container URLs — always fixed; all callers use exec_in_manager
export TEST_MATRIX_DIRECT_URL="http://127.0.0.1:6167"
export TEST_MINIO_URL="http://127.0.0.1:9000"
export TEST_STORAGE_PREFIX="${TEST_STORAGE_PREFIX:-agentteams/agentteams-storage}"

# Derived external URLs — rebuilt by detect_manager_config() after port detection
export TEST_CONSOLE_URL="http://${TEST_MANAGER_HOST}:${TEST_CONSOLE_PORT}"

# Matrix domain — auto-detected from container env in detect_manager_config()
export TEST_MATRIX_DOMAIN="${TEST_MATRIX_DOMAIN:-}"

# Test state
TESTS_PASSED=0
TESTS_FAILED=0
TESTS_TOTAL=0
TEST_FAILURES=()

# ============================================================
# Logging
# ============================================================

log_info() {
    echo -e "\033[36m[TEST INFO]\033[0m $1" >&2
}

log_pass() {
    echo -e "\033[32m[TEST PASS]\033[0m $1"
    TESTS_PASSED=$((TESTS_PASSED + 1))
    TESTS_TOTAL=$((TESTS_TOTAL + 1))
}

log_fail() {
    echo -e "\033[31m[TEST FAIL]\033[0m $1"
    TESTS_FAILED=$((TESTS_FAILED + 1))
    TESTS_TOTAL=$((TESTS_TOTAL + 1))
    TEST_FAILURES+=("$1")
}

log_section() {
    echo ""
    echo -e "\033[35m=== $1 ===\033[0m"
}

# ============================================================
# Assertions
# ============================================================

assert_eq() {
    local expected="$1"
    local actual="$2"
    local message="${3:-assert_eq}"

    if [ "${expected}" = "${actual}" ]; then
        log_pass "${message}"
    else
        log_fail "${message} (expected: '${expected}', got: '${actual}')"
    fi
}

assert_contains() {
    local haystack="$1"
    local needle="$2"
    local message="${3:-assert_contains}"

    if echo "${haystack}" | grep -q "${needle}"; then
        log_pass "${message}"
    else
        log_fail "${message} (expected to contain: '${needle}')"
    fi
}

assert_contains_i() {
    local haystack="$1"
    local needle="$2"
    local message="${3:-assert_contains_i}"

    if echo "${haystack}" | grep -qi "${needle}"; then
        log_pass "${message}"
    else
        log_fail "${message} (expected to contain (case-insensitive): '${needle}')"
    fi
}

assert_not_empty() {
    local value="$1"
    local message="${2:-assert_not_empty}"

    if [ -n "${value}" ] && [ "${value}" != "null" ]; then
        log_pass "${message}"
    else
        log_fail "${message} (value is empty or null)"
    fi
}

assert_http_code() {
    local url="$1"
    local expected_code="$2"
    local message="${3:-assert_http_code}"
    local extra_args="${4:-}"

    local actual_code
    # Use -s (silent) without -f (fail) so curl always outputs the HTTP code.
    # With -f, curl exits non-zero on 4xx/5xx, and || echo "000" would concatenate.
    actual_code=$(curl -s -o /dev/null -w '%{http_code}' ${extra_args} "${url}" 2>/dev/null)

    assert_eq "${expected_code}" "${actual_code}" "${message}"
}

# ============================================================
# Wait / Poll utilities
# ============================================================

# Wait until a condition function returns 0, or timeout
# Usage: wait_until "description" timeout_seconds check_function [args...]
wait_until() {
    local description="$1"
    local timeout="$2"
    shift 2
    local check_fn="$@"

    local elapsed=0
    log_info "Waiting for: ${description} (timeout: ${timeout}s)"

    while ! eval "${check_fn}" 2>/dev/null; do
        sleep 5
        elapsed=$((elapsed + 5))
        if [ "${elapsed}" -ge "${timeout}" ]; then
            log_fail "Timeout waiting for: ${description}"
            return 1
        fi
    done

    log_info "${description} ready (took ${elapsed}s)"
    return 0
}

# Wait for Manager container to be healthy
wait_for_manager() {
    local timeout="${1:-300}"
    wait_until "Manager container healthy" "${timeout}" \
        "curl -sf http://${TEST_MANAGER_HOST}:${TEST_GATEWAY_PORT}/ > /dev/null 2>&1"
}

# Wait for Manager Agent (OpenClaw or CoPaw) to be fully ready
# Phase 1: Runtime health check (OpenClaw gateway or CoPaw process)
# Phase 2: Manager has joined the specified DM room
# Usage: wait_for_manager_agent_ready [timeout] [room_id] [access_token]
wait_for_manager_agent_ready() {
    local timeout="${1:-300}"
    local room_id="${2:-}"
    local access_token="${3:-}"
    local infra_container="${TEST_CONTROLLER_CONTAINER:-agentteams-controller}"
    local agent_container="${TEST_AGENT_CONTAINER:-${infra_container}}"
    local manager_user="manager"
    local matrix_domain="${TEST_MATRIX_DOMAIN:-matrix-local.agentteams.io:${TEST_GATEWAY_PORT}}"

    local elapsed=0

    # Detect Manager runtime (check agent container first, then infra)
    local manager_runtime
    manager_runtime=$(docker exec "${agent_container}" printenv AGENTTEAMS_MANAGER_RUNTIME 2>/dev/null || \
                      docker exec "${infra_container}" printenv AGENTTEAMS_MANAGER_RUNTIME 2>/dev/null || \
                      docker exec "${agent_container}" printenv HICLAW_MANAGER_RUNTIME 2>/dev/null || \
                      docker exec "${infra_container}" printenv HICLAW_MANAGER_RUNTIME 2>/dev/null || echo "openclaw")

    # Phase 1: Wait for Manager Agent to be healthy (runtime-specific, on agent container)
    log_info "Waiting for Manager ${manager_runtime} runtime to be healthy (container: ${agent_container})..."
    local runtime_ready=false

    while [ "${elapsed}" -lt "${timeout}" ]; do
        case "${manager_runtime}" in
            copaw)
                if docker exec "${agent_container}" pgrep -f "copaw(_worker\\.run_copaw_app)? app" >/dev/null 2>&1 && \
                   docker exec "${agent_container}" curl -sf http://127.0.0.1:18799/ >/dev/null 2>&1; then
                    runtime_ready=true
                    break
                fi
                ;;
            *)
                if docker exec "${agent_container}" openclaw gateway health --json 2>/dev/null | grep -q '"ok"'; then
                    runtime_ready=true
                    break
                fi
                ;;
        esac
        sleep 5
        elapsed=$((elapsed + 5))
        printf "\r\033[36m[TEST INFO]\033[0m Waiting for %s runtime... (%ds/%ds)" "${manager_runtime}" "${elapsed}" "${timeout}"
    done

    if [ "${runtime_ready}" != "true" ]; then
        log_fail "${manager_runtime} runtime did not become healthy within ${timeout}s"
        return 1
    fi

    log_info "${manager_runtime} runtime is healthy (took ${elapsed}s)"

    # Phase 2: Wait for Manager to join the DM room (if room_id and token provided)
    # Matrix API calls go via infrastructure container (where Tuwunel runs)
    if [ -n "${room_id}" ] && [ -n "${access_token}" ]; then
        log_info "Waiting for Manager to join DM room..."
        local manager_full_id="@${manager_user}:${matrix_domain}"
        local manager_joined=false

        local room_enc="${room_id//!/%21}"
        while [ "${elapsed}" -lt "${timeout}" ]; do
            local members
            members=$(docker exec "${infra_container}" curl -sf -X GET \
                -H "Authorization: Bearer ${access_token}" \
                "http://127.0.0.1:6167/_matrix/client/v3/rooms/${room_enc}/members" 2>/dev/null | \
                jq -r '.chunk[].state_key' 2>/dev/null) || true

            if echo "${members}" | grep -q "${manager_full_id}"; then
                manager_joined=true
                log_info "Manager has joined the DM room"
                break
            fi
            sleep 3
            elapsed=$((elapsed + 3))
            printf "\r\033[36m[TEST INFO]\033[0m Waiting for Manager to join room... (%ds/%ds)" "${elapsed}" "${timeout}"
        done

        if [ "${manager_joined}" != "true" ]; then
            log_fail "Manager did not join the DM room within ${timeout}s"
            return 1
        fi
    fi

    log_info "Manager Agent is fully ready"
    return 0
}

# ------------------------------------------------------------
# CR-status-based waiters (replace fragile log-grep assertions).
#
# These replace the earlier `grep "team created"` / `grep "worker created"`
# patterns that broke after PR #666 — team members no longer emit a
# per-creation `worker created` log line, and the team reconciler now logs
# `team reconciled` (repeated) instead of a one-shot `team created`. The
# canonical readiness signal is the CR's `.status` subresource, which the
# CLI surfaces via `agt get`. Using the status means tests stay correct
# across logging refactors and work regardless of log rotation.
# ------------------------------------------------------------

# wait_team_active <team_name> [timeout_seconds] [expected_phase]
# Polls `agt get teams <name> -o json` until .phase matches expected_phase
# (default "Active"). Emits no log_pass/log_fail so the caller chooses how
# to assert (typically followed by `assert_eq` on the resulting phase).
# Returns 0 on match, 1 on timeout (and prints last-seen phase to stderr).
wait_team_active() {
    local team_name="$1"
    local timeout="${2:-180}"
    local want="${3:-Active}"
    local elapsed=0
    local last=""
    while [ "${elapsed}" -lt "${timeout}" ]; do
        last=$(exec_in_agent agt get teams "${team_name}" -o json 2>/dev/null | jq -r '.phase // empty')
        if [ "${last}" = "${want}" ]; then
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
    echo "wait_team_active: team=${team_name} timed out after ${timeout}s, last_phase='${last}'" >&2
    dump_diagnostics team "${team_name}"
    return 1
}

# wait_worker_phase <worker_name> [timeout_seconds] [expected_phase]
# Polls `agt get workers <name>` (works for standalone Workers AND
# synthesized team members, since ResourceHandler.teamMemberToResponse
# serves both under one endpoint) until .phase matches expected_phase
# (default "Running").
wait_worker_phase() {
    local worker_name="$1"
    local timeout="${2:-180}"
    local want="${3:-Running}"
    local elapsed=0
    local last=""
    while [ "${elapsed}" -lt "${timeout}" ]; do
        last=$(exec_in_agent agt get workers "${worker_name}" -o json 2>/dev/null | jq -r '.phase // empty')
        if [ "${last}" = "${want}" ]; then
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
    echo "wait_worker_phase: worker=${worker_name} timed out after ${timeout}s, last_phase='${last}'" >&2
    dump_diagnostics worker "${worker_name}"
    return 1
}

# wait_worker_provisioned <worker_name> [timeout_seconds]
# Stronger than wait_worker_phase: waits until the worker has both a non-
# empty .roomID AND a non-empty .matrixUserID. This is the correct post-
# PR #666 replacement for "grep 'worker created'", because a team member
# is "provisioned" precisely when its room + Matrix user have been
# persisted into Team.Status.Members (or Worker.Status for standalone).
# Does not require phase=Running, so tests that only need credentials
# (e.g. API-key lookup) don't block on container startup.
wait_worker_provisioned() {
    local worker_name="$1"
    local timeout="${2:-180}"
    local elapsed=0
    local room_id=""
    local mxid=""
    while [ "${elapsed}" -lt "${timeout}" ]; do
        local json
        json=$(exec_in_agent agt get workers "${worker_name}" -o json 2>/dev/null)
        room_id=$(echo "${json}" | jq -r '.roomID // empty')
        mxid=$(echo "${json}" | jq -r '.matrixUserID // empty')
        if [ -n "${room_id}" ] && [ -n "${mxid}" ]; then
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
    echo "wait_worker_provisioned: worker=${worker_name} timed out after ${timeout}s, roomID='${room_id}' matrixUserID='${mxid}'" >&2
    dump_diagnostics worker "${worker_name}"
    return 1
}

# wait_worker_model <worker_name> <expected_model> [timeout_seconds]
# Polls the API until the Worker spec model reflects an update.
wait_worker_model() {
    local worker_name="$1"
    local want="$2"
    local timeout="${3:-120}"
    local elapsed=0
    local last=""
    while [ "${elapsed}" -lt "${timeout}" ]; do
        last=$(exec_in_agent agt get workers "${worker_name}" -o json 2>/dev/null | jq -r '.model // empty')
        if [ "${last}" = "${want}" ]; then
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
    echo "wait_worker_model: worker=${worker_name} timed out after ${timeout}s, last_model='${last}', want='${want}'" >&2
    dump_diagnostics worker "${worker_name}"
    return 1
}

# wait_agent_file_contains <agent_name> <relative_path> <needle> [timeout_seconds]
# Polls MinIO for a generated agent file until it contains the expected text.
wait_agent_file_contains() {
    local agent_name="$1"
    local rel_path="$2"
    local needle="$3"
    local timeout="${4:-120}"
    local elapsed=0
    local storage_prefix="${STORAGE_PREFIX:?STORAGE_PREFIX is required}"
    local last=""
    while [ "${elapsed}" -lt "${timeout}" ]; do
        last=$(exec_in_manager mc cat "${storage_prefix}/agents/${agent_name}/${rel_path}" 2>/dev/null || true)
        if echo "${last}" | grep -Fq "${needle}"; then
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
    echo "wait_agent_file_contains: agent=${agent_name} path=${rel_path} timed out after ${timeout}s, want='${needle}'" >&2
    return 1
}

# wait_agent_matrix_allow_contains <agent_name> <jq_array_path> <matrix_id> [timeout_seconds]
# Polls openclaw.json until channels.matrix allow-lists contain the expected ID.
wait_agent_matrix_allow_contains() {
    local agent_name="$1"
    local jq_path="$2"
    local matrix_id="$3"
    local timeout="${4:-120}"
    local elapsed=0
    local storage_prefix="${STORAGE_PREFIX:?STORAGE_PREFIX is required}"
    local last=""
    while [ "${elapsed}" -lt "${timeout}" ]; do
        last=$(exec_in_manager mc cat "${storage_prefix}/agents/${agent_name}/openclaw.json" 2>/dev/null | jq -r "${jq_path}[]?" 2>/dev/null)
        if echo "${last}" | grep -Fq "${matrix_id}"; then
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
    echo "wait_agent_matrix_allow_contains: agent=${agent_name} path=${jq_path} timed out after ${timeout}s, want='${matrix_id}', last='${last}'" >&2
    return 1
}

# get_worker_room_id <worker_name>
# Echoes the worker's .roomID from the API, or empty on failure.
# Works for both standalone workers and team members, since
# ResourceHandler.teamMemberToResponse now populates RoomID from
# Team.Status.Members.
get_worker_room_id() {
    local worker_name="$1"
    exec_in_agent agt get workers "${worker_name}" -o json 2>/dev/null | jq -r '.roomID // empty'
}

worker_container_name() {
    local worker="$1"
    local container
    if [ -n "${TEST_WORKER_CONTAINER_PREFIX:-}" ]; then
        container="$(docker ps -a --format '{{.Names}}' 2>/dev/null | grep -E "^${TEST_WORKER_CONTAINER_PREFIX}${worker}$" | head -1 || true)"
        if [ -n "${container}" ]; then
            printf '%s\n' "${container}"
            return
        fi
    fi
    container="$(docker ps -a --format '{{.Names}}' 2>/dev/null | grep -E "^agentteams-worker-${worker}$" | head -1 || true)"
    container="${container:-$(docker ps -a --format '{{.Names}}' 2>/dev/null | grep -E "^hiclaw-worker-${worker}$" | head -1 || true)}"
    if [ -n "${TEST_WORKER_CONTAINER_PREFIX:-}" ]; then
        container="${container:-${TEST_WORKER_CONTAINER_PREFIX}${worker}}"
    fi
    printf '%s\n' "${container:-agentteams-worker-${worker}}"
}

remove_worker_container() {
    local worker="$1"
    if [ -n "${TEST_WORKER_CONTAINER_PREFIX:-}" ]; then
        docker rm -f "${TEST_WORKER_CONTAINER_PREFIX}${worker}" >/dev/null 2>&1 || true
    fi
    docker rm -f "agentteams-worker-${worker}" "hiclaw-worker-${worker}" >/dev/null 2>&1 || true
}

list_test_worker_containers() {
    docker ps -a --format '{{.Names}}' 2>/dev/null | grep -E "^(agentteams|hiclaw)-worker-test-" || true
}

# Wait for a Worker container to be running (started by Manager on demand)
# Usage: wait_for_worker_container <worker_name> [timeout_seconds]
# Returns 0 when container is running, 1 on timeout
wait_for_worker_container() {
    local worker="$1"
    local timeout="${2:-120}"
    local container
    local elapsed=0

    container="$(worker_container_name "${worker}")"
    log_info "Waiting for Worker container '${container}' to be running (timeout: ${timeout}s)..."
    while [ "${elapsed}" -lt "${timeout}" ]; do
        container="$(worker_container_name "${worker}")"
        if docker ps --format '{{.Names}}' 2>/dev/null | grep -q "^${container}$"; then
            log_info "Worker container '${container}' is running (took ${elapsed}s)"
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done

    log_info "Worker container '${container}' did not start within ${timeout}s" >&2
    dump_diagnostics worker "${worker}"
    return 1
}

# check_copaw_worker_probes <worker_name> [expected_readiness] [request_timeout]
# Verifies CoPaw worker adapter probes from inside the worker container:
#   - GET /worker/livez must return HTTP 200 and liveness=alive
#   - GET /worker/readyz must return HTTP 200+ready or HTTP 503+not_ready
# expected_readiness: any | ready | not_ready (default: any)
check_copaw_worker_probes() {
    local worker="$1"
    local expected="${2:-any}"
    local request_timeout="${3:-75}"
    local container
    local worker_port
    container="$(worker_container_name "${worker}")"

    case "${expected}" in
        any|ready|not_ready) ;;
        *)
            echo "CoPaw worker probes expected readiness is invalid: ${expected}" >&2
            return 1
            ;;
    esac

    if ! docker ps --format '{{.Names}}' 2>/dev/null | grep -q "^${container}$"; then
        echo "CoPaw worker probes skipped: container '${container}' is not running" >&2
        return 2
    fi

    worker_port="$(
        docker exec "${container}" sh -lc '
            if [ -n "${HICLAW_WORKER_PORT:-}" ]; then
                printf "%s" "${HICLAW_WORKER_PORT}"
            else
                console_port="${HICLAW_CONSOLE_PORT:-8088}"
                printf "%s" "$((console_port + 1))"
            fi
        ' 2>/dev/null
    )"
    worker_port="${worker_port:-8089}"

    local probe_output probe_status
    probe_output="$(
        docker exec \
            -e HICLAW_WORKER_PORT="${worker_port}" \
            -e HICLAW_EXPECT_READINESS="${expected}" \
            -e HICLAW_PROBE_REQUEST_TIMEOUT="${request_timeout}" \
            "${container}" \
            python3 -c '
import json
import os
import sys
import time
import urllib.error
import urllib.request

port = os.environ["HICLAW_WORKER_PORT"]
expected = os.environ.get("HICLAW_EXPECT_READINESS", "any")
request_timeout = float(os.environ.get("HICLAW_PROBE_REQUEST_TIMEOUT", "75"))
base_url = f"http://127.0.0.1:{port}"
required_components = {"copaw", "sync", "bridge", "model", "matrix"}

class RetryableProbeError(Exception):
    pass

def get_json(path, accepted_statuses):
    url = base_url + path
    try:
        with urllib.request.urlopen(url, timeout=request_timeout) as resp:
            status = getattr(resp, "status", 200)
            body = resp.read().decode("utf-8")
    except urllib.error.HTTPError as exc:
        status = exc.code
        body = exc.read().decode("utf-8")
    except Exception as exc:
        raise RetryableProbeError(f"{path} request failed: {type(exc).__name__}: {exc}") from exc

    if status not in accepted_statuses:
        print(f"{path} unexpected HTTP status: {status}", file=sys.stderr)
        print(body, file=sys.stderr)
        sys.exit(11)

    try:
        payload = json.loads(body)
    except Exception as exc:
        print(f"{path} invalid JSON: {type(exc).__name__}: {exc}", file=sys.stderr)
        print(body, file=sys.stderr)
        sys.exit(12)
    return status, payload

def validate_once():
    live_status, live = get_json("/worker/livez", {200})
    if live.get("liveness") != "alive":
        print(f"/worker/livez invalid liveness: {live.get('liveness')!r}", file=sys.stderr)
        sys.exit(13)

    ready_status, ready = get_json("/worker/readyz", {200, 503})
    readiness = ready.get("readiness")
    if readiness not in {"ready", "not_ready"}:
        print(f"/worker/readyz invalid readiness: {readiness!r}", file=sys.stderr)
        sys.exit(14)
    if ready_status == 200 and readiness != "ready":
        print(f"/worker/readyz HTTP 200 requires readiness=ready, got {readiness!r}", file=sys.stderr)
        sys.exit(15)
    if ready_status == 503 and readiness != "not_ready":
        print(f"/worker/readyz HTTP 503 requires readiness=not_ready, got {readiness!r}", file=sys.stderr)
        sys.exit(16)
    if expected != "any" and readiness != expected:
        raise RetryableProbeError(f"expected readiness {expected!r}, got {readiness!r}")

    healthiness = ready.get("healthiness")
    if healthiness not in {"healthy", "unhealthy"}:
        print(f"/worker/readyz invalid healthiness: {healthiness!r}", file=sys.stderr)
        sys.exit(18)

    components = ready.get("components")
    if not isinstance(components, dict):
        print("/worker/readyz components must be an object", file=sys.stderr)
        sys.exit(19)
    missing = required_components - set(components)
    if missing:
        print(f"/worker/readyz missing components: {sorted(missing)}", file=sys.stderr)
        sys.exit(20)
    for name in sorted(required_components):
        item = components.get(name)
        if not isinstance(item, dict):
            print(f"/worker/readyz component {name} must be an object", file=sys.stderr)
            sys.exit(21)
        value = item.get("healthiness")
        if value not in {"healthy", "unhealthy"}:
            print(f"/worker/readyz component {name} has invalid healthiness: {value!r}", file=sys.stderr)
            sys.exit(22)
    return live, ready

deadline = time.monotonic() + request_timeout
last_error = None
while True:
    try:
        live, ready = validate_once()
        break
    except RetryableProbeError as exc:
        last_error = exc
        if time.monotonic() >= deadline:
            print(str(last_error), file=sys.stderr)
            sys.exit(10)
        time.sleep(2)

print("== /worker/livez ==")
print(json.dumps(live, ensure_ascii=False, indent=2))
print("== /worker/readyz ==")
print(json.dumps(ready, ensure_ascii=False, indent=2))
' 2>&1
    )"
    probe_status=$?

    printf "%s\n" "${probe_output}"

    if [ "${probe_status}" -ne 0 ]; then
        dump_diagnostics worker "${worker}"
    fi

    return ${probe_status}
}

# ============================================================
# Config Detection
# ============================================================

# Auto-detect configuration from Manager container
# This reads AgentTeams environment variables from the container and sets
# TEST_* variables accordingly. Call this after the container is running.
detect_manager_config() {
    local container="${TEST_CONTROLLER_CONTAINER:-agentteams-controller}"
    
    # Skip if container is not running
    if ! docker ps --format '{{.Names}}' | grep -q "^${container}$"; then
        return 0
    fi
    
    # Read all config and credentials from container environment in one call
    local container_env
    container_env=$(docker exec "${container}" printenv 2>/dev/null) || true

    _cenv() { echo "${container_env}" | grep "^${1}=" | cut -d= -f2-; }

    local detected_domain detected_gateway_port detected_console_port detected_element_port
    detected_domain=$(        _cenv AGENTTEAMS_MATRIX_DOMAIN)
    detected_gateway_port=$(  _cenv AGENTTEAMS_PORT_GATEWAY)
    detected_console_port=$(  _cenv AGENTTEAMS_PORT_CONSOLE)
    detected_element_port=$(  _cenv AGENTTEAMS_PORT_ELEMENT_WEB)
    detected_domain="${detected_domain:-$(_cenv HICLAW_MATRIX_DOMAIN)}"
    detected_gateway_port="${detected_gateway_port:-$(_cenv HICLAW_PORT_GATEWAY)}"
    detected_console_port="${detected_console_port:-$(_cenv HICLAW_PORT_CONSOLE)}"
    detected_element_port="${detected_element_port:-$(_cenv HICLAW_PORT_ELEMENT_WEB)}"

    [ -n "${detected_gateway_port}" ] && export TEST_GATEWAY_PORT="${detected_gateway_port}"
    [ -n "${detected_console_port}" ] && export TEST_CONSOLE_PORT="${detected_console_port}"
    [ -n "${detected_element_port}" ] && export TEST_ELEMENT_PORT="${detected_element_port}"

    # Rebuild derived URLs after port detection
    export TEST_CONSOLE_URL="http://${TEST_MANAGER_HOST}:${TEST_CONSOLE_PORT}"

    if [ -n "${detected_domain}" ] && [ -z "${TEST_MATRIX_DOMAIN}" ]; then
        export TEST_MATRIX_DOMAIN="${detected_domain}"
    elif [ -z "${TEST_MATRIX_DOMAIN}" ]; then
        export TEST_MATRIX_DOMAIN="matrix-local.agentteams.io:${TEST_GATEWAY_PORT}"
    fi

    # Load credentials from container env (only if not already set externally)
    [ -z "${TEST_ADMIN_USER}" ]          && export TEST_ADMIN_USER="$(           _cenv AGENTTEAMS_ADMIN_USER)"
    [ -z "${TEST_ADMIN_USER}" ]          && export TEST_ADMIN_USER="$(           _cenv HICLAW_ADMIN_USER)"
    [ -z "${TEST_ADMIN_PASSWORD}" ]      && export TEST_ADMIN_PASSWORD="$(        _cenv AGENTTEAMS_ADMIN_PASSWORD)"
    [ -z "${TEST_ADMIN_PASSWORD}" ]      && export TEST_ADMIN_PASSWORD="$(        _cenv HICLAW_ADMIN_PASSWORD)"
    [ -z "${TEST_MINIO_USER}" ]          && export TEST_MINIO_USER="$(            _cenv AGENTTEAMS_MINIO_USER)"
    [ -z "${TEST_MINIO_USER}" ]          && export TEST_MINIO_USER="$(            _cenv HICLAW_MINIO_USER)"
    [ -z "${TEST_MINIO_PASSWORD}" ]      && export TEST_MINIO_PASSWORD="$(        _cenv AGENTTEAMS_MINIO_PASSWORD)"
    [ -z "${TEST_MINIO_PASSWORD}" ]      && export TEST_MINIO_PASSWORD="$(        _cenv HICLAW_MINIO_PASSWORD)"
    [ -z "${TEST_REGISTRATION_TOKEN}" ]  && export TEST_REGISTRATION_TOKEN="$(    _cenv AGENTTEAMS_REGISTRATION_TOKEN)"
    [ -z "${TEST_REGISTRATION_TOKEN}" ]  && export TEST_REGISTRATION_TOKEN="$(    _cenv HICLAW_REGISTRATION_TOKEN)"
    [ -z "${HICLAW_LLM_API_KEY}" ]       && export HICLAW_LLM_API_KEY="$(         _cenv AGENTTEAMS_LLM_API_KEY)"
    [ -z "${HICLAW_LLM_API_KEY}" ]       && export HICLAW_LLM_API_KEY="$(         _cenv HICLAW_LLM_API_KEY)"
    [ -z "${TEST_MANAGER_GATEWAY_KEY}" ] && export TEST_MANAGER_GATEWAY_KEY="$(   _cenv AGENTTEAMS_MANAGER_GATEWAY_KEY)"
    [ -z "${TEST_MANAGER_GATEWAY_KEY}" ] && export TEST_MANAGER_GATEWAY_KEY="$(   _cenv HICLAW_MANAGER_GATEWAY_KEY)"
}

# ============================================================
# Test Lifecycle
# ============================================================

test_setup() {
    local test_name="$1"
    log_section "Starting: ${test_name}"
    
    # Auto-detect configuration from Manager container
    detect_manager_config
}

test_teardown() {
    local test_name="$1"
    log_section "Finished: ${test_name}"
}

# Print summary and exit with appropriate code
test_summary() {
    echo ""
    echo "========================================"
    echo "  Test Summary"
    echo "========================================"
    echo "  Total:  ${TESTS_TOTAL}"
    echo -e "  \033[32mPassed: ${TESTS_PASSED}\033[0m"
    echo -e "  \033[31mFailed: ${TESTS_FAILED}\033[0m"
    echo "========================================"

    if [ ${TESTS_FAILED} -gt 0 ]; then
        echo ""
        echo "Failures:"
        for failure in "${TEST_FAILURES[@]}"; do
            echo "  - ${failure}"
        done
        echo ""
        return 1
    fi

    return 0
}

# ============================================================
# LLM / Agent helpers
# ============================================================

# Check if LLM API key is configured (required for tests that need Manager Agent responses)
require_llm_key() {
    if [ -z "${HICLAW_LLM_API_KEY}" ]; then
        log_info "SKIP: No LLM API key configured (set HICLAW_LLM_API_KEY). This test requires Manager Agent LLM responses."
        return 1
    fi
    return 0
}

# ============================================================
# Docker helpers
# ============================================================

# Run a command inside the infrastructure container (Matrix, MinIO, Higress, controller).
# Used by matrix-client.sh and minio-client.sh to avoid exposing Matrix/MinIO ports to host.
exec_in_manager() {
    docker exec "${TEST_CONTROLLER_CONTAINER:-agentteams-controller}" "$@"
}

# Run a command inside the Manager Agent container.
# In legacy mode (all-in-one manager), this falls back to the same container.
# In embedded-controller mode, this targets the separate agent container.
exec_in_agent() {
    docker exec "${TEST_AGENT_CONTAINER:-${TEST_CONTROLLER_CONTAINER:-agentteams-controller}}" "$@"
}

# Copy a file between containers via tar pipe (avoids host filesystem symlink issues on macOS).
# Usage: copy_to_agent <src_path_in_controller> <dst_path_in_agent>
copy_to_agent() {
    local src_path="$1"
    local dst_path="$2"
    local src_dir dst_dir src_file
    src_dir=$(dirname "${src_path}")
    src_file=$(basename "${src_path}")
    dst_dir=$(dirname "${dst_path}")
    exec_in_agent mkdir -p "${dst_dir}" 2>/dev/null
    # Use docker cp via host temp dir for reliability (tar pipe can truncate)
    local tmp_host="/tmp/.hiclaw-copy-$$"
    mkdir -p "${tmp_host}"
    docker cp "${TEST_CONTROLLER_CONTAINER}:${src_path}" "${tmp_host}/${src_file}" 2>/dev/null
    docker cp "${tmp_host}/${src_file}" "${TEST_AGENT_CONTAINER}:${dst_path}" 2>/dev/null
    rm -rf "${tmp_host}"
}

start_worker_container() {
    local worker_name="$1"
    local container_name="agentteams-test-worker-${worker_name}"

    docker run -d \
        --name "${container_name}" \
        --network host \
        -e "AGENTTEAMS_WORKER_NAME=${worker_name}" \
        -e "AGENTTEAMS_MATRIX_URL=http://${TEST_MANAGER_HOST}:${TEST_GATEWAY_PORT}" \
        -e "AGENTTEAMS_AI_GATEWAY_URL=http://${TEST_MANAGER_HOST}:${TEST_GATEWAY_PORT}" \
        -e "AGENTTEAMS_FS_ENDPOINT=http://${TEST_MANAGER_HOST}:9000" \
        -e "AGENTTEAMS_FS_BUCKET=agentteams-storage" \
        -e "AGENTTEAMS_FS_ACCESS_KEY=${TEST_MINIO_USER}" \
        -e "AGENTTEAMS_FS_SECRET_KEY=${TEST_MINIO_PASSWORD}" \
        "agentteams/worker-agent:${AGENTTEAMS_VERSION:-latest}" 2>/dev/null

    echo "${container_name}"
}

stop_worker_container() {
    local container_name="$1"
    docker stop "${container_name}" 2>/dev/null || true
    docker rm "${container_name}" 2>/dev/null || true
}

docker_env_value() {
    local container="$1"
    local key="$2"
    docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "${container}" 2>/dev/null \
        | awk -F= -v k="${key}" '$1 == k {sub(/^[^=]*=/, ""); print; exit}'
}

worker_scoped_minio_stat() {
    local container="$1"
    local worker_name="$2"
    local storage_prefix="$3"
    local controller="${TEST_CONTROLLER_CONTAINER:-agentteams-controller}"
    local bucket_path="${storage_prefix#*/}"
    local access_key secret_key endpoint

    access_key="$(docker_env_value "${container}" AGENTTEAMS_FS_ACCESS_KEY)"
    secret_key="$(docker_env_value "${container}" AGENTTEAMS_FS_SECRET_KEY)"
    endpoint="$(docker_env_value "${container}" AGENTTEAMS_FS_ENDPOINT)"
    endpoint="${endpoint:-http://127.0.0.1:9000}"

    if [ -z "${access_key}" ] || [ -z "${secret_key}" ]; then
        echo "worker scoped stat skipped: AGENTTEAMS_FS_ACCESS_KEY/SECRET_KEY missing in container env"
        return 0
    fi

    echo "worker scoped env: access_key_present=yes secret_key_present=yes endpoint=${endpoint} bucket_path=${bucket_path}"
    docker exec \
        -e "AGENTTEAMS_DBG_ENDPOINT=${endpoint}" \
        -e "AGENTTEAMS_DBG_ACCESS_KEY=${access_key}" \
        -e "AGENTTEAMS_DBG_SECRET_KEY=${secret_key}" \
        "${controller}" sh -lc '
            endpoint=$(printf "%s" "${AGENTTEAMS_DBG_ENDPOINT}" | sed "s#agentteams-controller#127.0.0.1#g")
            mc alias set workerdebug "${endpoint}" "${AGENTTEAMS_DBG_ACCESS_KEY}" "${AGENTTEAMS_DBG_SECRET_KEY}" >/dev/null 2>&1 || {
                echo "worker scoped alias set failed"
                exit 0
            }
            mc stat "workerdebug/'"${bucket_path}"'/agents/'"${worker_name}"'/openclaw.json" 2>&1 || true
            mc alias remove workerdebug >/dev/null 2>&1 || true
        ' 2>&1 || true
}

# ============================================================
# Diagnostics (failure-time dumps)
# ============================================================

# dump_diagnostics <kind> <name>
# Print diagnostic info to stderr when a wait/probe fails. Always returns 0
# so callers can chain it before their own `return 1` / `log_fail`.
#
# kind=worker: worker container logs (race-prone; may be gone) + container
#              state + controller logs filtered for the name + worker CR JSON.
# kind=team:   controller logs filtered for the name + team CR JSON.
#
# Worker container `docker logs` is attempted FIRST because the controller
# may force-delete the container within ~100ms of probe failure; everything
# after that point may show "No such container". The controller container
# itself is long-lived so its logs are always available.
dump_diagnostics() {
    local kind="$1"
    local name="$2"
    local controller="${TEST_CONTROLLER_CONTAINER:-agentteams-controller}"
    local storage_prefix="${STORAGE_PREFIX:-${TEST_STORAGE_PREFIX:-agentteams/agentteams-storage}}"

    {
        case "${kind}" in
            worker)
                local container
                container="$(worker_container_name "${name}")"
                printf "\n--- docker logs %s (last 100 lines) ---\n" "${container}"
                docker logs --tail 100 "${container}" 2>&1 || true
                printf "\n--- container state: %s ---\n" "${container}"
                docker inspect --format='status={{.State.Status}} exit={{.State.ExitCode}} oom={{.State.OOMKilled}} restarts={{.RestartCount}} startedAt={{.State.StartedAt}} finishedAt={{.State.FinishedAt}} error={{.State.Error}}' "${container}" 2>&1 || true
                printf "\n--- root MinIO object stat/list: %s/agents/%s ---\n" "${storage_prefix}" "${name}"
                exec_in_manager sh -lc "mc stat '${storage_prefix}/agents/${name}/openclaw.json' 2>&1 || true; mc ls --recursive '${storage_prefix}/agents/${name}' 2>&1 | head -40 || true" || true
                printf "\n--- worker scoped MinIO stat: %s ---\n" "${name}"
                docker exec "${container}" sh -lc "mc stat 'agentteams/${storage_prefix#*/}/agents/${name}/openclaw.json' 2>&1 || true" 2>&1 || true
                printf "\n--- worker scoped MinIO stat via captured env: %s ---\n" "${name}"
                worker_scoped_minio_stat "${container}" "${name}" "${storage_prefix}"
                printf "\n--- controller logs (recent, filtered for %s) ---\n" "${name}"
                docker logs --tail 1000 "${controller}" 2>&1 \
                    | grep -E "${name}|worker-${name}|MinIO|policy|openclaw.json|recreating|spec changed" | tail -80 || true
                printf "\n--- agt get worker %s ---\n" "${name}"
                exec_in_agent agt get workers "${name}" -o json 2>&1 || true
                ;;
            team)
                printf "\n--- controller logs (recent, filtered for %s) ---\n" "${name}"
                docker logs --tail 300 "${controller}" 2>&1 \
                    | grep -E "${name}|team reconciled|member" | tail -80 || true
                printf "\n--- agt get team %s ---\n" "${name}"
                exec_in_agent agt get teams "${name}" -o json 2>&1 || true
                ;;
            *)
                printf "dump_diagnostics: unknown kind '%s' (name=%s)\n" "${kind}" "${name}"
                ;;
        esac
        printf -- "--- end of diagnostics ---\n"
    } >&2
    return 0
}
