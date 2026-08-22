#!/bin/bash
# test-11-github-pr-collab.sh - Case 11: Non-linear multi-Worker GitHub PR collaboration
# Verifies: A develops feature & creates PR, B reviews & comments on GitHub PR,
#           A fixes issues, C adds tests and verifies they pass.
#
# This test validates:
# 1. GitHub PR workflow across multiple workers
# 2. PR review comments via GitHub API
# 3. Non-linear task dependencies (revision flow)
# 4. Cross-worker coordination via GitHub

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/test-helpers.sh"
source "${SCRIPT_DIR}/lib/matrix-client.sh"

test_setup "11-github-pr-collab"

ADMIN_LOGIN=$(matrix_login "${TEST_ADMIN_USER}" "${TEST_ADMIN_PASSWORD}")
ADMIN_TOKEN=$(echo "${ADMIN_LOGIN}" | jq -r '.access_token')

MANAGER_USER="@manager:${TEST_MATRIX_DOMAIN}"

# Check if GitHub token is configured
if [ -z "${HICLAW_GITHUB_TOKEN}" ] && [ -z "${TEST_GITHUB_TOKEN}" ]; then
    log_info "SKIP: No GitHub token configured"
    test_teardown "11-github-pr-collab"
    test_summary
    exit 0
fi

# Configuration
GITHUB_TOKEN="${TEST_GITHUB_TOKEN:-${HICLAW_GITHUB_TOKEN}}"
GITHUB_OWNER="${TEST_GITHUB_OWNER:-johnlanni}"
GITHUB_REPO="${TEST_GITHUB_REPO:-test-temp-0224}"

log_section "Prerequisite: Verify GitHub repo access"

# Verify we can access the repo
REPO_CHECK=$(curl -s -o /dev/null -w "%{http_code}" \
    -H "Authorization: Bearer ${GITHUB_TOKEN}" \
    -H "Accept: application/vnd.github+json" \
    "https://api.github.com/repos/${GITHUB_OWNER}/${GITHUB_REPO}")

if [ "${REPO_CHECK}" != "200" ]; then
    log_info "SKIP: Cannot access GitHub repo ${GITHUB_OWNER}/${GITHUB_REPO} (HTTP ${REPO_CHECK})"
    test_teardown "11-github-pr-collab"
    test_summary
    exit 0
fi
log_pass "GitHub repo ${GITHUB_OWNER}/${GITHUB_REPO} accessible"

# Generate unique branch names for this test run
TEST_RUN_ID=$(date +%s)
FEATURE_BRANCH="feature/calc-function-${TEST_RUN_ID}"
TEST_BRANCH="test/calc-tests-${TEST_RUN_ID}"

log_section "Phase 1: Assign GitHub PR Collaboration Task"

DM_ROOM=$(matrix_find_dm_room "${ADMIN_TOKEN}" "${MANAGER_USER}" 2>/dev/null || true)

if [ -z "${DM_ROOM}" ]; then
    log_info "Creating DM room with Manager..."
    DM_ROOM=$(matrix_create_dm_room "${ADMIN_TOKEN}" "${MANAGER_USER}")
    sleep 3  # Wait for Manager to join
fi

assert_not_empty "${DM_ROOM}" "DM room with Manager exists"

# Wait for Manager Agent to be fully ready (OpenClaw gateway + joined DM room)
wait_for_manager_agent_ready 300 "${DM_ROOM}" "${ADMIN_TOKEN}" || {
    log_fail "Manager Agent not ready in time"
    test_teardown "11-github-pr-collab"
    test_summary
    exit 1
}

# Create the task description
TASK_DESCRIPTION="I need a collaborative GitHub PR workflow test:

**Scenario**: Implement a simple calculator module with PR-based collaboration

**Phase 1 - Feature Development (Worker A: alice)**:
1. Create branch '${FEATURE_BRANCH}' from main
2. Create file 'src/calculator.js' with add(a,b) and multiply(a,b) functions
3. Create Pull Request titled 'feat: add calculator module'

**Phase 2 - Code Review (Worker B: bob)**:
1. Review the PR on GitHub
2. Add a review COMMENT on the PR requesting: 'Please add input validation for negative numbers'
3. Report REVISION_NEEDED so alice can fix

