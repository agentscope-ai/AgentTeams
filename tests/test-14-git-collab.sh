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

TEST_WORKER_RUNTIME="${AGENTTEAMS_DEFAULT_WORKER_RUNTIME:-openclaw}"

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

docker exec "${TEST_CONTROLLER_CONTAINER}" bash -c "
    set -e
    mkdir -p '${REPO_PATH}.git'
    git init --bare '${REPO_PATH}.git'
    tmpdir=\$(mktemp -d)
    git -C \"\$tmpdir\" init
    git -C \"\$tmpdir\" remote add origin '${REPO_PATH}.git'
    echo '# Collab Test Project' > \"\$tmpdir/README.md\"
    git -C \"\$tmpdir\" add .
    git -C \"\$tmpdir\" -c user.email='setup@agentteams.io' -c user.name='Setup' -c core.hooksPath=/dev/null commit -m 'Initial commit'
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
    docker exec "${TEST_CONTROLLER_CONTAINER}" rm -rf "${REPO_PATH}.git" 2>/dev/null || true
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
1. Ensure workers with usernames exactly 'alice', 'bob', and 'charlie' exist with the git-delegation skill. The username (container name) must match exactly — do not use variations like 'alice-dev' or 'bob-backend'. IMPORTANT: Create any missing workers IN PARALLEL (run all create-worker.sh calls concurrently) to save time — do NOT create them one by one sequentially. When creating any missing worker, use these exact values — do NOT ask me to confirm any of them:
   - runtime: install default
   - skills: github-operations, git-delegation
   - SOUL/role: 'Developer working on a shared git repo using git-delegation workflows'
   If a worker already exists, reuse it.
2. Create a shared project room that includes alice, bob, charlie, and the human admin (use the create-project.sh script). All phase assignments and reports MUST happen in this project room — never in individual worker rooms.

Matrix mention isolation is mandatory in the project room:
- You may post the overall project plan without mentioning any Worker.
- Send each executable phase assignment as a separate message that mentions exactly one Worker: the Worker assigned to that phase.
- Never mention multiple Workers in one assignment message, never prefix an assignment with a participant roll-call such as 'alice bob charlie', and never include future-phase Worker names in the current assignment message.
- Creating the room with all participants does not assign work. After room creation, the first actionable message must mention only alice and contain only Phase 1 instructions.

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
    MANAGER_TOKEN=$(docker exec "${TEST_AGENT_CONTAINER}" \
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

case "${TEST_WORKER_RUNTIME}" in
    openclaw) EXPECTED_WORKER_IMAGE="agentteams/worker-agent:" ;;
    copaw) EXPECTED_WORKER_IMAGE="agentteams/copaw-worker:" ;;
    hermes) EXPECTED_WORKER_IMAGE="agentteams/hermes-worker:" ;;
    qwenpaw) EXPECTED_WORKER_IMAGE="agentteams/qwenpaw-worker:" ;;
    *) EXPECTED_WORKER_IMAGE="" ;;
esac

for WORKER_NAME in alice bob charlie; do
    WORKER_JSON=$(exec_in_agent agt get workers "${WORKER_NAME}" -o json 2>/dev/null || echo "{}")
    WORKER_RUNTIME=$(echo "${WORKER_JSON}" | jq -r '.runtime // empty')
    assert_eq "${TEST_WORKER_RUNTIME}" "${WORKER_RUNTIME}" \
        "Worker ${WORKER_NAME} runtime matches test matrix (got: '${WORKER_RUNTIME}', want: '${TEST_WORKER_RUNTIME}')"

    if [ -n "${EXPECTED_WORKER_IMAGE}" ]; then
        WORKER_IMAGE=$(docker inspect --format '{{.Config.Image}}' "agentteams-worker-${WORKER_NAME}" 2>/dev/null || true)
        assert_contains "${WORKER_IMAGE}" "${EXPECTED_WORKER_IMAGE}" \
            "Worker ${WORKER_NAME} image matches runtime ${TEST_WORKER_RUNTIME} (got: '${WORKER_IMAGE}')"
    fi
done

