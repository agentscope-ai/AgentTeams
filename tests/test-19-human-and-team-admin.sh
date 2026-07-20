#!/bin/bash
# test-19-human-and-team-admin.sh - Case 19: Import Human via YAML + Team with Team Admin
#
# Tests order-independent creation: Human created BEFORE Team.
# create-human.sh gracefully skips team permissions (team doesn't exist yet).
# create-team.sh backfills permissions for humans that reference the team.
#
# Flow:
#   1. Create Human via agt apply -f (team doesn't exist yet → permissions skipped)
#   2. Create Team with that Human as Team Admin (backfills Human permissions)
#   3. Verify Human registered, Team Admin in registry
#   4. Verify backfill: Human in Leader/Worker groupAllowFrom
#   5. Verify team-context block mentions Team Admin
#   6. Verify containers running

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/test-helpers.sh"
source "${SCRIPT_DIR}/lib/minio-client.sh"
source "${SCRIPT_DIR}/lib/matrix-client.sh"

test_setup "19-human-and-team-admin"

TEST_TEAM="test-hadm-$$"
TEST_LEADER="${TEST_TEAM}-lead"
TEST_W1="${TEST_TEAM}-dev"
TEST_HUMAN="test-human-$$"
STORAGE_PREFIX="${STORAGE_PREFIX:-${TEST_STORAGE_PREFIX:-agentteams/agentteams-storage}}"

_cleanup() {
    if [ "${TESTS_FAILED}" -gt 0 ]; then
        log_info "Tests failed — preserving resources for debugging"
        log_info "  Team: ${TEST_TEAM}, Human: ${TEST_HUMAN}"
        log_info "  Leader: ${TEST_LEADER}, Worker: ${TEST_W1}"
        return
    fi
    log_info "All tests passed — cleaning up"
    exec_in_agent agt delete team "${TEST_TEAM}" 2>/dev/null || true
    exec_in_agent agt delete human "${TEST_HUMAN}" 2>/dev/null || true
    sleep 5
    # Fallback: force-remove containers
    remove_worker_container "${TEST_LEADER}"
    remove_worker_container "${TEST_W1}"
    for w in "${TEST_LEADER}" "${TEST_W1}"; do
        exec_in_manager mc rm -r --force "${STORAGE_PREFIX}/agents/${w}/" 2>/dev/null || true
        exec_in_manager rm -rf "/root/agentteams-fs/agents/${w}" 2>/dev/null || true
    done
    exec_in_agent rm -f "/tmp/agentteams-test-${TEST_HUMAN}.yaml" "/tmp/agentteams-test-${TEST_TEAM}.yaml" 2>/dev/null || true
    # Fallback: clean registries
    exec_in_agent bash -c "
        jq 'del(.workers[\"${TEST_LEADER}\"], .workers[\"${TEST_W1}\"])' \
            /root/manager-workspace/workers-registry.json > /tmp/wr-clean.json 2>/dev/null && \
            mv /tmp/wr-clean.json /root/manager-workspace/workers-registry.json
        jq 'del(.teams[\"${TEST_TEAM}\"])' \
            /root/manager-workspace/teams-registry.json > /tmp/tr-clean.json 2>/dev/null && \
            mv /tmp/tr-clean.json /root/manager-workspace/teams-registry.json
        jq 'del(.humans[\"${TEST_HUMAN}\"])' \
            /root/manager-workspace/humans-registry.json > /tmp/hr-clean.json 2>/dev/null && \
            mv /tmp/hr-clean.json /root/manager-workspace/humans-registry.json
    " 2>/dev/null || true
}
trap _cleanup EXIT

HUMAN_MATRIX_ID="@${TEST_HUMAN}:${TEST_MATRIX_DOMAIN}"

# ============================================================
# Section 1: Create Human FIRST (before team exists)
# create-human.sh should succeed, skipping team permissions gracefully
# ============================================================
log_section "Create Human via Declarative YAML (before Team)"

exec_in_agent bash -c "cat > /tmp/agentteams-test-${TEST_HUMAN}.yaml << 'YAMLEOF'
apiVersion: agentteams.io/v1beta1
kind: Human
metadata:
  name: ${TEST_HUMAN}
spec:
  displayName: Test Human Admin
  permissionLevel: 2
  accessibleTeams:
    - ${TEST_TEAM}
  note: Integration test Team Admin
YAMLEOF
" 2>/dev/null