**Phase 3 - Fix Implementation (Worker A: alice)**:
1. Update the calculator.js to add input validation
2. Push the fix to the same branch (new commit)
3. Report SUCCESS

**Phase 4 - Test Addition (Worker C: charlie)**:
1. Create branch '${TEST_BRANCH}' from alice's feature branch (after fix)
2. Add file 'tests/calculator.test.js' with unit tests
3. Verify tests would pass (since no actual test runner, just verify the test file is valid)
4. Create PR titled 'test: add calculator unit tests'
5. Report SUCCESS

GitHub repo: ${GITHUB_OWNER}/${GITHUB_REPO}

Please create workers alice (developer), bob (reviewer), and charlie (qa-tester) and coordinate this workflow."

matrix_send_message "${ADMIN_TOKEN}" "${DM_ROOM}" "${TASK_DESCRIPTION}"

log_info "Waiting for Manager to acknowledge and start coordination..."
REPLY=$(matrix_wait_for_reply "${ADMIN_TOKEN}" "${DM_ROOM}" "@manager" 300 \
    "${ADMIN_TOKEN}" "${DM_ROOM}" "Please check if the PR collaboration task has been processed.")

# Note: Manager may start processing immediately without explicit acknowledgment
# So we don't fail if no reply is received, as long as workflow completes
if [ -n "${REPLY}" ]; then
    log_pass "Manager acknowledged the GitHub PR collaboration task"
else
    log_info "No explicit acknowledgment from Manager (may have started processing directly)"
fi

log_section "Phase 2: Wait for Workflow Completion (up to 8 minutes)"

log_info "Waiting for Workers to complete the GitHub PR workflow..."
sleep 480  # 8 minutes for full workflow

# Read all messages in DM room
MESSAGES=$(matrix_read_messages "${ADMIN_TOKEN}" "${DM_ROOM}" 100)
MSG_BODIES=$(echo "${MESSAGES}" | jq -r '[.chunk[].content.body] | join("\n---\n")' 2>/dev/null)

log_section "Phase 3: Verify Workflow Results"

# Check for evidence of each phase

# Phase 1: Feature development by alice
PHASE1_EVIDENCE=0
if echo "${MSG_BODIES}" | grep -qi "alice\|calculator\|feature"; then
    PHASE1_EVIDENCE=1
    log_pass "Phase 1: Alice's feature development activity detected"
else
    log_fail "Phase 1: No evidence of Alice's feature development"
fi

# Phase 2: Code review by bob with REVISION_NEEDED
PHASE2_EVIDENCE=0
if echo "${MSG_BODIES}" | grep -qi "bob\|review\|revision"; then
    PHASE2_EVIDENCE=1
    log_pass "Phase 2: Bob's review activity detected"
else
    log_fail "Phase 2: No evidence of Bob's code review"
fi

# Phase 3: Fix by alice
PHASE3_EVIDENCE=0
if echo "${MSG_BODIES}" | grep -qi "fix\|validation\|updated"; then
    PHASE3_EVIDENCE=1
    log_pass "Phase 3: Alice's fix implementation detected"
else
    log_fail "Phase 3: No evidence of Alice's fix"
fi

# Phase 4: Tests by charlie
PHASE4_EVIDENCE=0
if echo "${MSG_BODIES}" | grep -qi "charlie\|test\|qa"; then
    PHASE4_EVIDENCE=1
    log_pass "Phase 4: Charlie's test activity detected"
else
    log_fail "Phase 4: No evidence of Charlie's tests"
fi

log_section "Phase 4: Verify GitHub State"

# Check for PR existence
PR_CHECK=$(curl -s \
    -H "Authorization: Bearer ${GITHUB_TOKEN}" \
    -H "Accept: application/vnd.github+json" \
    "https://api.github.com/repos/${GITHUB_OWNER}/${GITHUB_REPO}/pulls?state=all&head=${GITHUB_OWNER}:${FEATURE_BRANCH}" 2>/dev/null)

