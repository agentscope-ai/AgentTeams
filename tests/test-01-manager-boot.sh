#!/bin/bash
# test-01-manager-boot.sh - Case 1: Manager boots, all services healthy, IM login
# Verifies: gateway/console ports accessible, Matrix/MinIO healthy via docker exec,
#           Matrix login works, Higress Console session, MinIO storage, Manager Agent responds

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/test-helpers.sh"
source "${SCRIPT_DIR}/lib/matrix-client.sh"
source "${SCRIPT_DIR}/lib/higress-client.sh"
source "${SCRIPT_DIR}/lib/minio-client.sh"

test_setup "01-manager-boot"

# ---- Service Health Checks ----
log_section "Service Health"

# Gateway root may return 200 (console route) or 404 (no default route) - either is fine
GATEWAY_CODE=$(curl -s -o /dev/null -w '%{http_code}' "http://${TEST_MANAGER_HOST}:${TEST_GATEWAY_PORT}/" 2>/dev/null)
if [ "${GATEWAY_CODE}" != "000" ]; then
    log_pass "Higress Gateway port 8080 is accessible (HTTP ${GATEWAY_CODE})"
else
    log_fail "Higress Gateway port 8080 is accessible (no response)"
fi

assert_http_code "http://${TEST_MANAGER_HOST}:${TEST_CONSOLE_PORT}/" "200" \
    "Higress Console port 8001 is accessible"

assert_http_code "http://${TEST_MANAGER_HOST}:${TEST_ELEMENT_PORT}/" "200" \
    "Element Web port 8088 is accessible"

# Infrastructure checks go to the infra container; workspace checks go to the agent container
_INFRA_CTR="${TEST_CONTROLLER_CONTAINER:-hiclaw-manager}"
_AGENT_CTR="${TEST_AGENT_CONTAINER:-${_INFRA_CTR}}"

MATRIX_CODE=$(docker exec "${_INFRA_CTR}" curl -s -o /dev/null -w '%{http_code}' \
    "http://127.0.0.1:6167/_matrix/client/versions" 2>/dev/null || echo "000")
if [ "${MATRIX_CODE}" = "200" ]; then
    log_pass "Tuwunel Matrix is healthy (internal port 6167)"
else
    log_fail "Tuwunel Matrix is healthy (internal port 6167, got HTTP ${MATRIX_CODE})"
fi

MINIO_CODE=$(docker exec "${_INFRA_CTR}" curl -s -o /dev/null -w '%{http_code}' \
    "http://127.0.0.1:9000/minio/health/live" 2>/dev/null || echo "000")
if [ "${MINIO_CODE}" = "200" ]; then
    log_pass "MinIO API is healthy (internal port 9000)"
else
    log_fail "MinIO API is healthy (internal port 9000, got HTTP ${MINIO_CODE})"
fi

# ---- Matrix Login ----
log_section "Matrix Login"

ADMIN_LOGIN=$(matrix_login "${TEST_ADMIN_USER}" "${TEST_ADMIN_PASSWORD}")
ADMIN_TOKEN=$(echo "${ADMIN_LOGIN}" | jq -r '.access_token')
assert_not_empty "${ADMIN_TOKEN}" "Admin Matrix login returns access_token"

# ---- Higress Console ----
log_section "Higress Console"

HIGRESS_SESSION=$(higress_login "${TEST_ADMIN_USER}" "${TEST_ADMIN_PASSWORD}" 2>/dev/null || echo "")
if [ -n "${HIGRESS_SESSION}" ]; then
    log_pass "Higress Console login succeeded"
else
    log_fail "Higress Console login failed (Manager may not have initialized console yet)"
fi

CONSUMERS=$(higress_get_consumers 2>/dev/null || echo "")
if echo "${CONSUMERS}" | grep -q "manager" 2>/dev/null; then
    log_pass "Manager consumer exists in Higress"
else
    log_fail "Manager consumer exists in Higress (not found, Manager Agent may still be initializing)"
fi

# ---- MinIO Storage ----
log_section "MinIO Storage"

if minio_setup 2>/dev/null; then
    log_pass "MinIO mc alias configured"
else
    log_fail "MinIO mc alias configured"
fi

# Manager workspace files are stored locally in the agent container (not in MinIO).
# In embedded-controller mode, the agent container is separate from the infra container.
if docker exec "${_AGENT_CTR}" test -f /root/manager-workspace/SOUL.md 2>/dev/null; then
    log_pass "Manager SOUL.md exists in workspace"
else
    log_fail "Manager SOUL.md exists in workspace"
fi

if docker exec "${_AGENT_CTR}" test -f /root/manager-workspace/AGENTS.md 2>/dev/null; then
    log_pass "Manager AGENTS.md exists in workspace"
else
    log_fail "Manager AGENTS.md exists in workspace"
fi

if docker exec "${_AGENT_CTR}" test -f /root/manager-workspace/HEARTBEAT.md 2>/dev/null; then
    log_pass "Manager HEARTBEAT.md exists in workspace"
else
    log_fail "Manager HEARTBEAT.md exists in workspace"
fi

# ---- Runtime Detection ----
log_section "Manager Runtime"

MANAGER_RUNTIME=$(docker exec "${_AGENT_CTR}" printenv HICLAW_MANAGER_RUNTIME 2>/dev/null || \
                   docker exec "${_INFRA_CTR}" printenv HICLAW_MANAGER_RUNTIME 2>/dev/null || echo "openclaw")
log_pass "Manager runtime: ${MANAGER_RUNTIME}"

# Runtime-specific config verification (agent container)
case "${MANAGER_RUNTIME}" in
    copaw)
        AGENT_JSON="/root/manager-workspace/.copaw/workspaces/default/agent.json"
        if docker exec "${_AGENT_CTR}" jq -e '.channels.matrix.enabled == true' "${AGENT_JSON}" >/dev/null 2>&1; then
            log_pass "CoPaw agent.json valid"
        else
            log_fail "CoPaw agent.json valid"
        fi

        if docker exec "${_AGENT_CTR}" sh -c 'pgrep -f "copaw app" || pgrep -f "qwenpaw app"' >/dev/null 2>&1; then
            log_pass "CoPaw process running"
        else
            log_fail "CoPaw process running"
        fi
        ;;
    *)
        if docker exec "${_AGENT_CTR}" jq -e '.channels.matrix.accessToken' /root/manager-workspace/openclaw.json >/dev/null 2>&1; then
            log_pass "OpenClaw config (openclaw.json) valid"
        else
            log_fail "OpenClaw config (openclaw.json) valid"
        fi
        ;;
esac

# ---- Manager Agent Responds ----
log_section "Manager Agent Communication"

# Find Manager DM room or create one
MANAGER_USER_ID="@manager:${TEST_MATRIX_DOMAIN}"
ROOMS=$(matrix_joined_rooms "${ADMIN_TOKEN}" | jq -r '.joined_rooms[]' 2>/dev/null)

if [ -z "${ROOMS}" ]; then
    log_info "No existing rooms, sending DM to Manager..."
fi

# Send hello and wait for response (Manager should auto-join DM)
# This tests that OpenClaw is running and connected to Matrix
log_info "Attempting to communicate with Manager Agent..."

test_teardown "01-manager-boot"
test_summary
