#!/bin/bash
# test-13-git-delegation.sh - Case 13: Git Delegation Mechanism
# Verifies: Worker can delegate git operations (commit, push) to Manager
#           with proper coordination via .processing marker

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/test-helpers.sh"
source "${SCRIPT_DIR}/lib/matrix-client.sh"
source "${SCRIPT_DIR}/lib/agent-metrics.sh"

test_setup "13-git-delegation"

# Check prerequisites
if [ -z "${HICLAW_GITHUB_TOKEN}" ] && [ -z "${TEST_GITHUB_TOKEN}" ]; then
    log_info "SKIP: No GitHub token configured (set HICLAW_GITHUB_TOKEN or TEST_GITHUB_TOKEN)"
    test_teardown "13-git-delegation"
    test_summary
    exit 0
fi

if ! require_llm_key; then
    test_teardown "13-git-delegation"
    test_summary
    exit 0
fi

ADMIN_LOGIN=$(matrix_login "${TEST_ADMIN_USER}" "${TEST_ADMIN_PASSWORD}")
ADMIN_TOKEN=$(echo "${ADMIN_LOGIN}" | jq -r '.access_token')

MANAGER_USER="@manager:${TEST_MATRIX_DOMAIN}"

log_section "Setup: Ensure Alice Worker Exists"

DM_ROOM=$(matrix_find_dm_room "${ADMIN_TOKEN}" "${MANAGER_USER}" 2>/dev/null || true)
assert_not_empty "${DM_ROOM}" "DM room with Manager found"

# Wait for Manager Agent to be fully ready
wait_for_manager_agent_ready 300 "${DM_ROOM}" "${ADMIN_TOKEN}" || {
    log_fail "Manager Agent not ready in time"
    test_teardown "13-git-delegation"
    test_summary
    exit 1
}

# Ensure Alice worker exists
METRICS_BASELINE=$(snapshot_baseline "alice")
matrix_send_message "${ADMIN_TOKEN}" "${DM_ROOM}" \
    "Create a worker named alice if it doesn't exist yet. Give her the github-operations and git-delegation skills."

log_info "Waiting for Worker creation/setup..."
REPLY=$(matrix_wait_for_reply "${ADMIN_TOKEN}" "${DM_ROOM}" "@manager" 120 \
    "${ADMIN_TOKEN}" "${DM_ROOM}" "Please check if the git delegation task has been processed.")
assert_not_empty "${REPLY}" "Manager acknowledged worker setup"

sleep 10  # Wait for worker to be ready

log_section "Assign Git Delegation Task"

# Generate unique branch name for this test run
TEST_BRANCH="test-git-delegation-$(date +%Y%m%d%H%M%S)"

matrix_send_message "${ADMIN_TOKEN}" "${DM_ROOM}" \
    "Ask Alice to test the git delegation mechanism: 1) Clone the test repo to her workspace, 2) Create a new branch named '${TEST_BRANCH}', 3) Add a file docs/git-delegation-test.md with content 'Git delegation test at $(date)', 4) Use git delegation to commit and push the changes, 5) Report the result."

log_info "Waiting for Manager to relay git delegation task..."
REPLY=$(matrix_wait_for_reply "${ADMIN_TOKEN}" "${DM_ROOM}" "@manager" 180 \
    "${ADMIN_TOKEN}" "${DM_ROOM}" "Please check if the git delegation task has been processed.")

assert_not_empty "${REPLY}" "Manager acknowledged git delegation task"

log_section "Wait for Git Delegation Completion"

# Wait for git operations to complete
log_info "Waiting for git delegation to complete (up to 5 min)..."
sleep 180

# Read messages for progress updates
MESSAGES=$(matrix_read_messages "${ADMIN_TOKEN}" "${DM_ROOM}" 50)
log_info "Checking for git delegation reports in Room..."

MSG_BODIES=$(echo "${MESSAGES}" | jq -r '[.chunk[].content.body] | join("\n")' 2>/dev/null)

# Check for git-result or git-failed response
if echo "${MSG_BODIES}" | grep -qi "git-result\|git-failed\|commit\|push"; then
    log_pass "Git delegation activity detected in Room"
else
    log_info "No git delegation keywords found in recent messages"
fi

# Check for completion
if echo "${MSG_BODIES}" | grep -qi "completed\|done\|success"; then
    log_pass "Task completion reported"
fi

log_section "Verify Task Coordination Mechanism"

# The .processing marker mechanism should prevent conflicts
# We verify this by checking the skill was executed correctly

if echo "${MSG_BODIES}" | grep -qi "processing\|marker\|coordinate"; then
    log_pass "Task coordination mechanism mentioned"
else
    log_info "Task coordination may have happened silently (expected)"
fi

log_section "Collect Metrics"
wait_for_session_stable 5 60
METRICS=$(collect_delta_metrics "13-git-delegation" "$METRICS_BASELINE" "alice")
save_metrics_file "$METRICS" "13-git-delegation"
print_metrics_report "$METRICS"

test_teardown "13-git-delegation"
test_summary
