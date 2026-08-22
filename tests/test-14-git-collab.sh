#!/bin/bash
# test-14-git-collab.sh - Case 14: Non-linear multi-Worker local git collaboration
# Verifies: 4-phase PR-style collaboration using local bare git repo (no GitHub required):
#   Phase 1 (alice): implement feature on a branch
#   Phase 2 (bob): review and request changes via a review branch
#   Phase 3 (alice): fix based on review, update branch
#   Phase 4 (charlie): add tests on a test branch

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/test-helpers.sh"
source "${SCRIPT_DIR}/lib/matrix-client.sh"
source "${SCRIPT_DIR}/lib/agent-metrics.sh"

test_setup "14-git-collab"

if ! require_llm_key; then
    test_teardown "14-git-collab"
    test_summary
    exit 0
fi

ADMIN_LOGIN=$(matrix_login "${TEST_ADMIN_USER}" "${TEST_ADMIN_PASSWORD}")
ADMIN_TOKEN=$(echo "${ADMIN_LOGIN}" | jq -r '.access_token')

MANAGER_USER="@manager:${TEST_MATRIX_DOMAIN}"

# Generate unique branch names for this test run
TEST_RUN_ID=$(date +%s)
REPO_PATH="/root/git-repos/collab-test-${TEST_RUN_ID}"
FEATURE_BRANCH="feature/proposal-${TEST_RUN_ID}"
REVIEW_BRANCH="review/proposal-${TEST_RUN_ID}"
TEST_BRANCH="verify/proposal-${TEST_RUN_ID}"

log_section "Setup: Initialize Bare Git Repo"

docker exec "${TEST_MANAGER_CONTAINER}" bash -c "
    set -e
    mkdir -p '${REPO_PATH}.git'
    git init --bare '${REPO_PATH}.git'
    tmpdir=\$(mktemp -d)
    git -C \"\$tmpdir\" init
    git -C \"\$tmpdir\" remote add origin '${REPO_PATH}.git'
    echo '# Collab Test Project' > \"\$tmpdir/README.md\"
    git -C \"\$tmpdir\" add .
    git -C \"\$tmpdir\" -c user.email='setup@hiclaw.io' -c user.name='Setup' -c core.hooksPath=/dev/null commit -m 'Initial commit'
    git -C \"\$tmpdir\" push origin HEAD:main
    rm -rf \"\$tmpdir\"
" || {
    log_fail "Failed to initialize bare git repo"
    test_teardown "14-git-collab"
    test_summary
    exit 1
}
log_pass "Bare git repo initialized at ${REPO_PATH}.git"

# All git operations are delegated to the Manager, which runs them locally
# inside the manager container — no network protocol needed, use local path directly.
GIT_REPO_URL="${REPO_PATH}.git"
log_info "Git repo local path (used by Manager for all operations): ${GIT_REPO_URL}"

log_section "Setup: Find or Create DM Room"

DM_ROOM=$(matrix_find_dm_room "${ADMIN_TOKEN}" "${MANAGER_USER}" 2>/dev/null || true)

if [ -z "${DM_ROOM}" ]; then
    log_info "Creating DM room with Manager..."
    DM_ROOM=$(matrix_create_dm_room "${ADMIN_TOKEN}" "${MANAGER_USER}")
    sleep 5
fi

assert_not_empty "${DM_ROOM}" "DM room with Manager exists"

wait_for_manager_agent_ready 300 "${DM_ROOM}" "${ADMIN_TOKEN}" || {
    log_fail "Manager Agent not ready in time"
    docker exec "${TEST_MANAGER_CONTAINER}" rm -rf "${REPO_PATH}.git" 2>/dev/null || true
    test_teardown "14-git-collab"
    test_summary
    exit 1
}

log_section "Phase 1-4: Assign 4-Phase Git Collaboration Task"

TASK_DESCRIPTION="Please coordinate a 4-phase git collaboration workflow to test non-linear multi-worker coordination.

Git repo URL (reachable from all worker containers): ${GIT_REPO_URL}
The repo has a 'main' branch with an initial commit.

⚠️ CRITICAL WORKER ASSIGNMENT TABLE — MUST FOLLOW EXACTLY, NO EXCEPTIONS:

