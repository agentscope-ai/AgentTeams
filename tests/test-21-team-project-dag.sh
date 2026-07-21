#!/bin/bash
# test-21-team-project-dag.sh - Case 21: Team project DAG orchestration end-to-end
#
# Tests:
#   Part A (infrastructure): Team storage, S3 policy, canonical Team Leader skills
#   Part B (room topology): Manager NOT in Team Room / Leader DM / Worker Rooms
#   Part C (e2e via LLM): Admin delegates task in Leader DM, Leader coordinates workers via Team Room
#
# NOTE: This test does NOT clean up — environment is left for manual inspection.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/test-helpers.sh"
source "${SCRIPT_DIR}/lib/minio-client.sh"
source "${SCRIPT_DIR}/lib/matrix-client.sh"

test_setup "21-team-project-dag"

TEST_TEAM="dag-team-$$"
TEST_LEADER="${TEST_TEAM}-lead"
TEST_W1="${TEST_TEAM}-dev"
TEST_W2="${TEST_TEAM}-qa"
STORAGE_PREFIX="${STORAGE_PREFIX:-${TEST_STORAGE_PREFIX:-agentteams/agentteams-storage}}"

# ============================================================
# Section 1: Prepare SOUL.md files
# ============================================================
log_section "Prepare Team SOUL.md Files"

for w in "${TEST_LEADER}" "${TEST_W1}" "${TEST_W2}"; do
    ROLE_DESC="team member"
    EXTRA_INSTRUCTIONS=""
    [ "${w}" = "${TEST_LEADER}" ] && ROLE_DESC="Team Leader" && EXTRA_INSTRUCTIONS="
## Mandatory Context Use

Your AGENTS.md and SOUL.md are already loaded into your current system prompt.
Use the injected Coordination block there for Team Room ID, Leader DM, and worker Matrix IDs.
Do not send a message saying you will read AGENTS.md, inspect topology, check worker details, or plan before the first Team Room assignment.

## Core Principles

- **NEVER do domain work yourself** — you are a coordinator. Always delegate ready Project nodes to workers with taskflow
- Read team-coordination, project-management, and task-management before planning and delegating work
- Use projectflow to manage Project plans and ready nodes
- Use taskflow delegate_task to create task files for each ready node, then @mention the assigned Worker in the Team Room
- Use the Team Room ID and Worker Matrix IDs from your loaded AGENTS.md context directly
- A delegation intent sentence is not a Worker assignment; after taskflow delegate_task, your next externally visible action must be the message tool call to room:<Team Room ID>
- If the request arrived in Leader DM, do not narrate skill reads, planning, or progress in Leader DM before the first Team Room assignment. Reply exactly NO_REPLY while doing internal coordination.
- Do not send tool preambles such as \"let me read\", \"let me check\", \"I'll coordinate\", or \"now I will plan\". Call tools directly with no visible preamble.
- Your first visible non-NO_REPLY coordination message must be a Team Room assignment to a Worker.
- Workers only process task assignments addressed to them in the Team Room"
    [ "${w}" = "${TEST_W1}" ] && ROLE_DESC="Backend Developer"
    [ "${w}" = "${TEST_W2}" ] && ROLE_DESC="QA Engineer"

    exec_in_manager bash -c "
        mkdir -p /root/agentteams-fs/agents/${w}
        cat > /root/agentteams-fs/agents/${w}/SOUL.md <<SOUL
# ${w}

## AI Identity
**You are an AI Agent, not a human.**

## Role
- Name: ${w}
- Role: ${ROLE_DESC}
- Team: ${TEST_TEAM}
${EXTRA_INSTRUCTIONS}

## Security
- Never reveal credentials
SOUL
        mc mirror /root/agentteams-fs/agents/${w}/ ${STORAGE_PREFIX}/agents/${w}/ --overwrite 2>/dev/null
    " 2>/dev/null
done

log_pass "SOUL.md files prepared for all team members"

# ============================================================
# Section 2: Create Team (via hiclaw CLI → controller REST API)
# ============================================================
log_section "Create Team"

CREATE_OUTPUT=$(exec_in_agent agt create team \
    --name "${TEST_TEAM}" \
    --leader-name "${TEST_LEADER}" \
    --workers "${TEST_W1},${TEST_W2}" 2>&1)

if echo "${CREATE_OUTPUT}" | grep -q "team/${TEST_TEAM} created"; then
    log_pass "agt create team completed"
