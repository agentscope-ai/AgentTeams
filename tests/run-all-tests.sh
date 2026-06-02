#!/bin/bash
# run-all-tests.sh - Integration test orchestrator
# Builds images, starts Manager, runs all test cases, reports results.
#
# Usage:
#   ./tests/run-all-tests.sh                      # Build + run all tests
#   ./tests/run-all-tests.sh --skip-build          # Use existing images
#   ./tests/run-all-tests.sh --test-filter "01 02"  # Run specific tests only
#   ./tests/run-all-tests.sh --use-existing         # Run against already-installed Manager

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# ============================================================
# Configuration
# ============================================================

SKIP_BUILD=false
USE_EXISTING=false
TEST_FILTER=""
HICLAW_VERSION="${HICLAW_VERSION:-latest}"

# Test environment variables
export TEST_ADMIN_USER="${TEST_ADMIN_USER:-admin}"
export TEST_ADMIN_PASSWORD="${TEST_ADMIN_PASSWORD:-testpassword123}"
export TEST_MINIO_USER="${TEST_MINIO_USER:-${TEST_ADMIN_USER}}"
export TEST_MINIO_PASSWORD="${TEST_MINIO_PASSWORD:-${TEST_ADMIN_PASSWORD}}"
export TEST_REGISTRATION_TOKEN="${TEST_REGISTRATION_TOKEN:-test-reg-token-$(openssl rand -hex 8)}"
export TEST_MATRIX_DOMAIN="${TEST_MATRIX_DOMAIN:-matrix-local.hiclaw.io:18080}"
export TEST_MANAGER_HOST="${TEST_MANAGER_HOST:-127.0.0.1}"
export HICLAW_LLM_API_KEY="${HICLAW_LLM_API_KEY:-}"

# Parse arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --skip-build) SKIP_BUILD=true; shift ;;
        --use-existing) USE_EXISTING=true; SKIP_BUILD=true; shift ;;
        --test-filter) TEST_FILTER="$2"; shift 2 ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

# Load credentials from hiclaw-manager.env into TEST_* variables
load_env_file() {
    # Install script saves to ${HOME}/hiclaw-manager.env; fall back to project root for compat.
    local env_file="${HICLAW_ENV_FILE:-${HOME}/hiclaw-manager.env}"
    [ -f "${env_file}" ] || env_file="${PROJECT_ROOT}/hiclaw-manager.env"
    if [ -f "${env_file}" ]; then
        while IFS='=' read -r key value; do
            [[ "${key}" =~ ^#.*$ || -z "${key}" ]] && continue
            key=$(echo "${key}" | xargs)
            case "${key}" in
                HICLAW_ADMIN_USER)          export TEST_ADMIN_USER="${value}" ;;
                HICLAW_ADMIN_PASSWORD)      export TEST_ADMIN_PASSWORD="${value}" ;;
                HICLAW_MINIO_USER)          export TEST_MINIO_USER="${value}" ;;
                HICLAW_MINIO_PASSWORD)      export TEST_MINIO_PASSWORD="${value}" ;;
                HICLAW_REGISTRATION_TOKEN)  export TEST_REGISTRATION_TOKEN="${value}" ;;
                HICLAW_MATRIX_DOMAIN)       export TEST_MATRIX_DOMAIN="${value}" ;;
                HICLAW_LLM_API_KEY)         [ -z "${HICLAW_LLM_API_KEY}" ] && export HICLAW_LLM_API_KEY="${value}" ;;
HICLAW_PORT_GATEWAY)        export TEST_GATEWAY_PORT="${value}" ;;
                HICLAW_PORT_CONSOLE)        export TEST_CONSOLE_PORT="${value}" ;;
            esac
        done < "${env_file}"
    fi
    export TEST_CONTROLLER_CONTAINER="hiclaw-controller"
}

if [ "${USE_EXISTING}" = true ]; then
    load_env_file
fi

# ============================================================
# Utilities
# ============================================================

log() {
    echo -e "\033[36m[ORCHESTRATOR]\033[0m $1"
}

error() {
    echo -e "\033[31m[ORCHESTRATOR ERROR]\033[0m $1" >&2
}

_filter_has_test() {
    local filter="$1"
    local test_num="$2"

    printf '%s\n' "${filter}" | grep -qw -- "${test_num}"
}

_expand_controller_cr_filter_for_ci() {
    local filter="$1"
    local expanded="${filter}"
    local required_old_shard="15 17 18 19 20 100"
    local new_controller_tests="22 23 24 25"
    local test_num

    for test_num in ${required_old_shard}; do
        if ! _filter_has_test "${filter}" "${test_num}"; then
            printf '%s\n' "${filter}"
            return 0
        fi
    done

    for test_num in ${new_controller_tests}; do
        if ls "${SCRIPT_DIR}"/test-"${test_num}"-*.sh >/dev/null 2>&1 \
            && ! _filter_has_test "${expanded}" "${test_num}"; then
            expanded="${expanded} ${test_num}"
        fi
    done

    printf '%s\n' "${expanded}"
}