log_info "Waiting for Phase 4 verification branch (timeout: 1800s)..."
PHASE4_REF=""
DEADLINE=$(( $(date +%s) + 1800 ))
NEXT_NUDGE=$(( $(date +%s) + 120 ))
while [ "$(date +%s)" -lt "${DEADLINE}" ]; do
    PHASE4_REF=$(docker exec "${TEST_CONTROLLER_CONTAINER}" \
        git --git-dir="${REPO_PATH}.git" rev-parse --verify "refs/heads/${TEST_BRANCH}" 2>/dev/null || true)
    [ -n "${PHASE4_REF}" ] && break

    if [ "$(date +%s)" -ge "${NEXT_NUDGE}" ]; then
        matrix_send_message "${ADMIN_TOKEN}" "${DM_ROOM}" \
            "Please continue the 4-phase workflow from the latest worker report in the project room. Do not stop until Phase 4 is complete." \
            2>/dev/null || true
        NEXT_NUDGE=$(( $(date +%s) + 120 ))
    fi
    sleep 10
done
assert_not_empty "${PHASE4_REF}" "Phase 4 verification branch exists"

log_section "Verify Four-Phase Git Results"

FEATURE_CONTENT=$(docker exec "${TEST_CONTROLLER_CONTAINER}" \
    git --git-dir="${REPO_PATH}.git" show "refs/heads/${FEATURE_BRANCH}:doc/proposal.md" 2>/dev/null || true)
REVIEW_CONTENT=$(docker exec "${TEST_CONTROLLER_CONTAINER}" \
    git --git-dir="${REPO_PATH}.git" show "refs/heads/${REVIEW_BRANCH}:reviews/proposal-review.md" 2>/dev/null || true)
VERIFY_CONTENT=$(docker exec "${TEST_CONTROLLER_CONTAINER}" \
    git --git-dir="${REPO_PATH}.git" show "refs/heads/${TEST_BRANCH}:verify/checklist.md" 2>/dev/null || true)

assert_contains "${FEATURE_CONTENT}" "## Summary" "Phase 3 added Summary to proposal"
assert_contains "${FEATURE_CONTENT}" "## Goals" "Feature branch retains proposal Goals"
assert_contains "${REVIEW_CONTENT}" "Please add a ## Summary section" "Phase 2 review requested Summary"
assert_contains "${VERIFY_CONTENT}" "Summary" "Phase 4 checklist verifies Summary"
assert_contains "${VERIFY_CONTENT}" "Goals" "Phase 4 checklist verifies Goals"
assert_contains_i "${VERIFY_CONTENT}" "review" "Phase 4 checklist verifies review was addressed"

FEATURE_COMMITS=$(docker exec "${TEST_CONTROLLER_CONTAINER}" \
    git --git-dir="${REPO_PATH}.git" log --format=%s "refs/heads/${FEATURE_BRANCH}" 2>/dev/null || true)
REVIEW_COMMIT=$(docker exec "${TEST_CONTROLLER_CONTAINER}" \
    git --git-dir="${REPO_PATH}.git" log -1 --format=%s "refs/heads/${REVIEW_BRANCH}" 2>/dev/null || true)
VERIFY_COMMIT=$(docker exec "${TEST_CONTROLLER_CONTAINER}" \
    git --git-dir="${REPO_PATH}.git" log -1 --format=%s "refs/heads/${TEST_BRANCH}" 2>/dev/null || true)
assert_contains "${FEATURE_COMMITS}" "feat: add proposal" "Phase 1 commit message is exact"
assert_contains "${FEATURE_COMMITS}" "fix: add summary section per review" "Phase 3 commit message is exact"
assert_eq "review: request summary section" "${REVIEW_COMMIT}" "Phase 2 commit message is exact"
assert_eq "verify: proposal review checklist" "${VERIFY_COMMIT}" "Phase 4 commit message is exact"

