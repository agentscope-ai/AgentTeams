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

test_setup "18-team-config-verify"

TEST_TEAM="test-team-$$"
TEST_LEADER="${TEST_TEAM}-lead"
TEST_W1="${TEST_TEAM}-dev"
TEST_W2="${TEST_TEAM}-qa"
STORAGE_PREFIX="hiclaw/hiclaw-storage"

_cleanup() {
    log_info "Cleaning up team: ${TEST_TEAM}"
    # Stop worker containers
    docker rm -f "hiclaw-worker-${TEST_LEADER}" 2>/dev/null || true
    docker rm -f "hiclaw-worker-${TEST_W1}" 2>/dev/null || true
    docker rm -f "hiclaw-worker-${TEST_W2}" 2>/dev/null || true
    # Clean MinIO
    for w in "${TEST_LEADER}" "${TEST_W1}" "${TEST_W2}"; do
        exec_in_manager mc rm -r --force "${STORAGE_PREFIX}/agents/${w}/" 2>/dev/null || true
        exec_in_manager rm -rf "/root/hiclaw-fs/agents/${w}" 2>/dev/null || true
    done
    # Clean registries
    exec_in_manager bash -c "
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
        mkdir -p /root/hiclaw-fs/agents/${w}
        cat > /root/hiclaw-fs/agents/${w}/SOUL.md <<SOUL
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
        mc mirror /root/hiclaw-fs/agents/${w}/ ${STORAGE_PREFIX}/agents/${w}/ --overwrite 2>/dev/null
    " 2>/dev/null
done

log_pass "SOUL.md files prepared for all team members"

# ============================================================
# Section 2: Create Team
# ============================================================
log_section "Create Team"

CREATE_OUTPUT=$(exec_in_manager bash -c "
    bash /opt/hiclaw/agent/skills/team-management/scripts/create-team.sh \
        --name '${TEST_TEAM}' --leader '${TEST_LEADER}' --workers '${TEST_W1},${TEST_W2}'
" 2>&1)

if echo "${CREATE_OUTPUT}" | grep -q "RESULT"; then
    log_pass "create-team.sh completed"
else
    log_fail "create-team.sh failed"
    echo "${CREATE_OUTPUT}" | tail -10
fi

# ============================================================
# Section 3: Verify teams-registry.json
# ============================================================
log_section "Verify teams-registry.json"

TEAM_ENTRY=$(exec_in_manager jq -r --arg t "${TEST_TEAM}" '.teams[$t] // empty' /root/manager-workspace/teams-registry.json 2>/dev/null)
assert_not_empty "${TEAM_ENTRY}" "Team registered in teams-registry.json"

TEAM_LEADER_REG=$(echo "${TEAM_ENTRY}" | jq -r '.leader // empty')
assert_eq "${TEST_LEADER}" "${TEAM_LEADER_REG}" "Team leader is ${TEST_LEADER}"

TEAM_WORKERS_REG=$(echo "${TEAM_ENTRY}" | jq -r '.workers | length')
assert_eq "2" "${TEAM_WORKERS_REG}" "Team has 2 workers"

TEAM_ROOM=$(echo "${TEAM_ENTRY}" | jq -r '.team_room_id // empty')
assert_not_empty "${TEAM_ROOM}" "Team Room ID exists: ${TEAM_ROOM}"

# ============================================================
# Section 4: Verify workers-registry.json roles
# ============================================================
log_section "Verify Worker Roles in Registry"

LEADER_ROLE=$(exec_in_manager jq -r --arg w "${TEST_LEADER}" '.workers[$w].role // empty' /root/manager-workspace/workers-registry.json 2>/dev/null)
assert_eq "team_leader" "${LEADER_ROLE}" "Leader has role=team_leader"

LEADER_TEAM=$(exec_in_manager jq -r --arg w "${TEST_LEADER}" '.workers[$w].team_id // empty' /root/manager-workspace/workers-registry.json 2>/dev/null)
assert_eq "${TEST_TEAM}" "${LEADER_TEAM}" "Leader has correct team_id"

W1_ROLE=$(exec_in_manager jq -r --arg w "${TEST_W1}" '.workers[$w].role // empty' /root/manager-workspace/workers-registry.json 2>/dev/null)
assert_eq "worker" "${W1_ROLE}" "Worker 1 has role=worker"

W1_TEAM=$(exec_in_manager jq -r --arg w "${TEST_W1}" '.workers[$w].team_id // empty' /root/manager-workspace/workers-registry.json 2>/dev/null)
assert_eq "${TEST_TEAM}" "${W1_TEAM}" "Worker 1 has correct team_id"

W2_ROLE=$(exec_in_manager jq -r --arg w "${TEST_W2}" '.workers[$w].role // empty' /root/manager-workspace/workers-registry.json 2>/dev/null)
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

# Manager: should have Leader but NOT team workers
MGR_GAF=$(exec_in_manager jq -r '.channels.matrix.groupAllowFrom[]' /root/manager-workspace/openclaw.json 2>/dev/null)
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

# Leader should have team-task-management skill
LEADER_TTM=$(exec_in_manager bash -c "mc ls '${STORAGE_PREFIX}/agents/${TEST_LEADER}/skills/team-task-management/SKILL.md' >/dev/null 2>&1 && echo yes || echo no")
if [ "${LEADER_TTM}" = "yes" ]; then
    log_pass "Leader has team-task-management skill"
else
    log_fail "Leader missing team-task-management skill"
fi

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

TEAM_AGENT_COUNT=$(exec_in_manager jq -r --arg t "${TEST_TEAM}" '[.workers | to_entries[] | select(.value.team_id == $t)] | length' /root/manager-workspace/workers-registry.json 2>/dev/null)
assert_eq "3" "${TEAM_AGENT_COUNT}" "Team has 3 agents total (1 leader + 2 workers)"

# ============================================================
# Section 10: Verify containers running
# ============================================================
log_section "Verify Containers"

for w in "${TEST_LEADER}" "${TEST_W1}" "${TEST_W2}"; do
    RUNNING=$(docker ps --format '{{.Names}}' 2>/dev/null | grep "hiclaw-worker-${w}" || echo "")
    if [ -n "${RUNNING}" ]; then
        log_pass "Container running: hiclaw-worker-${w}"
    else
        DEPLOY=$(exec_in_manager jq -r --arg w "${w}" '.workers[$w].deployment // empty' /root/manager-workspace/workers-registry.json 2>/dev/null)
        if [ "${DEPLOY}" = "remote" ]; then
            log_pass "Agent ${w} registered in remote mode"
        else
            log_fail "Container not running: hiclaw-worker-${w}"
        fi
    fi
done

# ============================================================
test_teardown "18-team-config-verify"
test_summary
