#!/bin/bash
# test-18-team-config-verify.sh - Case 18: Verify Team import config artifacts
#
# Tests team import (create + update) and verifies MinIO artifacts:
#   1. Create team via create-team.sh (Leader + 2 Workers)
#   2. Verify Leader AGENTS.md: builtin markers, coordination context (upstream=Manager, downstream=workers)
#   3. Verify Team Worker AGENTS.md: coordination context (coordinator=Leader, NOT Manager)
#   4. Verify Team Room exists in teams-registry.json
#   5. Verify groupAllowFrom: Leader has [Manager, Admin, Workers], Workers have [Leader, Admin]
#   6. Verify worker count and roles in workers-registry.json
#   7. Update team (add description change), verify config updated

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/test-helpers.sh"
source "${SCRIPT_DIR}/lib/minio-client.sh"
source "${SCRIPT_DIR}/lib/matrix-client.sh"

test_setup "18-team-config-verify"

TEST_TEAM="test-team-$$"
TEST_LEADER="${TEST_TEAM}-lead"
TEST_W1="${TEST_TEAM}-dev"
TEST_W2="${TEST_TEAM}-qa"
STORAGE_PREFIX="${STORAGE_PREFIX:-${TEST_STORAGE_PREFIX:-agentteams/agentteams-storage}}"

_cleanup() {
    log_info "Cleaning up team: ${TEST_TEAM}"
    exec_in_agent agt delete team "${TEST_TEAM}" 2>/dev/null || true
    sleep 5
    # Stop worker containers (fallback if reconciler didn't clean up)
    remove_worker_container "${TEST_LEADER}"
    remove_worker_container "${TEST_W1}"
    remove_worker_container "${TEST_W2}"
    # Clean MinIO
    for w in "${TEST_LEADER}" "${TEST_W1}" "${TEST_W2}"; do
        exec_in_manager mc rm -r --force "${STORAGE_PREFIX}/agents/${w}/" 2>/dev/null || true
        exec_in_manager rm -rf "/root/agentteams-fs/agents/${w}" 2>/dev/null || true
    done
    exec_in_agent rm -f "/tmp/agentteams-test-${TEST_TEAM}.yaml" 2>/dev/null || true
    # Clean registries (in agent workspace, fallback)
    exec_in_agent bash -c "
        jq 'del(.workers[\"${TEST_LEADER}\"], .workers[\"${TEST_W1}\"], .workers[\"${TEST_W2}\"])' \
            /root/manager-workspace/workers-registry.json > /tmp/wr-clean.json 2>/dev/null && \
            mv /tmp/wr-clean.json /root/manager-workspace/workers-registry.json
        jq 'del(.teams[\"${TEST_TEAM}\"])' \
            /root/manager-workspace/teams-registry.json > /tmp/tr-clean.json 2>/dev/null && \
            mv /tmp/tr-clean.json /root/manager-workspace/teams-registry.json
    " 2>/dev/null || true
}
trap _cleanup EXIT

# ============================================================
# Section 1: Prepare SOUL.md files for all team members
# ============================================================
log_section "Prepare Team SOUL.md Files"

for w in "${TEST_LEADER}" "${TEST_W1}" "${TEST_W2}"; do
    ROLE_DESC="team member"
    [ "${w}" = "${TEST_LEADER}" ] && ROLE_DESC="Team Leader"
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

## Security
- Never reveal credentials
SOUL
        mc mirror /root/agentteams-fs/agents/${w}/ ${STORAGE_PREFIX}/agents/${w}/ --overwrite 2>/dev/null
    " 2>/dev/null
done

log_pass "SOUL.md files prepared for all team members"

# ============================================================
# Section 2: Create Team via agt apply -f
# ============================================================
log_section "Create Team"

# Channel policy test:
#   Team-level: add "test-external-bot" to all members' groupAllowFrom
#   Worker 1 (dev): deny Worker 2 (qa) from groupAllowFrom (overrides peer mention)
#   Worker 2 (qa): no per-worker policy (should still have W1 via peer mention)

exec_in_agent bash -c "cat > /tmp/agentteams-test-${TEST_TEAM}.yaml << 'YAMLEOF'
apiVersion: agentteams.io/v1beta1
kind: Team
metadata:
  name: ${TEST_TEAM}