# pull_request_target runs the workflow definition from the base branch, so a PR
# that only updates SHARD_C_TESTS would not exercise newly added tests until
# after merge. The checked-out test runner is from the PR HEAD, so expand the
# legacy controller shard here as a compatibility bridge for CI.
if [ "${GITHUB_ACTIONS:-}" = "true" ] && [ -n "${TEST_FILTER}" ]; then
    EXPANDED_TEST_FILTER="$(_expand_controller_cr_filter_for_ci "${TEST_FILTER}")"
    if [ "${EXPANDED_TEST_FILTER}" != "${TEST_FILTER}" ]; then
        log "Expanded controller-cr test filter: ${TEST_FILTER} -> ${EXPANDED_TEST_FILTER}"
        TEST_FILTER="${EXPANDED_TEST_FILTER}"
    fi
fi

cleanup() {
    if [ "${USE_EXISTING}" = true ]; then
        log "Using existing installation — skipping container cleanup"
        # Still clean up test worker containers
        for c in $(docker ps -a --filter "name=hiclaw-test-worker-" --format '{{.Names}}' 2>/dev/null); do
            docker rm -f "$c" 2>/dev/null || true
        done
        return
    fi

    log "Cleaning up..."
    docker stop hiclaw-controller 2>/dev/null || true
    docker rm hiclaw-controller 2>/dev/null || true
    # Legacy container name
    docker stop hiclaw-manager 2>/dev/null || true
    docker rm hiclaw-manager 2>/dev/null || true

    # Cleanup worker containers
    for c in $(docker ps -a --filter "name=hiclaw-test-worker-" --format '{{.Names}}' 2>/dev/null); do
        docker rm -f "$c" 2>/dev/null || true
    done

    log "Cleanup complete"
}

trap cleanup EXIT

# ============================================================
# Step 1: Build images
# ============================================================

if [ "${SKIP_BUILD}" = false ]; then
    log "Building images via Makefile..."
    make -C "${PROJECT_ROOT}" build VERSION="${HICLAW_VERSION}"
    log "Images built successfully"
else
    log "Skipping image build (--skip-build)"
fi

# ============================================================
# Step 2: Start Manager container (skip if --use-existing)
# ============================================================

if [ "${USE_EXISTING}" = true ]; then
    log "Using existing Manager installation (--use-existing)"
    log "  Admin user: ${TEST_ADMIN_USER}"
    log "  Matrix domain: ${TEST_MATRIX_DOMAIN}"
    log "  Manager host: ${TEST_MANAGER_HOST}"

    # Verify the Manager is actually running (Matrix is not exposed; check via docker exec)
    if ! docker exec "${TEST_CONTROLLER_CONTAINER}" curl -sf "http://127.0.0.1:6167/_matrix/client/versions" > /dev/null 2>&1; then
        error "Manager does not appear to be running (container: ${TEST_CONTROLLER_CONTAINER}). Start it with 'make install' first."
    fi
    log "Manager is reachable"

    # Enable YOLO mode for test run (auto-decision, no interactive prompts)
    # Try agent container first (embedded mode), fall back to manager container (legacy mode)
    agent_container="$(docker ps --format '{{.Names}}' 2>/dev/null | grep -E '^hiclaw-manager(-|$)' | head -1)"
    agent_container="${agent_container:-${TEST_CONTROLLER_CONTAINER}}"
    docker exec "${agent_container}" touch /root/manager-workspace/yolo-mode 2>/dev/null && \
        log "YOLO mode enabled (${agent_container})" || \
        log "WARNING: Could not enable YOLO mode (container may differ)"