| Phase | Assigned Worker | Trigger condition                     |
|-------|-----------------|---------------------------------------|
| 1     | alice           | start immediately                     |
| 2     | bob             | ONLY after alice reports PHASE1_DONE  |
| 3     | alice           | ONLY after bob reports REVISION_NEEDED|
| 4     | charlie         | ONLY after alice reports PHASE3_DONE  |

DO NOT assign any phase to a different worker. DO NOT give alice phase 2 or phase 4. DO NOT give bob phase 1 or phase 3. DO NOT give charlie any phase except phase 4. Each phase must be done by the worker listed above and no one else.

IMPORTANT: You MUST use the EXACT branch names and file paths specified below. Do not rename, substitute, or simplify them. The verification system checks these exact names.

Before starting any phase:
1. Ensure workers with usernames exactly 'alice', 'bob', and 'charlie' exist with the git-delegation skill. The username (container name) must match exactly — do not use variations like 'alice-dev' or 'bob-backend'. IMPORTANT: Create any missing workers IN PARALLEL (run all create-worker.sh calls concurrently) to save time — do NOT create them one by one sequentially.
2. Create a shared project room that includes alice, bob, charlie, and the human admin (use the create-project.sh script). All phase assignments and reports MUST happen in this project room — never in individual worker rooms.

Run the phases strictly in order, waiting for each phase's report before starting the next.

**Phase 1 — alice (and only alice)**:
- Clone ${GIT_REPO_URL}
- Create branch named EXACTLY '${FEATURE_BRANCH}' from main (do not use any other name)
- Create file at path EXACTLY 'doc/proposal.md' with this content:
  # Project Proposal

  ## Background
  This project aims to improve team collaboration.

  ## Goals
  - Faster delivery
  - Better quality
- Commit with message 'feat: add proposal' and push branch '${FEATURE_BRANCH}' to ${GIT_REPO_URL}
- Report PHASE1_DONE

**Phase 2 — bob and only bob** (assign to bob, NOT alice, only after alice reports PHASE1_DONE):
- Clone ${GIT_REPO_URL}, check out branch '${FEATURE_BRANCH}', read doc/proposal.md
- Create branch named EXACTLY '${REVIEW_BRANCH}' from '${FEATURE_BRANCH}' (do not use any other name)
- Create file at path EXACTLY 'reviews/proposal-review.md' with this content:
  # Review

  The proposal looks good. Please add a ## Summary section at the top that briefly describes the project in one sentence.
- Commit 'review: request summary section' and push branch '${REVIEW_BRANCH}' to ${GIT_REPO_URL}
- Report REVISION_NEEDED

**Phase 3 — alice and only alice** (assign back to alice, NOT bob, only after bob reports REVISION_NEEDED):
- Work on branch '${FEATURE_BRANCH}' (not a new branch)
- Read bob's review file at path 'reviews/proposal-review.md' on branch '${REVIEW_BRANCH}'
- Edit 'doc/proposal.md' on branch '${FEATURE_BRANCH}': add a '## Summary' section immediately after the '# Project Proposal' title line, with one sentence describing the project
- Commit 'fix: add summary section per review' and push branch '${FEATURE_BRANCH}' to ${GIT_REPO_URL}
- Report PHASE3_DONE

**Phase 4 — charlie and only charlie** (assign to charlie, NOT alice or bob, only after alice reports PHASE3_DONE):
- Clone ${GIT_REPO_URL}, create branch named EXACTLY '${TEST_BRANCH}' from '${FEATURE_BRANCH}' (do not use any other name)
- Create file at path EXACTLY 'verify/checklist.md' confirming: (1) proposal.md has a Summary section, (2) Goals section is present, (3) review was addressed
- Commit 'verify: proposal review checklist' and push branch '${TEST_BRANCH}' to ${GIT_REPO_URL}
- Report PHASE4_DONE

When all 4 phases are done, post a final summary in the project room and @mention the human admin to notify them the workflow is complete."

# Snapshot before first LLM interaction
METRICS_BASELINE=$(snapshot_baseline "alice" "bob" "charlie")