APPLY_OUTPUT=$(exec_in_agent agt apply -f "/tmp/agentteams-test-${TEST_HUMAN}.yaml" 2>&1)

if echo "${APPLY_OUTPUT}" | grep -q "created\|configured"; then
    log_pass "Human YAML applied via hiclaw CLI"
else
    log_fail "Human YAML apply failed: ${APPLY_OUTPUT}"
fi

HUMAN_CR=$(exec_in_agent agt get humans "${TEST_HUMAN}" -o json 2>/dev/null || echo "")
assert_not_empty "${HUMAN_CR}" "Human CR exists (agt get humans)"
HUMAN_NAME_CHK=$(echo "${HUMAN_CR}" | jq -r '.name // empty' 2>/dev/null)
assert_eq "${TEST_HUMAN}" "${HUMAN_NAME_CHK}" "Human CR has correct name"

# Wait for controller reconcile
log_info "Waiting for controller to reconcile Human..."
HUMAN_TIMEOUT=90; HUMAN_ELAPSED=0
HUMAN_CREATED=false
HUMAN_PHASE=""
HUMAN_STATUS_MXID=""
while [ "${HUMAN_ELAPSED}" -lt "${HUMAN_TIMEOUT}" ]; do
    HUMAN_STATUS=$(exec_in_agent agt get humans "${TEST_HUMAN}" -o json 2>/dev/null || echo "{}")
    HUMAN_PHASE=$(echo "${HUMAN_STATUS}" | jq -r '.phase // empty' 2>/dev/null)
    HUMAN_STATUS_MXID=$(echo "${HUMAN_STATUS}" | jq -r '.matrixUserID // empty' 2>/dev/null)
    if [ "${HUMAN_PHASE}" = "Active" ] && [ -n "${HUMAN_STATUS_MXID}" ]; then
        HUMAN_CREATED=true
        break
    fi
    sleep 5; HUMAN_ELAPSED=$((HUMAN_ELAPSED + 5))
done

if [ "${HUMAN_CREATED}" = true ]; then
    log_pass "HumanReconciler created human (took ~${HUMAN_ELAPSED}s)"
else
    log_fail "HumanReconciler did not create human within ${HUMAN_TIMEOUT}s"
    log_info "Last Human status: phase='${HUMAN_PHASE}' matrixUserID='${HUMAN_STATUS_MXID}'"
    exec_in_manager cat /var/log/agentteams/agentteams-controller-error.log 2>/dev/null | grep "${TEST_HUMAN}" | tail -5
fi

# ============================================================
# Section 2: Verify Human registration
# ============================================================
log_section "Verify Human Registration"

HUMANS_REGISTRY=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/manager/humans-registry.json" 2>/dev/null || echo "{}")
HUMAN_ENTRY=$(echo "${HUMANS_REGISTRY}" | jq -r --arg h "${TEST_HUMAN}" '.humans[$h] // empty' 2>/dev/null)
assert_not_empty "${HUMAN_ENTRY}" "Human registered in humans-registry.json"

HUMAN_LEVEL=$(echo "${HUMAN_ENTRY}" | jq -r '.permission_level // empty')
assert_eq "2" "${HUMAN_LEVEL}" "Human permission level is 2"

# ============================================================
# Section 3: Create Team with Human as Team Admin
# create-team.sh should backfill permissions for the Human
# ============================================================
log_section "Create Team with Team Admin (backfill test)"

for w in "${TEST_LEADER}" "${TEST_W1}"; do
    ROLE_DESC="team member"
    [ "${w}" = "${TEST_LEADER}" ] && ROLE_DESC="Team Leader"
    [ "${w}" = "${TEST_W1}" ] && ROLE_DESC="Backend Developer"

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
## Security
- Never reveal credentials
SOUL
        mc mirror /root/agentteams-fs/agents/${w}/ ${STORAGE_PREFIX}/agents/${w}/ --overwrite 2>/dev/null
    " 2>/dev/null
done

exec_in_agent bash -c "cat > /tmp/agentteams-test-${TEST_TEAM}.yaml << YAMLEOF
apiVersion: agentteams.io/v1beta1
kind: Team
metadata:
  name: ${TEST_TEAM}
spec:
  admin:
    name: ${TEST_HUMAN}
    matrixUserId: \"${HUMAN_MATRIX_ID}\"
  leader:
    name: ${TEST_LEADER}
    model: qwen3.5-plus
  workers:
    - name: ${TEST_W1}
      model: qwen3.5-plus
