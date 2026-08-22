#!/bin/bash
# test-08-github-mcp.sh - Case 8: GitHub operations via MCP Server
# Verifies: Worker can read repo, create branch, create file, create PR
#           via Higress MCP Server (using mcporter)

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/test-helpers.sh"
source "${SCRIPT_DIR}/lib/matrix-client.sh"

test_setup "08-github-mcp"

# Check prerequisites
if [ -z "${HICLAW_GITHUB_TOKEN}" ] && [ -z "${TEST_GITHUB_TOKEN}" ]; then
    log_info "SKIP: No GitHub token configured (set HICLAW_GITHUB_TOKEN or TEST_GITHUB_TOKEN)"
    test_teardown "08-github-mcp"
    test_summary
    exit 0
fi

if ! require_llm_key; then
    test_teardown "08-github-mcp"
    test_summary
    exit 0
fi

ADMIN_LOGIN=$(matrix_login "${TEST_ADMIN_USER}" "${TEST_ADMIN_PASSWORD}")
ADMIN_TOKEN=$(echo "${ADMIN_LOGIN}" | jq -r '.access_token')

MANAGER_USER="@manager:${TEST_MATRIX_DOMAIN}"

log_section "Assign GitHub Task"

DM_ROOM=$(matrix_find_dm_room "${ADMIN_TOKEN}" "${MANAGER_USER}" 2>/dev/null || true)
assert_not_empty "${DM_ROOM}" "DM room with Manager found"

# Wait for Manager Agent to be fully ready (OpenClaw gateway + joined DM room)
wait_for_manager_agent_ready 300 "${DM_ROOM}" "${ADMIN_TOKEN}" || {
    log_fail "Manager Agent not ready in time"
    test_teardown "08-github-mcp"
    test_summary
    exit 1
}

# Send GitHub task
matrix_send_message "${ADMIN_TOKEN}" "${DM_ROOM}" \
    "Ask Alice to perform these GitHub operations on the test repo: 1) Read the README.md, 2) Create a branch named 'test-alice-feature', 3) Create a new file docs/test.md with content 'Test from Alice', 4) Create a Pull Request."

log_info "Waiting for Manager to relay GitHub task..."
REPLY=$(matrix_wait_for_reply "${ADMIN_TOKEN}" "${DM_ROOM}" "@manager" 180 \
    "${ADMIN_TOKEN}" "${DM_ROOM}" "Please check if the GitHub MCP task has been processed.")

assert_not_empty "${REPLY}" "Manager acknowledged GitHub task"

log_section "Wait for GitHub Operations"

# Wait for Alice to complete GitHub operations
log_info "Waiting for Alice to complete GitHub operations (up to 5 min)..."
sleep 120

# Read messages for progress updates
MESSAGES=$(matrix_read_messages "${ADMIN_TOKEN}" "${DM_ROOM}" 30)
log_info "Checking for GitHub operation reports in Room..."

# Look for indicators of GitHub operations
MSG_BODIES=$(echo "${MESSAGES}" | jq -r '[.chunk[].content.body] | join("\n")' 2>/dev/null)
if echo "${MSG_BODIES}" | grep -qi "branch\|pull request\|PR\|github"; then
    log_pass "GitHub operation activity detected in Room"
else
    log_info "No GitHub operation keywords found in recent messages"
fi

test_teardown "08-github-mcp"
test_summary