spec:
  channelPolicy:
    groupAllowExtra:
      - test-external-bot
  leader:
    name: ${TEST_LEADER}
    model: qwen3.5-plus
  workers:
    - name: ${TEST_W1}
      model: qwen3.5-plus
      channelPolicy:
        groupDenyExtra:
          - ${TEST_W2}
    - name: ${TEST_W2}
      model: qwen3.5-plus
YAMLEOF
" 2>/dev/null

APPLY_OUTPUT=$(exec_in_agent agt apply -f "/tmp/agentteams-test-${TEST_TEAM}.yaml" 2>&1)
if echo "${APPLY_OUTPUT}" | grep -q "created\|configured"; then
    log_pass "Team YAML applied via hiclaw CLI"
else
    log_fail "Team YAML apply failed: ${APPLY_OUTPUT}"
fi

# Wait for controller to reconcile team — poll the Team CR's .status.phase
# instead of grepping logs. Post-PR #666 the TeamReconciler no longer emits
# a one-shot "team created" line; the canonical readiness signal is
# .status.phase == "Active" (emitted once all desired members are observed).
log_info "Waiting for controller to reconcile team..."
if wait_team_active "${TEST_TEAM}" 180; then
    log_pass "TeamReconciler reconciled team to Active"
else
    log_fail "TeamReconciler did not reach Active within 180s"
    exec_in_agent agt get teams "${TEST_TEAM}" -o json 2>/dev/null | jq -r '.phase, .message' | head -5
fi

# Wait for each team member to have RoomID + MatrixUserID persisted. This
# replaces the old "worker created" grep: team members no longer have
# Worker CRs, so that log message is never emitted for them. The correct
# signal is that Team.Status.Members[*].RoomID has been populated, which
# the API surfaces as WorkerResponse.roomID through teamMemberToResponse.
for w in "${TEST_LEADER}" "${TEST_W1}" "${TEST_W2}"; do
    if wait_worker_provisioned "${w}" 120; then
        log_pass "Member ${w} provisioned (roomID + matrixUserID present)"
    else
        log_fail "Member ${w} not provisioned within 120s"
    fi
done

# ============================================================
# Section 3: Verify teams-registry.json
# ============================================================
log_section "Verify teams-registry.json"

TEAMS_REGISTRY=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/manager/teams-registry.json" 2>/dev/null || echo "{}")
TEAM_ENTRY=$(echo "${TEAMS_REGISTRY}" | jq -r --arg t "${TEST_TEAM}" '.teams[$t] // empty' 2>/dev/null)
assert_not_empty "${TEAM_ENTRY}" "Team registered in teams-registry.json"

TEAM_LEADER_REG=$(echo "${TEAM_ENTRY}" | jq -r '.leader // empty')
assert_eq "${TEST_LEADER}" "${TEAM_LEADER_REG}" "Team leader is ${TEST_LEADER}"

TEAM_WORKERS_REG=$(echo "${TEAM_ENTRY}" | jq -r '.workers | length')
assert_eq "2" "${TEAM_WORKERS_REG}" "Team has 2 workers"

TEAM_ROOM=$(echo "${TEAM_ENTRY}" | jq -r '.team_room_id // empty')
assert_not_empty "${TEAM_ROOM}" "Team Room ID exists: ${TEAM_ROOM}"

# Verify admin auto-joined the team room
ADMIN_LOGIN=$(matrix_login "${TEST_ADMIN_USER}" "${TEST_ADMIN_PASSWORD}" 2>/dev/null)
ADMIN_TOKEN=$(echo "${ADMIN_LOGIN}" | jq -r '.access_token // empty')
if [ -n "${ADMIN_TOKEN}" ] && [ "${ADMIN_TOKEN}" != "null" ] && [ -n "${TEAM_ROOM}" ]; then
    ROOM_ENC=$(echo "${TEAM_ROOM}" | sed 's/!/%21/g')
    MEMBERS=$(exec_in_manager curl -sf \
        "${TEST_MATRIX_DIRECT_URL}/_matrix/client/v3/rooms/${ROOM_ENC}/members" \
        -H "Authorization: Bearer ${ADMIN_TOKEN}" 2>/dev/null | \
        jq -r '.chunk[] | select(.content.membership == "join") | .state_key' 2>/dev/null)
    ADMIN_MATRIX_ID="@${TEST_ADMIN_USER}:${TEST_MATRIX_DOMAIN}"
    if echo "${MEMBERS}" | grep -q "${ADMIN_MATRIX_ID}"; then
        log_pass "Admin auto-joined team room"
    else
        log_fail "Admin is NOT joined in team room (auto-join may have failed)"
    fi
