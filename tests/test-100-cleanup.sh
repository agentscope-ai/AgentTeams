#!/bin/bash
# test-100-cleanup.sh - Case 100: Clean up all test-created workers and teams
#
# This test runs LAST and verifies that the delete flow properly cleans up
# container resources (not just stops them). It:
#   1. Discovers all test-* workers and teams from registries
#   2. Deletes them via agt delete
#   3. Waits for controller reconcile (which now calls lifecycle-worker.sh --action delete)
#   4. Verifies containers are removed (not just stopped)
#   5. Verifies worker-lifecycle.json entries are cleaned up

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/test-helpers.sh"

test_setup "100-cleanup"

STORAGE_PREFIX="${STORAGE_PREFIX:-${TEST_STORAGE_PREFIX:-agentteams/agentteams-storage}}"

# ============================================================
# Section 1: Discover test workers and teams
# ============================================================
log_section "Discover Test Resources"

# Find all test-* workers in workers-registry.json (from MinIO)
WORKERS_REGISTRY=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/manager/workers-registry.json" 2>/dev/null || echo "{}")
TEST_WORKERS=$(echo "${WORKERS_REGISTRY}" | jq -r '.workers | keys[] | select(startswith("test-"))' 2>/dev/null || echo "")

# Find all test-* teams in teams-registry.json (from MinIO)
TEAMS_REGISTRY=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/manager/teams-registry.json" 2>/dev/null || echo "{}")
TEST_TEAMS=$(echo "${TEAMS_REGISTRY}" | jq -r '.teams | keys[] | select(startswith("test-"))' 2>/dev/null || echo "")

WORKER_COUNT=$(echo "${TEST_WORKERS}" | grep -c . 2>/dev/null || echo "0")
TEAM_COUNT=$(echo "${TEST_TEAMS}" | grep -c . 2>/dev/null || echo "0")

log_info "Found ${WORKER_COUNT} test worker(s) and ${TEAM_COUNT} test team(s) to clean up"

if [ "${WORKER_COUNT}" -eq 0 ] && [ "${TEAM_COUNT}" -eq 0 ]; then
    log_pass "No test resources to clean up"
    test_teardown "100-cleanup"
    test_summary
    exit $?
fi

# List what we found
for w in ${TEST_WORKERS}; do
    log_info "  Worker: ${w}"
done
for t in ${TEST_TEAMS}; do
    log_info "  Team: ${t}"
done

# ============================================================
# Section 2: Record pre-delete state
# ============================================================
log_section "Pre-Delete State"

# Snapshot which containers exist (running or stopped)
PRE_CONTAINERS=$(list_test_worker_containers)
PRE_CONTAINER_COUNT=$(echo "${PRE_CONTAINERS}" | grep -c . 2>/dev/null || echo "0")
log_info "${PRE_CONTAINER_COUNT} test worker container(s) present before cleanup"

# Snapshot lifecycle entries
PRE_LIFECYCLE_WORKERS=$(exec_in_agent jq -r '.workers | keys[] | select(startswith("test-"))' ~/worker-lifecycle.json 2>/dev/null || echo "")
PRE_LIFECYCLE_COUNT=$(echo "${PRE_LIFECYCLE_WORKERS}" | grep -c . 2>/dev/null || echo "0")
log_info "${PRE_LIFECYCLE_COUNT} test worker(s) in worker-lifecycle.json before cleanup"

# ============================================================
# Section 3: Delete teams first (teams contain workers)
# ============================================================
if [ -n "${TEST_TEAMS}" ]; then
    log_section "Delete Teams"

    for team in ${TEST_TEAMS}; do
        log_info "Deleting team: ${team}"
        DELETE_OUTPUT=$(exec_in_agent agt delete team "${team}" 2>&1)
        if echo "${DELETE_OUTPUT}" | grep -q "deleted"; then
            log_pass "agt delete team ${team} reported success"
        else
            log_info "agt delete team ${team} failed (YAML likely already removed by prior test cleanup)"
        fi
    done
fi

# ============================================================
# Section 4: Delete standalone workers (not part of a team)
# ============================================================
if [ -n "${TEST_WORKERS}" ]; then
    log_section "Delete Workers"

    # Collect team member names to skip (already handled by team delete)
    TEAM_MEMBERS=""
    for team in ${TEST_TEAMS}; do
        MEMBERS=$(echo "${TEAMS_REGISTRY}" | jq -r --arg t "${team}" \
            '(.teams[$t].leader // empty), (.teams[$t].workers[]? // empty)' 2>/dev/null || echo "")
        TEAM_MEMBERS="${TEAM_MEMBERS} ${MEMBERS}"
    done

    for worker in ${TEST_WORKERS}; do
        # Skip if this worker was part of a team (already deleted above)
        if echo "${TEAM_MEMBERS}" | grep -qw "${worker}"; then
            log_info "Skipping ${worker} (part of a deleted team)"
            continue
        fi

        log_info "Deleting worker: ${worker}"
        DELETE_OUTPUT=$(exec_in_agent agt delete worker "${worker}" 2>&1)
        if echo "${DELETE_OUTPUT}" | grep -q "deleted"; then
            log_pass "agt delete worker ${worker} reported success"
        else
            log_info "agt delete worker ${worker} skipped (YAML likely already removed by prior test)"
            remove_worker_container "${worker}"
            exec_in_agent bash /opt/agentteams/agent/skills/worker-management/scripts/lifecycle-worker.sh \
                --action delete --worker "${worker}" 2>/dev/null || true
        fi
    done
