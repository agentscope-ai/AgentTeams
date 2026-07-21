#!/bin/bash
# test-22-delete-worker-cleanup.sh - Case 22: Delete worker releases resources
#
# Verifies that `agt delete worker <name>` actually releases the worker's
# infrastructure side-effects, not just the CR. test-100 covers bulk cleanup
# of N test workers and checks containers + lifecycle.json + YAML, but it
# does NOT assert that the Higress consumer or the MinIO agents/<name>/
# directory are removed, and it tolerates pre-existing residue (a clean
# slate run passes with zero workers).
#
# This test is deterministic: it creates one fresh worker, snapshots the
# expected post-create state, deletes it, and asserts each resource is
# released individually. It then re-creates the same name to confirm the
# delete did not leave anything behind that would block reuse.
#
# This is a controller-cr style test — no LLM required.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/test-helpers.sh"
source "${SCRIPT_DIR}/lib/minio-client.sh"
source "${SCRIPT_DIR}/lib/higress-client.sh"

test_setup "22-delete-worker-cleanup"

TEST_WORKER="test-del-$$"
STORAGE_PREFIX="${STORAGE_PREFIX:-${TEST_STORAGE_PREFIX:-agentteams/agentteams-storage}}"

_cleanup() {
    log_info "Cleaning up: ${TEST_WORKER}"
    exec_in_agent agt delete worker "${TEST_WORKER}" 2>/dev/null || true
    sleep 5
    remove_worker_container "${TEST_WORKER}"
    exec_in_manager mc rm -r --force "${STORAGE_PREFIX}/agents/${TEST_WORKER}/" 2>/dev/null || true
    exec_in_manager mc rm "${STORAGE_PREFIX}/agentteams-config/workers/${TEST_WORKER}.yaml" 2>/dev/null || true
}
trap _cleanup EXIT

minio_setup

_get_higress_consumers_or_fail() {
    local label="$1"
    local consumers

    if ! higress_login "${TEST_ADMIN_USER}" "${TEST_ADMIN_PASSWORD}" > /dev/null 2>&1; then
        log_fail "Unable to log in to Higress before ${label}"
        return 1
    fi

    if ! consumers=$(higress_get_consumers 2>/dev/null); then
        log_fail "Unable to query Higress consumers during ${label}"
        return 1
    fi

    if ! echo "${consumers}" | jq -e '.data | type == "array"' >/dev/null 2>&1; then
        log_fail "Higress consumers response during ${label} is not valid JSON with a data array"
        return 1
    fi

    HIGRESS_CONSUMERS_JSON="${consumers}"
}

_worker_container_exists() {
    docker ps -a --format '{{.Names}}' 2>/dev/null | grep -q "^$(worker_container_name "${TEST_WORKER}")$"
}

_higress_consumer_exists() {
    echo "${HIGRESS_CONSUMERS_JSON}" | jq -r '.data[]?.name // empty' 2>/dev/null \
        | grep -Fxq "worker-${TEST_WORKER}"
}

_minio_agent_dir_listing() {
    minio_list_dir "agents/${TEST_WORKER}/" 2>/dev/null || true
}

_minio_worker_yaml() {
    exec_in_manager mc cat "${STORAGE_PREFIX}/agentteams-config/workers/${TEST_WORKER}.yaml" 2>/dev/null || true
}

# ============================================================
# Section 1: Create the worker
# ============================================================
log_section "Create Worker ${TEST_WORKER}"

CREATE_OUTPUT=$(exec_in_agent agt create worker --name "${TEST_WORKER}" --no-wait 2>&1)
CREATE_EXIT=$?
if [ "${CREATE_EXIT}" -eq 0 ]; then
    log_pass "agt create worker accepted"
else
    log_fail "agt create worker failed: ${CREATE_OUTPUT}"
    test_teardown "22-delete-worker-cleanup"
    test_summary
    exit 1
fi

if wait_worker_provisioned "${TEST_WORKER}" 180; then
    log_pass "Worker provisioned (roomID + matrixUserID populated)"
else
    log_fail "Worker did not reach provisioned state in 180s"
    test_teardown "22-delete-worker-cleanup"
    test_summary
    exit 1
fi

if wait_for_worker_container "${TEST_WORKER}" 120; then
    log_pass "Worker container started"
else
    log_fail "Worker container did not start in 120s"
fi

# ============================================================
# Section 2: Snapshot pre-delete state
# ============================================================
log_section "Snapshot Pre-Delete State"

HIGRESS_CONSUMERS_JSON=""
if _get_higress_consumers_or_fail "pre-delete snapshot"; then
    CONSUMERS_BEFORE="${HIGRESS_CONSUMERS_JSON}"
    if _higress_consumer_exists; then
        log_pass "Higress consumer 'worker-${TEST_WORKER}' exists before delete"
    else
        log_fail "Higress consumer 'worker-${TEST_WORKER}' missing before delete (cannot test cleanup)"
    fi
fi

if minio_file_exists "agents/${TEST_WORKER}/SOUL.md"; then
    log_pass "MinIO SOUL.md exists before delete"