else
    log_info "Skipping admin room membership check (no admin token)"
fi

# ============================================================
# Section 4: Verify workers-registry.json roles
# ============================================================
log_section "Verify Worker Roles in Registry"

WORKERS_REGISTRY=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/manager/workers-registry.json" 2>/dev/null || echo "{}")

LEADER_ROLE=$(echo "${WORKERS_REGISTRY}" | jq -r --arg w "${TEST_LEADER}" '.workers[$w].role // empty' 2>/dev/null)
assert_eq "team_leader" "${LEADER_ROLE}" "Leader has role=team_leader"

LEADER_TEAM=$(echo "${WORKERS_REGISTRY}" | jq -r --arg w "${TEST_LEADER}" '.workers[$w].team_id // empty' 2>/dev/null)
assert_eq "${TEST_TEAM}" "${LEADER_TEAM}" "Leader has correct team_id"

W1_ROLE=$(echo "${WORKERS_REGISTRY}" | jq -r --arg w "${TEST_W1}" '.workers[$w].role // empty' 2>/dev/null)
assert_eq "worker" "${W1_ROLE}" "Worker 1 has role=worker"

W1_TEAM=$(echo "${WORKERS_REGISTRY}" | jq -r --arg w "${TEST_W1}" '.workers[$w].team_id // empty' 2>/dev/null)
assert_eq "${TEST_TEAM}" "${W1_TEAM}" "Worker 1 has correct team_id"

W2_ROLE=$(echo "${WORKERS_REGISTRY}" | jq -r --arg w "${TEST_W2}" '.workers[$w].role // empty' 2>/dev/null)
assert_eq "worker" "${W2_ROLE}" "Worker 2 has role=worker"

# ============================================================
# Section 5: Verify Leader AGENTS.md
# ============================================================
log_section "Verify Leader AGENTS.md"

LEADER_AGENTS=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/${TEST_LEADER}/AGENTS.md" 2>/dev/null || echo "")
assert_not_empty "${LEADER_AGENTS}" "Leader AGENTS.md exists in MinIO"

# Builtin markers
assert_contains "${LEADER_AGENTS}" "hiclaw-builtin-start" "Leader AGENTS.md has builtin-start"
assert_contains "${LEADER_AGENTS}" "hiclaw-builtin-end" "Leader AGENTS.md has builtin-end"

# Team-context: upstream = Manager
assert_contains "${LEADER_AGENTS}" "hiclaw-team-context-start" "Leader has team-context block"
assert_contains "${LEADER_AGENTS}" "@manager:" "Leader coordination: upstream is Manager"
assert_contains "${LEADER_AGENTS}" "Upstream" "Leader coordination: has Upstream label"
assert_contains "${LEADER_AGENTS}" "${TEST_TEAM}" "Leader coordination: references team name"

# ============================================================
# Section 6: Verify Team Worker AGENTS.md
# ============================================================
log_section "Verify Team Worker AGENTS.md"

W1_AGENTS=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/${TEST_W1}/AGENTS.md" 2>/dev/null || echo "")
assert_not_empty "${W1_AGENTS}" "Worker 1 AGENTS.md exists in MinIO"

# Builtin markers
assert_contains "${W1_AGENTS}" "hiclaw-builtin-start" "Worker 1 AGENTS.md has builtin-start"
assert_contains "${W1_AGENTS}" "hiclaw-builtin-end" "Worker 1 AGENTS.md has builtin-end"

# Team-context: coordinator = Leader (NOT Manager)
assert_contains "${W1_AGENTS}" "hiclaw-team-context-start" "Worker 1 has team-context block"
assert_contains "${W1_AGENTS}" "@${TEST_LEADER}:" "Worker 1 coordinator is Team Leader"