fi

# ============================================================
# Section 5: Wait for controller reconcile
# ============================================================
log_section "Wait for Controller Reconcile"

log_info "Waiting for controller to process all deletes..."
RECONCILE_TIMEOUT=120
RECONCILE_ELAPSED=0

# Wait until all test worker containers are gone (not just stopped — removed)
while [ "${RECONCILE_ELAPSED}" -lt "${RECONCILE_TIMEOUT}" ]; do
    REMAINING=$(list_test_worker_containers)
    if [ -z "${REMAINING}" ]; then
        break
    fi
    sleep 5
    RECONCILE_ELAPSED=$((RECONCILE_ELAPSED + 5))
    REMAINING_COUNT=$(echo "${REMAINING}" | grep -c . 2>/dev/null || echo "0")
    printf "\r[TEST INFO] Waiting for containers to be removed... (%d remaining, %ds/%ds)" "${REMAINING_COUNT}" "${RECONCILE_ELAPSED}" "${RECONCILE_TIMEOUT}"
done
echo ""

if [ "${RECONCILE_ELAPSED}" -lt "${RECONCILE_TIMEOUT}" ]; then
    log_pass "All test containers removed (took ~${RECONCILE_ELAPSED}s)"
else
    STILL_PRESENT=$(list_test_worker_containers)
    if [ -n "${STILL_PRESENT}" ]; then
        log_fail "Some test containers still present after ${RECONCILE_TIMEOUT}s:"
        echo "${STILL_PRESENT}" | while read -r c; do
            log_info "  ${c} (status: $(docker inspect --format '{{.State.Status}}' "${c}" 2>/dev/null || echo 'unknown'))"
        done
    fi
fi

# ============================================================
# Section 6: Verify containers are removed (not just stopped)
# ============================================================
log_section "Verify Container Removal"

POST_CONTAINERS=$(list_test_worker_containers)
if [ -z "${POST_CONTAINERS}" ]; then
    log_pass "No test worker containers remain (all removed, not just stopped)"
else
    for c in ${POST_CONTAINERS}; do
        STATUS=$(docker inspect --format '{{.State.Status}}' "${c}" 2>/dev/null || echo "unknown")
        if [ "${STATUS}" = "exited" ]; then
            log_fail "Container ${c} is stopped but NOT removed (status: ${STATUS})"
        else
            log_fail "Container ${c} still exists (status: ${STATUS})"
        fi
    done
fi

# ============================================================
# Section 7: Verify worker-lifecycle.json cleanup
# ============================================================
log_section "Verify Lifecycle State Cleanup"

POST_LIFECYCLE_WORKERS=$(exec_in_agent jq -r '.workers | keys[] | select(startswith("test-"))' ~/worker-lifecycle.json 2>/dev/null || echo "")
if [ -z "${POST_LIFECYCLE_WORKERS}" ]; then
    log_pass "No test workers remain in worker-lifecycle.json"
else
    for w in ${POST_LIFECYCLE_WORKERS}; do
        log_fail "Worker ${w} still in worker-lifecycle.json"
    done
fi

# ============================================================
# Section 8: Verify YAML removed from MinIO
# ============================================================
log_section "Verify MinIO Cleanup"

for w in ${TEST_WORKERS}; do
    YAML_EXISTS=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agentteams-config/workers/${w}.yaml" 2>/dev/null || echo "")
    if [ -z "${YAML_EXISTS}" ]; then
        log_pass "YAML removed from MinIO: ${w}"
    else
        log_fail "YAML still in MinIO: ${w}"
    fi
done

for t in ${TEST_TEAMS}; do
    YAML_EXISTS=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agentteams-config/teams/${t}.yaml" 2>/dev/null || echo "")
    if [ -z "${YAML_EXISTS}" ]; then
        log_pass "YAML removed from MinIO: ${t}"
    else
        log_fail "YAML still in MinIO: ${t}"
    fi
done

# ============================================================
# Section 9: Verify registries cleaned
# ============================================================
log_section "Verify Registry Cleanup"

POST_WORKERS_REGISTRY=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/manager/workers-registry.json" 2>/dev/null || echo "{}")
for w in ${TEST_WORKERS}; do
    REG_ENTRY=$(echo "${POST_WORKERS_REGISTRY}" | jq -r --arg w "${w}" '.workers[$w] // empty' 2>/dev/null)
    if [ -z "${REG_ENTRY}" ]; then
        log_pass "Worker removed from workers-registry.json: ${w}"
    else
        log_info "Worker still in workers-registry.json: ${w} (expected — registry cleanup is separate)"
    fi
done

POST_TEAMS_REGISTRY=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/manager/teams-registry.json" 2>/dev/null || echo "{}")
for t in ${TEST_TEAMS}; do
    REG_ENTRY=$(echo "${POST_TEAMS_REGISTRY}" | jq -r --arg t "${t}" '.teams[$t] // empty' 2>/dev/null)
    if [ -z "${REG_ENTRY}" ]; then
        log_pass "Team removed from teams-registry.json: ${t}"
    else
        log_info "Team still in teams-registry.json: ${t} (expected — registry cleanup is separate)"
    fi
done

# ============================================================
# Summary
# ============================================================
test_teardown "100-cleanup"
test_summary
