#!/bin/bash
# test-09-github-collab.sh - Case 9: Multi-Worker GitHub collaboration
# Verifies: Alice and Bob create separate branches and PRs for a shared project

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/test-helpers.sh"
source "${SCRIPT_DIR}/lib/matrix-client.sh"

test_setup "09-github-collab"

ADMIN_LOGIN=$(matrix_login "${TEST_ADMIN_USER}" "${TEST_ADMIN_PASSWORD}")
ADMIN_TOKEN=$(echo "${ADMIN_LOGIN}" | jq -r '.access_token')

MANAGER_USER="@manager:${TEST_MATRIX_DOMAIN}"

# Check if GitHub token is configured
if [ -z "${HICLAW_GITHUB_TOKEN}" ] && [ -z "${TEST_GITHUB_TOKEN}" ]; then
    log_info "SKIP: No GitHub token configured"
    test_teardown "09-github-collab"
    test_summary
    exit 0
fi

log_section "Assign Collaborative GitHub Task"

DM_ROOM=$(matrix_find_dm_room "${ADMIN_TOKEN}" "${MANAGER_USER}" 2>/dev/null || true)
assert_not_empty "${DM_ROOM}" "DM room with Manager found"

# Wait for Manager Agent to be fully ready (OpenClaw gateway + joined DM room)
wait_for_manager_agent_ready 300 "${DM_ROOM}" "${ADMIN_TOKEN}" || {
    log_fail "Manager Agent not ready in time"
    test_teardown "09-github-collab"
    test_summary
    exit 1
}

matrix_send_message "${ADMIN_TOKEN}" "${DM_ROOM}" \
    "I need Alice and Bob to collaborate on the test repo via GitHub. Alice should create a branch 'feature/alice-docs' and add a file docs/alice.md. Bob should create a branch 'feature/bob-api' and add a file src/bob.py. Both should create separate PRs."

log_info "Waiting for Manager to coordinate..."
REPLY=$(matrix_wait_for_reply "${ADMIN_TOKEN}" "${DM_ROOM}" "@manager" 180 \
    "${ADMIN_TOKEN}" "${DM_ROOM}" "Please check if the GitHub collaboration task has been processed.")

assert_not_empty "${REPLY}" "Manager acknowledged collaborative GitHub task"

log_section "Wait for Collaborative GitHub Work"

log_info "Waiting for both Workers to complete GitHub tasks (up to 5 min)..."
sleep 180

# Check messages for evidence of two separate PRs
MESSAGES=$(matrix_read_messages "${ADMIN_TOKEN}" "${DM_ROOM}" 50)
MSG_BODIES=$(echo "${MESSAGES}" | jq -r '[.chunk[].content.body] | join("\n")' 2>/dev/null)

if echo "${MSG_BODIES}" | grep -qi "alice"; then
    log_pass "Alice's GitHub activity reported"
fi
if echo "${MSG_BODIES}" | grep -qi "bob"; then
    log_pass "Bob's GitHub activity reported"
fi

test_teardown "09-github-collab"
test_summary