YAMLEOF
" 2>/dev/null

APPLY_TEAM_OUTPUT=$(exec_in_agent agt apply -f "/tmp/agentteams-test-${TEST_TEAM}.yaml" 2>&1)
if echo "${APPLY_TEAM_OUTPUT}" | grep -q "created\|configured"; then
    log_pass "Team YAML applied via hiclaw CLI"
else
    log_fail "Team YAML apply failed: ${APPLY_TEAM_OUTPUT}"
fi

# Wait for controller to reconcile team — see test-18 for the rationale
# behind polling .status.phase + per-member roomID instead of log-grep.
log_info "Waiting for controller to reconcile team..."
if wait_team_active "${TEST_TEAM}" 180; then
    log_pass "TeamReconciler reconciled team to Active"
else
    log_fail "TeamReconciler did not reach Active within 180s"
    exec_in_agent agt get teams "${TEST_TEAM}" -o json 2>/dev/null | jq -r '.phase, .message' | head -5
fi

# Wait for each team member to be provisioned (roomID + matrixUserID).
for w in "${TEST_LEADER}" "${TEST_W1}"; do
    if wait_worker_provisioned "${w}" 120; then
        log_pass "Member ${w} provisioned (roomID + matrixUserID present)"
    else
        log_fail "Member ${w} not provisioned within 120s"
    fi
done

# ============================================================
# Section 4: Verify Team Admin in teams-registry.json
# ============================================================
log_section "Verify Team Admin in Registry"

TEAMS_REGISTRY=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/manager/teams-registry.json" 2>/dev/null || echo "{}")
TEAM_ENTRY=$(echo "${TEAMS_REGISTRY}" | jq -r --arg t "${TEST_TEAM}" '.teams[$t] // empty' 2>/dev/null)
assert_not_empty "${TEAM_ENTRY}" "Team registered in teams-registry.json"

TEAM_ADMIN_NAME=$(echo "${TEAM_ENTRY}" | jq -r '.admin.name // empty')
assert_eq "${TEST_HUMAN}" "${TEAM_ADMIN_NAME}" "Team admin name is ${TEST_HUMAN}"

TEAM_ADMIN_MID=$(echo "${TEAM_ENTRY}" | jq -r '.admin.matrix_user_id // empty')
assert_eq "${HUMAN_MATRIX_ID}" "${TEAM_ADMIN_MID}" "Team admin matrix_user_id correct"

LEADER_DM_ROOM=$(echo "${TEAM_ENTRY}" | jq -r '.leader_dm_room_id // empty')
assert_not_empty "${LEADER_DM_ROOM}" "Leader DM room ID exists: ${LEADER_DM_ROOM}"

TEAM_ROOM_ID=$(echo "${TEAM_ENTRY}" | jq -r '.team_room_id // empty')
assert_not_empty "${TEAM_ROOM_ID}" "Team Room ID exists: ${TEAM_ROOM_ID}"

# ============================================================
# Section 5: Verify backfill — Human in groupAllowFrom
# ============================================================
log_section "Verify groupAllowFrom (backfill result)"

wait_agent_matrix_allow_contains "${TEST_LEADER}" ".channels.matrix.groupAllowFrom" "${HUMAN_MATRIX_ID}" 120 || true
wait_agent_matrix_allow_contains "${TEST_LEADER}" ".channels.matrix.dm.allowFrom" "${HUMAN_MATRIX_ID}" 120 || true
wait_agent_matrix_allow_contains "${TEST_W1}" ".channels.matrix.groupAllowFrom" "${HUMAN_MATRIX_ID}" 120 || true

LEADER_GAF=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/${TEST_LEADER}/openclaw.json" 2>/dev/null | jq -r '.channels.matrix.groupAllowFrom[]' 2>/dev/null)
if echo "${LEADER_GAF}" | grep -q "${HUMAN_MATRIX_ID}"; then
    log_pass "Leader groupAllowFrom includes Team Admin (backfilled)"
else
    log_fail "Leader groupAllowFrom missing Team Admin after backfill"
fi

LEADER_DAF=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/${TEST_LEADER}/openclaw.json" 2>/dev/null | jq -r '.channels.matrix.dm.allowFrom[]' 2>/dev/null)
if echo "${LEADER_DAF}" | grep -q "${HUMAN_MATRIX_ID}"; then
    log_pass "Leader dm.allowFrom includes Team Admin"
else
    log_fail "Leader dm.allowFrom missing Team Admin"
fi