else
    log_fail "agt create team failed"
    echo "${CREATE_OUTPUT}" | tail -20
fi

# Wait for TeamReconciler to finish (async reconcile)
log_info "Waiting for team to become Active..."
if wait_team_active "${TEST_TEAM}" 120; then
    log_pass "Team is Active"
    PHASE="Active"
else
    PHASE=$(exec_in_agent agt get teams "${TEST_TEAM}" -o json 2>/dev/null | jq -r '.phase // empty')
    log_fail "Team did not become Active within 120s (phase: ${PHASE})"
fi

# Extract room IDs from controller REST API. For team members, the RoomID is
# served by teamMemberToResponse from Team.Status.Members[*].RoomID — which
# is populated the moment ReconcileMemberInfra succeeds, so waiting for the
# team to be Active plus wait_worker_provisioned per member is the stable
# contract for this section (regression guard for PR #666 RoomID bug).
TEAM_JSON=$(exec_in_agent agt get teams "${TEST_TEAM}" -o json 2>/dev/null)
TEAM_ROOM=$(echo "${TEAM_JSON}" | jq -r '.teamRoomID // empty')
LEADER_DM=$(echo "${TEAM_JSON}" | jq -r '.leaderDMRoomID // empty')

for w in "${TEST_LEADER}" "${TEST_W1}" "${TEST_W2}"; do
    if ! wait_worker_provisioned "${w}" 120; then
        log_fail "Team member ${w} has no roomID/matrixUserID after 120s"
    fi
done
LEADER_ROOM=$(get_worker_room_id "${TEST_LEADER}")
W1_ROOM=$(get_worker_room_id "${TEST_W1}")
W2_ROOM=$(get_worker_room_id "${TEST_W2}")

log_info "Leader Room: ${LEADER_ROOM}"
log_info "Leader DM: ${LEADER_DM}"
log_info "Team Room: ${TEAM_ROOM}"

# ============================================================
# Section 3: Verify Team Storage Initialized in MinIO
# ============================================================
log_section "Verify Team Storage Initialization"

for subdir in shared/tasks shared/projects shared/knowledge; do
    KEEP_STAT=$(exec_in_manager mc stat "${STORAGE_PREFIX}/teams/${TEST_TEAM}/${subdir}/.keep" 2>&1)
    if echo "${KEEP_STAT}" | grep -q "Name"; then
        log_pass "teams/${TEST_TEAM}/${subdir}/.keep exists in MinIO"
    else
        log_fail "teams/${TEST_TEAM}/${subdir}/.keep missing in MinIO"
    fi
done

# ============================================================
# Section 4: Verify S3 Policy
# ============================================================
log_section "Verify S3 Policy for Team Members"