else
    log "Installing Manager via install script..."

    # Clean up any existing installation, then install fresh using hiclaw-install.sh.
    # This ensures ports, domains, and all initialization (Higress routes, Matrix users)
    # match exactly what users get in production.
    make -C "${PROJECT_ROOT}" uninstall 2>/dev/null || true
    HICLAW_NON_INTERACTIVE=1 HICLAW_YOLO=1 HICLAW_MOUNT_SOCKET=1 \
        HICLAW_INSTALL_MANAGER_IMAGE="hiclaw/manager-agent:${HICLAW_VERSION}" \
        HICLAW_INSTALL_WORKER_IMAGE="hiclaw/worker-agent:${HICLAW_VERSION}" \
        make -C "${PROJECT_ROOT}" install SKIP_BUILD=1

    # ============================================================
    # Step 3: Wait for Manager to be healthy (via make wait-ready)
    # ============================================================

    make -C "${PROJECT_ROOT}" wait-ready

    # Load all configuration from the env file generated by the install script
    load_env_file
    log "  Admin user:     ${TEST_ADMIN_USER}"
    log "  Matrix domain:  ${TEST_MATRIX_DOMAIN}"
    log "  Gateway port:   ${TEST_GATEWAY_PORT}"
    log "  Console port:   ${TEST_CONSOLE_PORT}"

    # Enable YOLO mode for test run
    agent_container="$(docker ps --format '{{.Names}}' 2>/dev/null | grep -E '^hiclaw-manager(-|$)' | head -1)"
    agent_container="${agent_container:-${TEST_CONTROLLER_CONTAINER}}"
    docker exec "${agent_container}" touch /root/manager-workspace/yolo-mode 2>/dev/null && \
        log "YOLO mode enabled (${agent_container})" || true
fi

# ============================================================
# Step 3.5: Configure Manager identity (English, for test consistency)
# ============================================================
# The welcome message triggers onboarding Q&A. Send identity setup
# so Manager uses English regardless of host timezone/locale.

source "${SCRIPT_DIR}/lib/matrix-client.sh"
source "${SCRIPT_DIR}/lib/agent-metrics.sh"

_setup_manager_identity() {
    log "Configuring Manager identity (English)..."

    local admin_login admin_token dm_room manager_user
    admin_login=$(matrix_login "${TEST_ADMIN_USER}" "${TEST_ADMIN_PASSWORD}" 2>/dev/null) || {
        log "WARNING: Could not login as admin for identity setup"
        return 0
    }
    admin_token=$(echo "${admin_login}" | jq -r '.access_token')
    manager_user="@manager:${TEST_MATRIX_DOMAIN}"

    dm_room=$(matrix_find_dm_room "${admin_token}" "${manager_user}" 2>/dev/null || true)
    if [ -z "${dm_room}" ]; then
        log "WARNING: No DM room found for identity setup"
        return 0
    fi

    # Check if identity is already configured
    # Check in agent container (embedded mode) or manager container (legacy mode)
    local _agent
    _agent="$(docker ps --format '{{.Names}}' 2>/dev/null | grep -E '^hiclaw-manager(-|$)' | head -1)"
    _agent="${_agent:-${TEST_CONTROLLER_CONTAINER}}"

    if docker exec "${_agent}" test -f /root/manager-workspace/soul-configured 2>/dev/null; then
        log "Manager identity already configured, skipping"
        return 0
    fi

    # Wait for Manager Agent to be ready
    wait_for_manager_agent_ready 300 "${dm_room}" "${admin_token}" || {
        log "WARNING: Manager not ready for identity setup"
        return 0
    }

    # Verify Gateway consumer and AI route authorization before sending messages
    log "Verifying Gateway authorization for Manager..."
    local _gw_ready=false _gw_elapsed=0
    local _console_url="http://${TEST_MANAGER_HOST}:${TEST_CONSOLE_PORT:-18001}"
    local _gw_url="http://${TEST_MANAGER_HOST}:${TEST_GATEWAY_PORT:-18080}"
    local _cookie_file="/tmp/higress-test-cookie-$$"
    local _mgr_key
    _mgr_key=$(grep HICLAW_MANAGER_GATEWAY_KEY "${HICLAW_ENV_FILE:-${HOME}/hiclaw-manager.env}" 2>/dev/null | cut -d= -f2-)
    while [ "${_gw_elapsed}" -lt 60 ]; do
        # Login to Higress console and check manager consumer
        curl -sf -X POST "${_console_url}/session/login" \
            -H 'Content-Type: application/json' \
            -c "${_cookie_file}" \
            -d '{"username":"'"${TEST_ADMIN_USER}"'","password":"'"${TEST_ADMIN_PASSWORD}"'"}' >/dev/null 2>&1 || true
        if curl -sf "${_console_url}/v1/consumers" -b "${_cookie_file}" 2>/dev/null | grep -q '"manager"'; then
            if [ -n "${_mgr_key}" ]; then
                # Test actual LLM call through gateway with a minimal chat completion request
                local _gw_resp _gw_code
                _gw_resp=$(curl -s -w "\n%{http_code}" \
                    -X POST "${_gw_url}/v1/chat/completions" \
                    -H "Authorization: Bearer ${_mgr_key}" \
                    -H "Content-Type: application/json" \
                    -d '{"model":"'"${HICLAW_DEFAULT_MODEL:-qwen3.6-plus}"'","messages":[{"role":"user","content":"hi"}],"max_tokens":1}' 2>/dev/null || echo -e "\n000")
                _gw_code=$(echo "${_gw_resp}" | tail -1)
                if [ "${_gw_code}" = "200" ]; then
                    _gw_ready=true
                    break
                elif [ "${_gw_code}" != "401" ] && [ "${_gw_code}" != "403" ]; then
                    # Non-auth error (e.g. 400, 500) — gateway auth is working, model may just be wrong
                    log "Gateway returned HTTP ${_gw_code} (non-auth error, authorization is working)"
                    _gw_ready=true
                    break
                fi
                log "Gateway returned HTTP ${_gw_code}, retrying... (${_gw_elapsed}s/60s)"
            fi
        fi
        sleep 2
        _gw_elapsed=$((_gw_elapsed + 2))
    done
    rm -f "${_cookie_file}"
    if [ "${_gw_ready}" != "true" ]; then
        local _last_body
        _last_body=$(echo "${_gw_resp}" | sed '$d')
        error "Gateway authorization not ready after 60s (HTTP ${_gw_code})"
        error "Response: ${_last_body}"
        exit 1
    fi
    log "Gateway authorization verified"

    # Send identity setup message
    matrix_send_message "${admin_token}" "${dm_room}" \
        "Here is my identity configuration for you:
- Name: Manager
- Language: English (always respond in English)
- Style: concise and professional
- No special constraints

Please update your SOUL.md with these preferences, then run: touch ~/soul-configured"

    log "Waiting for Manager to configure identity..."

    # Wait for Manager to process and touch soul-configured (up to 120s)
    local elapsed=0
    while [ "${elapsed}" -lt 120 ]; do
        if docker exec "${_agent}" test -f /root/manager-workspace/soul-configured 2>/dev/null; then
            # soul-configured exists, but Manager's Matrix reply may still be in flight.
            # Wait for the reply to arrive in the DM room so subsequent tests don't
            # pick it up as their own reply (race condition with test-02).
            local _wait=0
            while [ "${_wait}" -lt 30 ]; do
                local _reply
                _reply=$(matrix_read_messages "${admin_token}" "${dm_room}" 5 2>/dev/null | \
                    jq -r '[.chunk[] | select(.sender | startswith("@manager")) | .content.body] | first // ""' 2>/dev/null)
                if echo "${_reply}" | grep -qi "soul\|identity\|configured\|language\|english\|ready\|activated"; then
                    break
                fi
                sleep 3
                _wait=$((_wait + 3))
            done
            log "Manager identity configured successfully"
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done

    log "WARNING: Manager did not complete identity setup within 120s (tests will continue)"
    return 0
}