matrix_send_message "${ADMIN_TOKEN}" "${DM_ROOM}" "${TASK_DESCRIPTION}"

log_info "Waiting for Manager to acknowledge and start coordination..."
REPLY=$(matrix_wait_for_reply "${ADMIN_TOKEN}" "${DM_ROOM}" "@manager" 300 \
    "${ADMIN_TOKEN}" "${DM_ROOM}" "Please check if the git collaboration task has been processed.")

if [ -n "${REPLY}" ]; then
    log_pass "Manager acknowledged the git collaboration task"
else
    log_info "No explicit acknowledgment (Manager may have started processing directly)"
fi

log_section "Wait for Workflow Completion (up to 30 minutes)"

# Get Manager's Matrix token (retry until openclaw.json is written)
log_info "Waiting for Manager token (timeout: 120s)..."
MANAGER_TOKEN=""
DEADLINE=$(( $(date +%s) + 120 ))
while [ "$(date +%s)" -lt "${DEADLINE}" ]; do
    MANAGER_TOKEN=$(docker exec "${TEST_MANAGER_CONTAINER}" \
        jq -r '.channels.matrix.accessToken // empty' /root/manager-workspace/openclaw.json 2>/dev/null || true)
    [ -n "${MANAGER_TOKEN}" ] && break
    sleep 5
done
assert_not_empty "${MANAGER_TOKEN}" "Manager Matrix token available"

log_info "Waiting for project room to be created (timeout: 900s)..."
PROJECT_ROOM=""
DEADLINE=$(( $(date +%s) + 900 ))
while [ "$(date +%s)" -lt "${DEADLINE}" ]; do
    PROJECT_ROOM=$(matrix_find_room_by_name "${MANAGER_TOKEN}" "Project:" 2>/dev/null || true)
    [ -n "${PROJECT_ROOM}" ] && break
    sleep 10
done
assert_not_empty "${PROJECT_ROOM}" "Project room created by Manager"
log_info "Project room: ${PROJECT_ROOM}"

log_info "Waiting for Manager to post completion message in project room (timeout: 1800s)..."
# First check if completion message was already posted
COMPLETION_MSG=$(matrix_read_messages "${MANAGER_TOKEN}" "${PROJECT_ROOM}" 50 2>/dev/null | \
    jq -r --arg u "@manager" '[.chunk[] | select(.sender | startswith($u)) | .content.body] | .[]' 2>/dev/null | \
    grep -iE "complete|done|finished|已完成|完成|all.*phase|phase.*4|PHASE4" | head -1 || true)

if [ -z "${COMPLETION_MSG}" ]; then
    COMPLETION_MSG=$(matrix_wait_for_message_containing "${MANAGER_TOKEN}" "${PROJECT_ROOM}" "@manager" \
        "complete\|done\|finished\|已完成\|完成\|all.*phase\|phase.*4\|PHASE4" 1800 \
        "${ADMIN_TOKEN}" "${DM_ROOM}" \
        "Please check the project room and continue coordinating the git collaboration workflow. If any phase is pending or a worker message was missed, please follow up." \
        2>/dev/null || true)
fi

assert_not_empty "${COMPLETION_MSG}" "Manager posted completion message in project room"
log_pass "Workflow complete — Manager's message: $(echo "${COMPLETION_MSG}" | head -c 200)"

log_section "Collect Metrics"

wait_for_worker_session_stable "alice" 5 120
wait_for_worker_session_stable "bob" 5 120
wait_for_worker_session_stable "charlie" 5 120
wait_for_session_stable 5 60
PREV_METRICS=$(cat "${TEST_OUTPUT_DIR}/metrics-14-git-collab.json" 2>/dev/null || true)
METRICS=$(collect_delta_metrics "14-git-collab" "$METRICS_BASELINE" "alice" "bob" "charlie")
print_metrics_report "$METRICS" "$PREV_METRICS"
save_metrics_file "$METRICS" "14-git-collab"

log_section "Cleanup"

docker exec "${TEST_MANAGER_CONTAINER}" rm -rf "${REPO_PATH}.git" 2>/dev/null || true
log_info "Removed bare git repo"

test_teardown "14-git-collab"
test_summary