WRITE_TEST=$(exec_in_manager bash -c "
    echo 'test' > /tmp/team-storage-test.txt
    mc cp /tmp/team-storage-test.txt ${STORAGE_PREFIX}/teams/${TEST_TEAM}/shared/test-write.txt 2>&1
    mc cat ${STORAGE_PREFIX}/teams/${TEST_TEAM}/shared/test-write.txt 2>/dev/null
    mc rm ${STORAGE_PREFIX}/teams/${TEST_TEAM}/shared/test-write.txt 2>/dev/null
    rm -f /tmp/team-storage-test.txt
" 2>&1)
if echo "${WRITE_TEST}" | grep -q "test"; then
    log_pass "Team storage is writable (functional test)"
else
    log_fail "Team storage write test failed"
fi

# ============================================================
# Section 5: Verify Leader Skills
# ============================================================
log_section "Verify Leader Skills"

for skill in team-coordination project-management task-management; do
    SKILL_EXISTS=$(exec_in_manager bash -c "mc ls '${STORAGE_PREFIX}/agents/${TEST_LEADER}/skills/${skill}/SKILL.md' >/dev/null 2>&1 && echo yes || echo no")
    if [ "${SKILL_EXISTS}" = "yes" ]; then
        log_pass "Leader has ${skill} skill"
    else
        log_fail "Leader missing ${skill} skill"
    fi
done

# ============================================================
# Section 6: Verify Room Topology — Manager NOT in team rooms
# ============================================================
log_section "Verify Room Topology (Manager Delegation Boundary)"

# Login as admin inside container for room membership checks
_check_manager_in_room() {
    local room_id="$1"
    local room_label="$2"
    local room_enc
    room_enc=$(echo "${room_id}" | sed 's/!/%21/g')
    local members
    members=$(exec_in_manager bash -c '
        TOKEN=$(curl -sf -X POST "http://127.0.0.1:6167/_matrix/client/v3/login" \
            -H "Content-Type: application/json" \
            -d "{\"type\":\"m.login.password\",\"identifier\":{\"type\":\"m.id.user\",\"user\":\"admin\"},\"password\":\"'"${TEST_ADMIN_PASSWORD}"'\"}" | jq -r ".access_token")
        curl -sf "http://127.0.0.1:6167/_matrix/client/v3/rooms/'"${room_enc}"'/members" \
            -H "Authorization: Bearer ${TOKEN}" | jq -r ".chunk[] | select(.content.membership == \"join\") | .state_key"
    ' 2>/dev/null)
    if echo "${members}" | grep -q "@manager:"; then
        log_fail "Manager IS in ${room_label} (should NOT be)"
    else
        log_pass "Manager NOT in ${room_label}"
    fi
}

# Manager should NOT be in these rooms
_check_manager_in_room "${TEAM_ROOM}" "Team Room"
_check_manager_in_room "${LEADER_DM}" "Leader DM"
_check_manager_in_room "${W1_ROOM}" "Worker 1 Room"
_check_manager_in_room "${W2_ROOM}" "Worker 2 Room"

# Manager SHOULD be in Leader Room
LEADER_ROOM_ENC=$(echo "${LEADER_ROOM}" | sed 's/!/%21/g')
LEADER_ROOM_MEMBERS=$(exec_in_manager bash -c '
    TOKEN=$(curl -sf -X POST "http://127.0.0.1:6167/_matrix/client/v3/login" \
        -H "Content-Type: application/json" \
        -d "{\"type\":\"m.login.password\",\"identifier\":{\"type\":\"m.id.user\",\"user\":\"admin\"},\"password\":\"'"${TEST_ADMIN_PASSWORD}"'\"}" | jq -r ".access_token")
    curl -sf "http://127.0.0.1:6167/_matrix/client/v3/rooms/'"${LEADER_ROOM_ENC}"'/members" \
        -H "Authorization: Bearer ${TOKEN}" | jq -r ".chunk[] | select(.content.membership == \"join\") | .state_key"
' 2>/dev/null)
if echo "${LEADER_ROOM_MEMBERS}" | grep -q "@manager:"; then
    log_pass "Manager IS in Leader Room (correct)"
else
    log_fail "Manager NOT in Leader Room (should be)"
fi

# ============================================================
# Section 7: Verify Canonical Team Leader Skill Guidance
# ============================================================
log_section "Verify Canonical Team Leader Skill Guidance"

LEADER_HOME="/root/agentteams-fs/agents/${TEST_LEADER}"
PROJECT_SKILL=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/${TEST_LEADER}/skills/project-management/SKILL.md" 2>/dev/null)
TASK_SKILL=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/${TEST_LEADER}/skills/task-management/SKILL.md" 2>/dev/null)
COMMUNICATION_SKILL=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/${TEST_LEADER}/skills/communication/SKILL.md" 2>/dev/null)
COORDINATION_SKILL=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/${TEST_LEADER}/skills/team-coordination/SKILL.md" 2>/dev/null)
LEADER_AGENTS=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/${TEST_LEADER}/AGENTS.md" 2>/dev/null)

assert_contains "${PROJECT_SKILL}" "projectflow" "project-management documents projectflow"
assert_contains "${PROJECT_SKILL}" "Project state is tool-owned" "project-management forbids manual project state mutation"
assert_contains "${PROJECT_SKILL}" "ready_nodes" "project-management documents DAG ready nodes"
assert_contains "${TASK_SKILL}" "taskflow" "task-management documents taskflow"
assert_contains "${TASK_SKILL}" "Task state is tool-owned" "task-management forbids manual task state mutation"
assert_contains "${TASK_SKILL}" "delegate_task does not send Matrix messages" "task-management requires explicit Team Room notification"
assert_contains "${TASK_SKILL}" "Mandatory next action after \`delegate_task\`" "task-management requires message after delegate_task"
assert_contains "${TASK_SKILL}" "delegate_task" "task-management documents task delegation"
assert_contains "${COMMUNICATION_SKILL}" "An assignment intent sentence is not an assignment" "communication forbids intent-only assignment replies"
assert_contains "${COMMUNICATION_SKILL}" "this cross-room \`message\` call is mandatory" "communication requires cross-room message for Team work"
assert_contains "${COORDINATION_SKILL}" "DAG" "team-coordination documents DAG strategy"
assert_contains "${COORDINATION_SKILL}" "Loop" "team-coordination documents Loop strategy"
assert_contains "${LEADER_AGENTS}" "Project/tool boundary" "Leader AGENTS documents tool-owned project/task boundary"
assert_contains "${LEADER_AGENTS}" "taskflow(delegate_task) only creates and publishes task state" "Leader AGENTS requires Team Room assignment after taskflow"
assert_contains "${LEADER_AGENTS}" "do not send DAG plans" "Leader AGENTS forbids interim Leader DM planning before Team Room assignment"
assert_contains "${LEADER_AGENTS}" "first visible non-\`NO_REPLY\` message" "Leader AGENTS requires NO_REPLY before first visible Team Room assignment"
assert_contains "${LEADER_AGENTS}" "Do not send a natural-language preamble before the tool call" "Leader AGENTS forbids visible tool preambles"
assert_contains "${LEADER_AGENTS}" "already loaded into your system prompt" "Leader AGENTS uses injected team context without visible topology preamble"
assert_contains "${LEADER_AGENTS}" "Delegation send boundary" "Leader AGENTS distinguishes intent from assignment send"
assert_contains "${LEADER_AGENTS}" "a delegation intent sentence is not a Worker assignment" "Leader AGENTS forbids intent-only delegation replies"

# ============================================================
# Section 8: End-to-End LLM Test — Admin delegates via Leader DM
# ============================================================
log_section "E2E: Admin Delegates Task via Leader DM"

if ! require_llm_key; then
    log_info "SKIP: No LLM API key — skipping e2e LLM test"
    test_teardown "21-team-project-dag"
    test_summary
    exit 0
fi

# Wait for worker containers
for w in "${TEST_LEADER}" "${TEST_W1}" "${TEST_W2}"; do
    wait_for_worker_container "${w}" 60 || log_fail "Container ${w} not running"
done

# Container running only proves Docker accepted the worker process. CoPaw needs
# a short bootstrap window before its Matrix channel is ready to accept invites.
if [ "${TEST_WORKER_RUNTIME:-}" = "copaw" ] || [ "${HICLAW_DEFAULT_WORKER_RUNTIME:-}" = "copaw" ] || [ "${AGENTTEAMS_DEFAULT_WORKER_RUNTIME:-}" = "copaw" ]; then
    for w in "${TEST_LEADER}" "${TEST_W1}" "${TEST_W2}"; do
        log_info "Waiting for CoPaw Worker readiness probe before room membership checks (${w})..."
        PROBE_OUTPUT=$(check_copaw_worker_probes "${w}" "ready" 60)
        PROBE_STATUS=$?
        if [ "${PROBE_STATUS}" = "0" ]; then
            log_pass "CoPaw Worker ${w} readiness probe is ready"
            log_info "${PROBE_OUTPUT}"
        else
            log_fail "CoPaw Worker ${w} readiness probe did not become ready"
            log_info "${PROBE_OUTPUT}"
        fi
    done
fi

# Send task from Admin directly in Leader DM
assert_not_empty "${LEADER_DM}" "Leader DM room exists"

# Container running != Matrix client joined. Default history_visibility is "shared",
# so if admin sends the task before Leader joins the DM, the Leader never sees it.
# Wait for Leader to actually join LEADER_DM (and Team Room, so it can coordinate).
ADMIN_LOGIN_TOKEN=$(matrix_login "${TEST_ADMIN_USER}" "${TEST_ADMIN_PASSWORD}" 2>/dev/null | jq -r '.access_token // empty')
TEAM_MEMBERS_JOINED=true
if [ -z "${ADMIN_LOGIN_TOKEN}" ] || [ "${ADMIN_LOGIN_TOKEN}" = "null" ]; then
    log_fail "Admin Matrix login failed (cannot verify Leader join)"
    TEAM_MEMBERS_JOINED=false
else
    LEADER_MATRIX_ID="@${TEST_LEADER}:${TEST_MATRIX_DOMAIN}"
    W1_MATRIX_ID="@${TEST_W1}:${TEST_MATRIX_DOMAIN}"
    W2_MATRIX_ID="@${TEST_W2}:${TEST_MATRIX_DOMAIN}"

    log_info "Waiting for Leader to join Leader DM..."
    if matrix_wait_for_user_joined "${ADMIN_LOGIN_TOKEN}" "${LEADER_DM}" "${LEADER_MATRIX_ID}" 60; then
        log_pass "Leader joined Leader DM"
    else
        log_fail "Leader did not join Leader DM within 60s"
        TEAM_MEMBERS_JOINED=false
    fi

    log_info "Waiting for Leader and workers to join Team Room..."
    for uid in "${LEADER_MATRIX_ID}" "${W1_MATRIX_ID}" "${W2_MATRIX_ID}"; do
        if matrix_wait_for_user_joined "${ADMIN_LOGIN_TOKEN}" "${TEAM_ROOM}" "${uid}" 60; then
            log_pass "${uid} joined Team Room"
        else
            log_fail "${uid} did not join Team Room within 60s"
            TEAM_MEMBERS_JOINED=false
        fi
    done
fi

if [ "${TEAM_MEMBERS_JOINED}" != "true" ]; then
    log_info "Skipping LLM coordination check because Team Room membership did not become ready"
    test_summary
    exit $?
fi

exec_in_manager bash -c '
TOKEN=$(curl -sf -X POST "http://127.0.0.1:6167/_matrix/client/v3/login" \
    -H "Content-Type: application/json" \
    -d "{\"type\":\"m.login.password\",\"identifier\":{\"type\":\"m.id.user\",\"user\":\"admin\"},\"password\":\"'"${TEST_ADMIN_PASSWORD}"'\"}" | jq -r ".access_token")
ROOM_ENC=$(echo "'"${LEADER_DM}"'" | sed "s/!/%21/g")
TXN=$(date +%s%N)
curl -sf -X PUT "http://127.0.0.1:6167/_matrix/client/v3/rooms/${ROOM_ENC}/send/m.room.message/${TXN}" \
    -H "Authorization: Bearer ${TOKEN}" \
    -H "Content-Type: application/json" \
    -d "{\"msgtype\":\"m.text\",\"body\":\"Please build a simple REST API for a todo-list app. The dev worker should design the API endpoints first, then implement them. The QA worker should write test cases after the API design is done. Coordinate your team and report back when everything is complete.\"}"
' 2>/dev/null

log_info "Task sent to Leader via Leader DM. Monitoring rooms..."

# Poll for Leader activity in Team Room.
#
# This test validates the routing boundary, not full project completion. If the
# Leader responds in Leader DM while no assignment appears in Team Room, the
# route is wrong and extra waiting only slows CI. If there is no response at
# all, keep a short 120s ceiling for startup jitter.
TEAM_ROOM_ENC=$(echo "${TEAM_ROOM}" | sed 's/!/%21/g')
LEADER_DM_ENC=$(echo "${LEADER_DM}" | sed 's/!/%21/g')
MAX_COORDINATION_POLLS="${MAX_COORDINATION_POLLS:-4}"
MAX_DM_ONLY_POLLS="${MAX_DM_ONLY_POLLS:-1}"

LEADER_RESPONDED=false
TEAM_COORDINATED=false
RUNTIME_ERROR=false
DM_ONLY_POLLS=0
for i in $(seq 1 "${MAX_COORDINATION_POLLS}"); do
    sleep 30
    log_info "Polling rooms... (${i}/${MAX_COORDINATION_POLLS}, elapsed: $((i*30))s)"

    # Check Team Room for Leader messages
    TEAM_MSGS=$(exec_in_manager bash -c '
        TOKEN=$(curl -sf -X POST "http://127.0.0.1:6167/_matrix/client/v3/login" \
            -H "Content-Type: application/json" \
            -d "{\"type\":\"m.login.password\",\"identifier\":{\"type\":\"m.id.user\",\"user\":\"admin\"},\"password\":\"'"${TEST_ADMIN_PASSWORD}"'\"}" | jq -r ".access_token")
        curl -sf "http://127.0.0.1:6167/_matrix/client/v3/rooms/'"${TEAM_ROOM_ENC}"'/messages?dir=b&limit=10" \
            -H "Authorization: Bearer ${TOKEN}" | jq -r ".chunk[] | select(.type == \"m.room.message\") | \"\(.sender | split(\":\")[0]): \(.content.body[0:200])\""
    ' 2>/dev/null)

    if echo "${TEAM_MSGS}" | grep -q "@${TEST_LEADER}:"; then
        log_info "Leader is active in Team Room"
        LEADER_RESPONDED=true
        TEAM_COORDINATED=true
    fi

    # Check if any worker has responded in Team Room
    if echo "${TEAM_MSGS}" | grep -qi "${TEST_W1}\|${TEST_W2}"; then
        log_info "Workers are responding in Team Room"
        TEAM_COORDINATED=true
        break
    fi

    # Also check Leader DM for any response back to admin
    DM_MSGS=$(exec_in_manager bash -c '
        TOKEN=$(curl -sf -X POST "http://127.0.0.1:6167/_matrix/client/v3/login" \
            -H "Content-Type: application/json" \
            -d "{\"type\":\"m.login.password\",\"identifier\":{\"type\":\"m.id.user\",\"user\":\"admin\"},\"password\":\"'"${TEST_ADMIN_PASSWORD}"'\"}" | jq -r ".access_token")
        curl -sf "http://127.0.0.1:6167/_matrix/client/v3/rooms/'"${LEADER_DM_ENC}"'/messages?dir=b&limit=5" \
            -H "Authorization: Bearer ${TOKEN}" | jq -r ".chunk[] | select(.type == \"m.room.message\" and (.sender | contains(\"'"${TEST_LEADER}"'\"))) | .content.body[0:200]"
    ' 2>/dev/null)

    if [ -n "${DM_MSGS}" ]; then
        log_info "Leader responded in Leader DM"
        LEADER_RESPONDED=true
        if echo "${DM_MSGS}" | grep -qi "Error:\\|No active model configured"; then
            RUNTIME_ERROR=true
        fi
    fi

    if [ "${LEADER_RESPONDED}" = "true" ] && [ "${TEAM_COORDINATED}" != "true" ]; then
        DM_ONLY_POLLS=$((DM_ONLY_POLLS + 1))
        if [ "${DM_ONLY_POLLS}" -ge "${MAX_DM_ONLY_POLLS}" ]; then
            log_info "Leader is replying in Leader DM but has not posted a Team Room assignment; failing fast"
            break
        fi
    fi
done

if [ "${RUNTIME_ERROR}" = "true" ]; then
    log_fail "Leader returned a runtime error"
elif [ "${LEADER_RESPONDED}" = "true" ] && [ "${TEAM_COORDINATED}" = "true" ]; then
    log_pass "Leader received and processed task from Admin via Leader DM"
else
    log_fail "Leader did not coordinate the task in Team Room within timeout"
fi

# Final snapshot of all rooms
log_section "Final Room Snapshot"

exec_in_manager bash -c '
TOKEN=$(curl -sf -X POST "http://127.0.0.1:6167/_matrix/client/v3/login" \
    -H "Content-Type: application/json" \
    -d "{\"type\":\"m.login.password\",\"identifier\":{\"type\":\"m.id.user\",\"user\":\"admin\"},\"password\":\"'"${TEST_ADMIN_PASSWORD}"'\"}" | jq -r ".access_token")

echo "--- Leader DM (Admin <-> Leader) ---"
ROOM_ENC=$(echo "'"${LEADER_DM}"'" | sed "s/!/%21/g")
curl -sf "http://127.0.0.1:6167/_matrix/client/v3/rooms/${ROOM_ENC}/messages?dir=b&limit=10" \
    -H "Authorization: Bearer ${TOKEN}" | jq -r ".chunk[] | select(.type == \"m.room.message\") | \"\(.sender | split(\":\")[0]): \(.content.body[0:300])\""

echo ""
echo "--- Team Room ---"
ROOM_ENC=$(echo "'"${TEAM_ROOM}"'" | sed "s/!/%21/g")
curl -sf "http://127.0.0.1:6167/_matrix/client/v3/rooms/${ROOM_ENC}/messages?dir=b&limit=15" \
    -H "Authorization: Bearer ${TOKEN}" | jq -r ".chunk[] | select(.type == \"m.room.message\") | \"\(.sender | split(\":\")[0]): \(.content.body[0:300])\""

echo ""
echo "--- Leader Room (Manager <-> Leader) ---"
ROOM_ENC=$(echo "'"${LEADER_ROOM}"'" | sed "s/!/%21/g")
curl -sf "http://127.0.0.1:6167/_matrix/client/v3/rooms/${ROOM_ENC}/messages?dir=b&limit=10" \
    -H "Authorization: Bearer ${TOKEN}" | jq -r ".chunk[] | select(.type == \"m.room.message\") | \"\(.sender | split(\":\")[0]): \(.content.body[0:300])\""
' 2>&1

log_info "Environment NOT cleaned up — inspect via Element at http://127.0.0.1:${TEST_ELEMENT_PORT:-18088}"

test_teardown "21-team-project-dag"
test_summary