else
    log_fail "MinIO SOUL.md missing before delete (cannot test cleanup)"
fi

PRE_YAML=$(_minio_worker_yaml)
PRE_YAML_EXISTS=0
if [ -n "${PRE_YAML}" ]; then
    PRE_YAML_EXISTS=1
    log_pass "MinIO YAML exists before delete"
else
    log_info "MinIO YAML absent before delete (expected for REST-created workers)"
fi

# ============================================================
# Section 3: Delete the worker
# ============================================================
log_section "Delete Worker"

DELETE_OUTPUT=$(exec_in_agent agt delete worker "${TEST_WORKER}" 2>&1)
DELETE_EXIT=$?
if [ "${DELETE_EXIT}" -eq 0 ] && echo "${DELETE_OUTPUT}" | grep -qi "deleted"; then
    log_pass "agt delete reports success: ${DELETE_OUTPUT}"
else
    log_fail "agt delete failed (exit=${DELETE_EXIT}): ${DELETE_OUTPUT}"
fi

# Wait for controller to release resources
log_info "Waiting for controller to release resources..."
DEADLINE=$(( $(date +%s) + 120 ))
while [ "$(date +%s)" -lt "${DEADLINE}" ]; do
    HIGRESS_CONSUMERS_JSON=""
    HIGRESS_QUERY_OK=0
    if _get_higress_consumers_or_fail "post-delete wait"; then
        HIGRESS_QUERY_OK=1
    fi
    AGENT_DIR_LISTING=$(_minio_agent_dir_listing)
    POST_YAML=$(_minio_worker_yaml)

    if ! _worker_container_exists \
        && [ "${HIGRESS_QUERY_OK}" -eq 1 ] \
        && ! _higress_consumer_exists \
        && [ -z "${AGENT_DIR_LISTING}" ] \
        && { [ "${PRE_YAML_EXISTS}" -eq 0 ] || [ -z "${POST_YAML}" ]; }; then
        break
    fi
    sleep 5
done

# ============================================================
# Section 4: Assert each resource was released
# ============================================================
log_section "Verify Cleanup"

# (a) container removed (not just stopped)
if _worker_container_exists; then
    STATUS=$(docker inspect --format '{{.State.Status}}' "$(worker_container_name "${TEST_WORKER}")" 2>/dev/null || echo unknown)
    log_fail "Worker container still present (status: ${STATUS})"
else
    log_pass "Worker container removed"
fi

# (b) Higress consumer removed
HIGRESS_CONSUMERS_JSON=""
if _get_higress_consumers_or_fail "post-delete assertion"; then
    CONSUMERS_AFTER="${HIGRESS_CONSUMERS_JSON}"
    if _higress_consumer_exists; then
        log_fail "Higress consumer 'worker-${TEST_WORKER}' still present after delete"
    else
        log_pass "Higress consumer removed"
    fi
fi

# (c) MinIO agents/<name>/ removed (or empty)
AGENT_DIR_LISTING=$(_minio_agent_dir_listing)
if [ -z "${AGENT_DIR_LISTING}" ]; then
    log_pass "MinIO agents/${TEST_WORKER}/ removed"
else
    log_fail "MinIO agents/${TEST_WORKER}/ still contains files: $(echo "${AGENT_DIR_LISTING}" | head -3)"
fi

# (d) MinIO YAML removed if this worker was created from a declarative YAML
POST_YAML=$(_minio_worker_yaml)
if [ "${PRE_YAML_EXISTS}" -eq 0 ]; then
    log_info "MinIO YAML removal check skipped (no pre-delete YAML for REST-created worker)"
elif [ -z "${POST_YAML}" ]; then
    log_pass "MinIO YAML removed"
else
    log_fail "MinIO YAML still present"
fi

# (e) workers-registry.json entry removed
REGISTRY=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/manager/workers-registry.json" 2>/dev/null || echo "{}")
if echo "${REGISTRY}" | jq -e --arg w "${TEST_WORKER}" '.workers[$w] // empty' >/dev/null 2>&1; then
    log_info "Worker still in workers-registry.json (registry cleanup is best-effort, see test-100 note)"
else
    log_pass "Worker removed from workers-registry.json"
fi

# ============================================================
# Section 5: Recreate same name — must not be blocked by stale state
# ============================================================
log_section "Reuse Name After Delete"

RECREATE_OUTPUT=$(exec_in_agent agt create worker --name "${TEST_WORKER}" --no-wait 2>&1)
RECREATE_EXIT=$?
if [ "${RECREATE_EXIT}" -eq 0 ]; then
    log_pass "Recreate with same name accepted"
else
    log_fail "Recreate with same name failed: ${RECREATE_OUTPUT}"
fi

if wait_worker_provisioned "${TEST_WORKER}" 180; then
    log_pass "Recreated worker provisioned"
else
    log_fail "Recreated worker did not reach provisioned state"
fi

# Trap will clean up the recreated worker.

# ============================================================
# Summary
# ============================================================
test_teardown "22-delete-worker-cleanup"
test_summary