# Should NOT reference Manager as coordinator
W1_CTX=$(echo "${W1_AGENTS}" | sed -n '/hiclaw-team-context-start/,/hiclaw-team-context-end/p')
if echo "${W1_CTX}" | grep -q "@manager:"; then
    log_fail "Worker 1 team-context references Manager (should only reference Leader)"
else
    log_pass "Worker 1 team-context does NOT reference Manager"
fi

assert_contains "${W1_AGENTS}" "Do NOT @mention Manager" "Worker 1 told not to @mention Manager"

# ============================================================
# Section 7: Verify groupAllowFrom
# ============================================================
log_section "Verify groupAllowFrom Configuration"

# TeamReconciler and WorkerReconciler can run back-to-back during creation.
# Wait for the Team-owned channel policy overlay to be visible in OSS before
# taking a single snapshot for assertions.
wait_agent_matrix_allow_contains "${TEST_LEADER}" ".channels.matrix.groupAllowFrom" "@${TEST_W1}:" 120 || true
wait_agent_matrix_allow_contains "${TEST_W2}" ".channels.matrix.groupAllowFrom" "@${TEST_W1}:" 120 || true
wait_agent_matrix_allow_contains "${TEST_W1}" ".channels.matrix.groupAllowFrom" "@test-external-bot:" 120 || true
wait_agent_matrix_allow_contains "manager" ".channels.matrix.groupAllowFrom" "@${TEST_LEADER}:" 120 || true

# Leader: should have [Manager, Admin, W1, W2]
LEADER_GAF=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/${TEST_LEADER}/openclaw.json" 2>/dev/null | jq -r '.channels.matrix.groupAllowFrom[]' 2>/dev/null)
if echo "${LEADER_GAF}" | grep -q "@manager:"; then
    log_pass "Leader groupAllowFrom includes Manager"
else
    log_fail "Leader groupAllowFrom missing Manager"
fi

for w in "${TEST_W1}" "${TEST_W2}"; do
    if echo "${LEADER_GAF}" | grep -q "@${w}:"; then
        log_pass "Leader groupAllowFrom includes ${w}"
    else
        log_fail "Leader groupAllowFrom missing ${w}"
    fi
done

# Workers: should have [Leader, Admin] but NOT Manager
W1_GAF=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/${TEST_W1}/openclaw.json" 2>/dev/null | jq -r '.channels.matrix.groupAllowFrom[]' 2>/dev/null)
if echo "${W1_GAF}" | grep -q "@${TEST_LEADER}:"; then
    log_pass "Worker 1 groupAllowFrom includes Leader"
else
    log_fail "Worker 1 groupAllowFrom missing Leader"
fi

if echo "${W1_GAF}" | grep -q "@manager:"; then
    log_fail "Worker 1 groupAllowFrom includes Manager (should NOT)"
else
    log_pass "Worker 1 groupAllowFrom does NOT include Manager"
fi

# Peer mentions: Workers should have each other in groupAllowFrom (default peerMentions=true)
# EXCEPT: W1 has groupDenyExtra for W2, so W1 should NOT have W2
W2_GAF=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/${TEST_W2}/openclaw.json" 2>/dev/null | jq -r '.channels.matrix.groupAllowFrom[]' 2>/dev/null)

if echo "${W1_GAF}" | grep -q "@${TEST_W2}:"; then
    log_fail "Worker 1 groupAllowFrom includes Worker 2 (should be denied by channelPolicy)"
else
    log_pass "Worker 1 groupAllowFrom does NOT include Worker 2 (denied by channelPolicy)"
fi

if echo "${W2_GAF}" | grep -q "@${TEST_W1}:"; then
    log_pass "Worker 2 groupAllowFrom includes Worker 1 (peer mention)"
else
    log_fail "Worker 2 groupAllowFrom missing Worker 1 (peer mention should be enabled by default)"
fi

if echo "${W2_GAF}" | grep -q "@${TEST_LEADER}:"; then
    log_pass "Worker 2 groupAllowFrom includes Leader"
else
    log_fail "Worker 2 groupAllowFrom missing Leader"
fi