EVENT_DEADLINE=$(( $(date +%s) + 300 ))
while true; do
    PROJECT_EVENTS=$(matrix_read_messages "${MANAGER_TOKEN}" "${PROJECT_ROOM}" 200 2>/dev/null || echo '{"chunk":[]}')
    PHASE1_TS=$(echo "${PROJECT_EVENTS}" | jq -r --arg u "@alice:${TEST_MATRIX_DOMAIN}" \
        '[.chunk[] | select(.sender == $u and (.content.body // "" | contains("PHASE1_DONE"))) | .origin_server_ts] | min // 0')
    PHASE2_TS=$(echo "${PROJECT_EVENTS}" | jq -r --arg u "@bob:${TEST_MATRIX_DOMAIN}" \
        '[.chunk[] | select(.sender == $u and (.content.body // "" | contains("REVISION_NEEDED"))) | .origin_server_ts] | min // 0')
    PHASE3_TS=$(echo "${PROJECT_EVENTS}" | jq -r --arg u "@alice:${TEST_MATRIX_DOMAIN}" \
        '[.chunk[] | select(.sender == $u and (.content.body // "" | contains("PHASE3_DONE"))) | .origin_server_ts] | min // 0')
    PHASE4_TS=$(echo "${PROJECT_EVENTS}" | jq -r --arg u "@charlie:${TEST_MATRIX_DOMAIN}" \
        '[.chunk[] | select(.sender == $u and (.content.body // "" | contains("PHASE4_DONE"))) | .origin_server_ts] | min // 0')
    [ "${PHASE1_TS}" -gt 0 ] && [ "${PHASE2_TS}" -gt 0 ] \
        && [ "${PHASE3_TS}" -gt 0 ] && [ "${PHASE4_TS}" -gt 0 ] && break
    [ "$(date +%s)" -ge "${EVENT_DEADLINE}" ] && break
    sleep 10
done

if [ "${PHASE1_TS}" -gt 0 ] && [ "${PHASE1_TS}" -lt "${PHASE2_TS}" ] \
    && [ "${PHASE2_TS}" -lt "${PHASE3_TS}" ] && [ "${PHASE3_TS}" -lt "${PHASE4_TS}" ]; then
    log_pass "Worker phase reports occurred in required alice → bob → alice → charlie order"
else
    log_fail "Missing or out-of-order phase reports (phase1=${PHASE1_TS}, phase2=${PHASE2_TS}, phase3=${PHASE3_TS}, phase4=${PHASE4_TS})"
fi

log_info "Waiting for Manager's final project-room summary..."
COMPLETION_PATTERN="workflow.*(complete|completed|done|finished)|all.*4.*phase.*(complete|completed|done|finished)|4.phase.*(complete|completed|done|finished)|工作流.*完成|四.*阶段.*完成"
COMPLETION_MSG=$(echo "${PROJECT_EVENTS}" | \
    jq -r --arg u "@manager" '[.chunk[] | select(.sender | startswith($u)) | .content.body] | .[]' 2>/dev/null | \
    grep -iE "${COMPLETION_PATTERN}" | head -1 || true)
if [ -z "${COMPLETION_MSG}" ]; then
    SUMMARY_DEADLINE=$(( $(date +%s) + 300 ))
    NEXT_SUMMARY_NUDGE=$(date +%s)
    while [ "$(date +%s)" -lt "${SUMMARY_DEADLINE}" ]; do
        if [ "$(date +%s)" -ge "${NEXT_SUMMARY_NUDGE}" ]; then
            matrix_send_message "${ADMIN_TOKEN}" "${DM_ROOM}" \
                "Phase 4 is present in git. Please post the final workflow summary in the project room and @mention the human admin." \
                2>/dev/null || true
            NEXT_SUMMARY_NUDGE=$(( $(date +%s) + 60 ))
        fi
        sleep 15
        PROJECT_EVENTS=$(matrix_read_messages "${MANAGER_TOKEN}" "${PROJECT_ROOM}" 200 2>/dev/null || echo '{"chunk":[]}')
        COMPLETION_MSG=$(echo "${PROJECT_EVENTS}" | \
            jq -r --arg u "@manager" '[.chunk[] | select(.sender | startswith($u)) | .content.body] | .[]' 2>/dev/null | \
            grep -iE "${COMPLETION_PATTERN}" | head -1 || true)
        [ -n "${COMPLETION_MSG}" ] && break
    done
fi

assert_not_empty "${COMPLETION_MSG}" "Manager posted final completion summary in project room"
assert_contains "${COMPLETION_MSG}" "@${TEST_ADMIN_USER}:${TEST_MATRIX_DOMAIN}" \
    "Manager final summary @mentions the human admin"
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

docker exec "${TEST_CONTROLLER_CONTAINER}" rm -rf "${REPO_PATH}.git" 2>/dev/null || true
log_info "Removed bare git repo"

test_teardown "14-git-collab"
test_summary
