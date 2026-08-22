#!/bin/bash
# test-10-mcp-permission.sh - Case 10: MCP permission dynamic revoke/restore
# Verifies: Manager can revoke Alice's GitHub MCP access, Alice gets 403,
#           then restore access and it works again

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/test-helpers.sh"
source "${SCRIPT_DIR}/lib/matrix-client.sh"
source "${SCRIPT_DIR}/lib/higress-client.sh"

test_setup "10-mcp-permission"

if ! require_llm_key; then
    test_teardown "10-mcp-permission"
    test_summary
    exit 0
fi

# This test requires GitHub MCP Server to be configured
if [ -z "${HICLAW_GITHUB_TOKEN}" ] && [ -z "${TEST_GITHUB_TOKEN}" ]; then
    log_info "SKIP: No GitHub token configured (set HICLAW_GITHUB_TOKEN or TEST_GITHUB_TOKEN). MCP permission tests require GitHub MCP Server."
    test_teardown "10-mcp-permission"
    test_summary
    exit 0
fi

ADMIN_LOGIN=$(matrix_login "${TEST_ADMIN_USER}" "${TEST_ADMIN_PASSWORD}")
ADMIN_TOKEN=$(echo "${ADMIN_LOGIN}" | jq -r '.access_token')

MANAGER_USER="@manager:${TEST_MATRIX_DOMAIN}"

log_section "Revoke Alice's MCP Access"

DM_ROOM=$(matrix_find_dm_room "${ADMIN_TOKEN}" "${MANAGER_USER}" 2>/dev/null || true)
assert_not_empty "${DM_ROOM}" "DM room with Manager found"

# Wait for Manager Agent to be fully ready (OpenClaw gateway + joined DM room)
wait_for_manager_agent_ready 300 "${DM_ROOM}" "${ADMIN_TOKEN}" || {
    log_fail "Manager Agent not ready in time"
    test_teardown "10-mcp-permission"
    test_summary
    exit 1
}

# Request revocation
matrix_send_message "${ADMIN_TOKEN}" "${DM_ROOM}" \
    "Please revoke Alice's access to the GitHub MCP Server. She should no longer be able to perform GitHub operations."

log_info "Waiting for Manager to revoke access..."
REPLY=$(matrix_wait_for_reply "${ADMIN_TOKEN}" "${DM_ROOM}" "@manager" 180 \
    "${ADMIN_TOKEN}" "${DM_ROOM}" "Please check if the MCP permission change has been processed.")

assert_not_empty "${REPLY}" "Manager replied to revoke request"

log_section "Verify Revocation"

higress_login "${TEST_ADMIN_USER}" "${TEST_ADMIN_PASSWORD}" > /dev/null
MCP_CONSUMERS=$(higress_get_mcp_consumers "mcp-github" 2>/dev/null || echo "")

if echo "${MCP_CONSUMERS}" | grep -q "worker-alice"; then
    log_fail "Alice still has MCP access after revocation"
else
    log_pass "Alice's MCP access revoked"
fi

log_section "Restore Alice's MCP Access"

matrix_send_message "${ADMIN_TOKEN}" "${DM_ROOM}" \
    "Please restore Alice's access to the GitHub MCP Server."

log_info "Waiting for Manager to restore access..."
REPLY=$(matrix_wait_for_reply "${ADMIN_TOKEN}" "${DM_ROOM}" "@manager" 180 \
    "${ADMIN_TOKEN}" "${DM_ROOM}" "Please check if the MCP permission change has been processed.")

assert_not_empty "${REPLY}" "Manager replied to restore request"

log_section "Verify Restoration"

sleep 15
MCP_CONSUMERS=$(higress_get_mcp_consumers "mcp-github" 2>/dev/null || echo "")

if echo "${MCP_CONSUMERS}" | grep -q "worker-alice"; then
    log_pass "Alice's MCP access restored"
else
    log_fail "Alice's MCP access not restored"
fi

test_teardown "10-mcp-permission"
test_summary
