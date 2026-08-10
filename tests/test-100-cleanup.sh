#!/bin/bash
# test-100-cleanup.sh - Case 100: Clean up all test-created workers and teams
#
# This test runs LAST and verifies that the delete flow properly cleans up
# container resources (not just stops them). It:
#   1. Discovers all test-* Worker and Team resources from the Controller API
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

# Find all test-* Worker resources.
WORKERS_RESOURCE=$(exec_in_agent agt get workers -o json 2>/dev/null || echo '{"workers":[]}')
TEST_WORKERS=$(echo "${WORKERS_RESOURCE}" | jq -r '.workers[].name | select(startswith("test-"))' 2>/dev/null || echo "")

# Find all test-* Team resources.
TEAMS_RESOURCE=$(exec_in_agent agt get teams -o json 2>/dev/null || echo '{"teams":[]}')
TEST_TEAMS=$(echo "${TEAMS_RESOURCE}" | jq -r '.teams[].name | select(startswith("test-"))' 2>/dev/null || echo "")

WORKER_COUNT=$(echo "${TEST_WORKERS}" | awk 'NF { count++ } END { print count + 0 }')
TEAM_COUNT=$(echo "${TEST_TEAMS}" | awk 'NF { count++ } END { print count + 0 }')

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
PRE_CONTAINER_COUNT=$(echo "${PRE_CONTAINERS}" | awk 'NF { count++ } END { print count + 0 }')
log_info "${PRE_CONTAINER_COUNT} test worker container(s) present before cleanup"

# Snapshot lifecycle entries
PRE_LIFECYCLE_WORKERS=$(exec_in_agent jq -r '.workers | keys[] | select(startswith("test-"))' ~/worker-lifecycle.json 2>/dev/null || echo "")
PRE_LIFECYCLE_COUNT=$(echo "${PRE_LIFECYCLE_WORKERS}" | awk 'NF { count++ } END { print count + 0 }')
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
# Section 4: Delete workers after their Team references are gone
# ============================================================
if [ -n "${TEST_WORKERS}" ]; then
    log_section "Delete Workers"

    for worker in ${TEST_WORKERS}; do
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
    REMAINING_COUNT=$(echo "${REMAINING}" | awk 'NF { count++ } END { print count + 0 }')
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
# Section 9: Verify resources deleted
# ============================================================
log_section "Verify Resource Cleanup"

for w in ${TEST_WORKERS}; do
    if exec_in_agent agt get workers "${w}" -o json >/dev/null 2>&1; then
        log_fail "Worker resource still exists: ${w}"
    else
        log_pass "Worker resource deleted: ${w}"
    fi
done

for t in ${TEST_TEAMS}; do
    if exec_in_agent agt get teams "${t}" -o json >/dev/null 2>&1; then
        log_fail "Team resource still exists: ${t}"
    else
        log_pass "Team resource deleted: ${t}"
    fi
done

# ============================================================
# Summary
# ============================================================
test_teardown "100-cleanup"
test_summary