if echo "${W2_GAF}" | grep -q "@manager:"; then
    log_fail "Worker 2 groupAllowFrom includes Manager (should NOT)"
else
    log_pass "Worker 2 groupAllowFrom does NOT include Manager"
fi

# channelPolicy: team-level groupAllowExtra should add test-external-bot to all members
if echo "${LEADER_GAF}" | grep -q "@test-external-bot:"; then
    log_pass "Leader groupAllowFrom includes test-external-bot (team channelPolicy)"
else
    log_fail "Leader groupAllowFrom missing test-external-bot (team channelPolicy)"
fi

if echo "${W1_GAF}" | grep -q "@test-external-bot:"; then
    log_pass "Worker 1 groupAllowFrom includes test-external-bot (team channelPolicy)"
else
    log_fail "Worker 1 groupAllowFrom missing test-external-bot (team channelPolicy)"
fi

if echo "${W2_GAF}" | grep -q "@test-external-bot:"; then
    log_pass "Worker 2 groupAllowFrom includes test-external-bot (team channelPolicy)"
else
    log_fail "Worker 2 groupAllowFrom missing test-external-bot (team channelPolicy)"
fi

# Manager: should have Leader but NOT team workers
MGR_GAF=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/manager/openclaw.json" 2>/dev/null | jq -r '.channels.matrix.groupAllowFrom[]' 2>/dev/null)
if echo "${MGR_GAF}" | grep -q "@${TEST_LEADER}:"; then
    log_pass "Manager groupAllowFrom includes Leader"
else
    log_fail "Manager groupAllowFrom missing Leader"
fi

if echo "${MGR_GAF}" | grep -q "@${TEST_W1}:"; then
    log_fail "Manager groupAllowFrom includes team worker (should NOT)"
else
    log_pass "Manager groupAllowFrom does NOT include team workers"
fi

# ============================================================
# Section 8: Verify builtin skills per role
# ============================================================
log_section "Verify Skills by Role"

# Leader should have canonical Team Leader skills
for skill in team-coordination project-management task-management; do
    LEADER_SKILL=$(exec_in_manager bash -c "mc ls '${STORAGE_PREFIX}/agents/${TEST_LEADER}/skills/${skill}/SKILL.md' >/dev/null 2>&1 && echo yes || echo no")
    if [ "${LEADER_SKILL}" = "yes" ]; then
        log_pass "Leader has ${skill} skill"
    else
        log_fail "Leader missing ${skill} skill"
    fi
done

# Workers should have standard worker skills
for skill in file-sync task-progress mcporter; do
    W1_SKILL=$(exec_in_manager bash -c "mc ls '${STORAGE_PREFIX}/agents/${TEST_W1}/skills/${skill}/SKILL.md' >/dev/null 2>&1 && echo yes || echo no")
    if [ "${W1_SKILL}" = "yes" ]; then
        log_pass "Worker 1 has ${skill} skill"
    else
        log_fail "Worker 1 missing ${skill} skill"
    fi
done

# ============================================================
# Section 9: Verify agent count
# ============================================================
log_section "Verify Agent Count"

TEAM_AGENT_COUNT=$(echo "${WORKERS_REGISTRY}" | jq -r --arg t "${TEST_TEAM}" '[.workers | to_entries[] | select(.value.team_id == $t)] | length' 2>/dev/null)
assert_eq "3" "${TEAM_AGENT_COUNT}" "Team has 3 agents total (1 leader + 2 workers)"

# ============================================================
# Section 10: Verify admin auto-joined worker rooms
# ============================================================
log_section "Verify Admin Auto-Joined Worker Rooms"

if [ -n "${ADMIN_TOKEN}" ] && [ "${ADMIN_TOKEN}" != "null" ]; then
    ADMIN_MATRIX_ID="@${TEST_ADMIN_USER}:${TEST_MATRIX_DOMAIN}"
    for w in "${TEST_LEADER}" "${TEST_W1}" "${TEST_W2}"; do
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
# Section 11: Verify containers running
# ============================================================
log_section "Verify Containers"

for w in "${TEST_LEADER}" "${TEST_W1}" "${TEST_W2}"; do
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
test_teardown "18-team-config-verify"
test_summary