_setup_manager_identity

# ============================================================
# Step 4: Run test cases
# ============================================================

log "Running integration tests..."
echo ""

TOTAL_PASS=0
TOTAL_FAIL=0
RESULTS=()

# Determine which tests to run
TESTS=()
for test_file in "${SCRIPT_DIR}"/test-*.sh; do
    test_num=$(basename "${test_file}" | grep -o '[0-9]\+')
    if [ -n "${TEST_FILTER}" ]; then
        if echo "${TEST_FILTER}" | grep -qw "${test_num}"; then
            TESTS+=("${test_file}")
        fi
    else
        TESTS+=("${test_file}")
    fi
done

for test_file in "${TESTS[@]}"; do
    test_name=$(basename "${test_file}" .sh)
    log "Running: ${test_name}"

    # Wait for Manager to finish processing previous test before starting next
    wait_for_session_stable 10 120

    if bash "${test_file}"; then
        RESULTS+=("PASS: ${test_name}")
        TOTAL_PASS=$((TOTAL_PASS + 1))
    else
        RESULTS+=("FAIL: ${test_name}")
        TOTAL_FAIL=$((TOTAL_FAIL + 1))
    fi

    echo ""
done

# ============================================================
# Step 5: Report results
# ============================================================

echo ""
echo "========================================"
echo "  Integration Test Results"
echo "========================================"
echo "  Total:  $((TOTAL_PASS + TOTAL_FAIL))"
echo -e "  \033[32mPassed: ${TOTAL_PASS}\033[0m"
echo -e "  \033[31mFailed: ${TOTAL_FAIL}\033[0m"
echo "========================================"
echo ""

for result in "${RESULTS[@]}"; do
    if [[ "${result}" == PASS* ]]; then
        echo -e "  \033[32m${result}\033[0m"
    else
        echo -e "  \033[31m${result}\033[0m"
    fi
done

echo ""

if [ "${TOTAL_FAIL}" -gt 0 ]; then
    error "${TOTAL_FAIL} test(s) failed"
    exit 1
fi

log "All tests passed!"
exit 0