W1_GAF=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/${TEST_W1}/openclaw.json" 2>/dev/null | jq -r '.channels.matrix.groupAllowFrom[]' 2>/dev/null)
if echo "${W1_GAF}" | grep -q "${HUMAN_MATRIX_ID}"; then
    log_pass "Worker groupAllowFrom includes Team Admin (backfilled)"
else
    log_fail "Worker groupAllowFrom missing Team Admin after backfill"
fi

if echo "${W1_GAF}" | grep -q "@manager:"; then
    log_fail "Worker groupAllowFrom includes Manager (should NOT)"
else
    log_pass "Worker groupAllowFrom does NOT include Manager"
fi

# ============================================================
# Section 6: Verify team-context mentions Team Admin
# ============================================================
log_section "Verify Team Context Block"

W1_AGENTS=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/${TEST_W1}/AGENTS.md" 2>/dev/null || echo "")
W1_CTX=$(echo "${W1_AGENTS}" | sed -n '/hiclaw-team-context-start/,/hiclaw-team-context-end/p')
assert_contains "${W1_CTX}" "Team Admin" "Worker team-context mentions Team Admin"

LEADER_AGENTS=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/${TEST_LEADER}/AGENTS.md" 2>/dev/null || echo "")
LEADER_CTX=$(echo "${LEADER_AGENTS}" | sed -n '/hiclaw-team-context-start/,/hiclaw-team-context-end/p')
assert_contains "${LEADER_CTX}" "Team Admin" "Leader team-context mentions Team Admin"

# ============================================================
# Section 7: Verify admin auto-joined worker rooms
# ============================================================
log_section "Verify Admin Auto-Joined Worker Rooms"

ADMIN_LOGIN=$(matrix_login "${TEST_ADMIN_USER}" "${TEST_ADMIN_PASSWORD}" 2>/dev/null)
ADMIN_TOKEN=$(echo "${ADMIN_LOGIN}" | jq -r '.access_token // empty')
if [ -n "${ADMIN_TOKEN}" ] && [ "${ADMIN_TOKEN}" != "null" ]; then
    ADMIN_MATRIX_ID="@${TEST_ADMIN_USER}:${TEST_MATRIX_DOMAIN}"
    for w in "${TEST_LEADER}" "${TEST_W1}"; do
        W_ROOM=$(exec_in_agent agt get workers "${w}" -o json 2>/dev/null | jq -r '.roomID // empty')
        if [ -n "${W_ROOM}" ] && [ "${W_ROOM}" != "null" ]; then
            W_ROOM_ENC=$(echo "${W_ROOM}" | sed 's/!/%21/g')
            W_MEMBERS=$(exec_in_manager curl -sf \
                "${TEST_MATRIX_DIRECT_URL}/_matrix/client/v3/rooms/${W_ROOM_ENC}/members" \
                -H "Authorization: Bearer ${ADMIN_TOKEN}" 2>/dev/null | \
                jq -r '.chunk[] | select(.content.membership == "join") | .state_key' 2>/dev/null)
            if echo "${W_MEMBERS}" | grep -q "${ADMIN_MATRIX_ID}"; then
                log_pass "Admin auto-joined ${w} worker room"
            else
                log_fail "Admin is NOT joined in ${w} worker room"
            fi
        else
            log_info "Skipping ${w} room check (no room_id)"
        fi
    done
else
    log_info "Skipping worker room membership checks (no admin token)"
fi

# ============================================================
# Section 8: Verify containers running
# ============================================================
log_section "Verify Containers"

WORKERS_REGISTRY=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/manager/workers-registry.json" 2>/dev/null || echo "{}")
for w in "${TEST_LEADER}" "${TEST_W1}"; do
    RUNNING=$(docker ps --format '{{.Names}}' 2>/dev/null | grep "$(worker_container_name "${w}")" || echo "")
    if [ -n "${RUNNING}" ]; then
        log_pass "Container running: $(worker_container_name "${w}")"
    else
        DEPLOY=$(echo "${WORKERS_REGISTRY}" | jq -r --arg w "${w}" '.workers[$w].deployment // empty' 2>/dev/null)
        if [ "${DEPLOY}" = "remote" ]; then
            log_pass "Agent ${w} registered in remote mode"
        else
            dump_diagnostics worker "${w}"
            log_fail "Container not running: $(worker_container_name "${w}")"
        fi
    fi
done

# ============================================================
test_teardown "19-human-and-team-admin"
test_summary