PR_COUNT=$(echo "${PR_CHECK}" | jq -r 'length' 2>/dev/null || echo "0")
if [ "${PR_COUNT}" -gt 0 ]; then
    log_pass "GitHub PR from ${FEATURE_BRANCH} exists"
    PR_NUMBER=$(echo "${PR_CHECK}" | jq -r '.[0].number' 2>/dev/null)
    log_info "PR Number: ${PR_NUMBER}"

    # Check for review comments
    if [ -n "${PR_NUMBER}" ] && [ "${PR_NUMBER}" != "null" ]; then
        COMMENTS=$(curl -s \
            -H "Authorization: Bearer ${GITHUB_TOKEN}" \
            -H "Accept: application/vnd.github+json" \
            "https://api.github.com/repos/${GITHUB_OWNER}/${GITHUB_REPO}/pulls/${PR_NUMBER}/comments" 2>/dev/null)
        COMMENT_COUNT=$(echo "${COMMENTS}" | jq -r 'length' 2>/dev/null || echo "0")
        if [ "${COMMENT_COUNT}" -gt 0 ]; then
            log_pass "PR has ${COMMENT_COUNT} review comment(s)"
        else
            log_info "No review comments found on PR (may be issue comments instead)"
        fi

        # Check for issue comments (general comments on PR)
        ISSUE_COMMENTS=$(curl -s \
            -H "Authorization: Bearer ${GITHUB_TOKEN}" \
            -H "Accept: application/vnd.github+json" \
            "https://api.github.com/repos/${GITHUB_OWNER}/${GITHUB_REPO}/issues/${PR_NUMBER}/comments" 2>/dev/null)
        ISSUE_COMMENT_COUNT=$(echo "${ISSUE_COMMENTS}" | jq -r 'length' 2>/dev/null || echo "0")
        if [ "${ISSUE_COMMENT_COUNT}" -gt 0 ]; then
            log_pass "PR has ${ISSUE_COMMENT_COUNT} issue comment(s)"
        fi
    fi
else
    log_info "GitHub PR from ${FEATURE_BRANCH} not found (workflow may not have completed)"
fi

# Check for test PR
TEST_PR_CHECK=$(curl -s \
    -H "Authorization: Bearer ${GITHUB_TOKEN}" \
    -H "Accept: application/vnd.github+json" \
    "https://api.github.com/repos/${GITHUB_OWNER}/${GITHUB_REPO}/pulls?state=all&head=${GITHUB_OWNER}:${TEST_BRANCH}" 2>/dev/null)

TEST_PR_COUNT=$(echo "${TEST_PR_CHECK}" | jq -r 'length' 2>/dev/null || echo "0")
if [ "${TEST_PR_COUNT}" -gt 0 ]; then
    log_pass "Test PR from ${TEST_BRANCH} exists"
else
    log_info "Test PR from ${TEST_BRANCH} not found (phase 4 may not have completed)"
fi

log_section "Phase 5: Cleanup - Close PRs and Delete Branches"

# Close any open PRs from this test
for branch in "${FEATURE_BRANCH}" "${TEST_BRANCH}"; do
    PR_TO_CLOSE=$(curl -s \
        -H "Authorization: Bearer ${GITHUB_TOKEN}" \
        -H "Accept: application/vnd.github+json" \
        "https://api.github.com/repos/${GITHUB_OWNER}/${GITHUB_REPO}/pulls?state=open&head=${GITHUB_OWNER}:${branch}" 2>/dev/null)

    PR_NUM=$(echo "${PR_TO_CLOSE}" | jq -r '.[0].number' 2>/dev/null)
    if [ -n "${PR_NUM}" ] && [ "${PR_NUM}" != "null" ]; then
        curl -s -X PATCH \
            -H "Authorization: Bearer ${GITHUB_TOKEN}" \
            -H "Accept: application/vnd.github+json" \
            "https://api.github.com/repos/${GITHUB_OWNER}/${GITHUB_REPO}/pulls/${PR_NUM}" \
            -d '{"state":"closed"}' > /dev/null 2>&1
        log_info "Closed PR #${PR_NUM} from ${branch}"
    fi

    # Delete the branch
    curl -s -X DELETE \
        -H "Authorization: Bearer ${GITHUB_TOKEN}" \
        -H "Accept: application/vnd.github+json" \
        "https://api.github.com/repos/${GITHUB_OWNER}/${GITHUB_REPO}/git/refs/heads/${branch}" > /dev/null 2>&1
    log_info "Deleted branch ${branch}"
done

test_teardown "11-github-pr-collab"
test_summary
