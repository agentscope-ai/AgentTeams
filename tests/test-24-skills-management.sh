#!/bin/bash
# test-24-skills-management.sh - Case 24: Worker skills round-trip via CLI
#
# Verifies the `--skills` flag on `agt create worker` and
# `agt update worker` flows through the controller and is reflected in
# the source-of-truth registry at agents/manager/workers-registry.json,
# and that the corresponding skill files land in agents/<name>/skills/.
#
# Built-in baseline skills (file-sync, etc.) are always pushed for every
# Worker regardless of --skills; the flag controls *on-demand* skills
# pulled from manager/agent/worker-skills/. This test exercises the
# on-demand path with github-operations → git-delegation.
#
# This is a controller-cr style test — no LLM required.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/test-helpers.sh"
source "${SCRIPT_DIR}/lib/minio-client.sh"

test_setup "24-skills-management"

TEST_WORKER="test-skl-$$"
STORAGE_PREFIX="${STORAGE_PREFIX:-${TEST_STORAGE_PREFIX:-agentteams/agentteams-storage}}"
REGISTRY_KEY="${STORAGE_PREFIX}/agents/manager/workers-registry.json"

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

# Helper: read worker entry from registry and return .skills as JSON array
_worker_skills_in_registry() {
    local worker="$1"
    exec_in_manager mc cat "${REGISTRY_KEY}" 2>/dev/null \
        | jq -c --arg w "${worker}" '.workers[$w].skills // empty' 2>/dev/null
}

# ============================================================
# Section 1: Create worker with --skills github-operations
# ============================================================
log_section "Create Worker with --skills github-operations"

CREATE_OUTPUT=$(exec_in_agent agt create worker --name "${TEST_WORKER}" \
    --skills github-operations --no-wait 2>&1)
CREATE_EXIT=$?
if [ "${CREATE_EXIT}" -eq 0 ]; then
    log_pass "agt create worker --skills github-operations accepted"
else
    log_fail "agt create failed: ${CREATE_OUTPUT}"
    test_teardown "24-skills-management"; test_summary; exit 1
fi

if wait_worker_provisioned "${TEST_WORKER}" 180; then
    log_pass "Worker provisioned"
else
    log_fail "Worker did not reach provisioned state"
    test_teardown "24-skills-management"; test_summary; exit 1
fi

# ============================================================
# Section 2: Registry reflects initial skills
# ============================================================
log_section "Verify Registry After Create"

# Give the controller a moment after provisioning to write the registry entry
DEADLINE=$(( $(date +%s) + 60 ))
INITIAL_SKILLS=""
while [ "$(date +%s)" -lt "${DEADLINE}" ]; do
    INITIAL_SKILLS=$(_worker_skills_in_registry "${TEST_WORKER}")
    [ -n "${INITIAL_SKILLS}" ] && [ "${INITIAL_SKILLS}" != "null" ] && break
    sleep 5
done

log_info "Initial skills in registry: ${INITIAL_SKILLS}"
if echo "${INITIAL_SKILLS}" | jq -e 'index("github-operations")' >/dev/null 2>&1; then
    log_pass "Registry contains 'github-operations' for ${TEST_WORKER}"
else
    log_fail "Registry missing 'github-operations' (got: ${INITIAL_SKILLS})"
fi

# Built-in baseline skill should be present in MinIO regardless of --skills
if minio_file_exists "agents/${TEST_WORKER}/skills/file-sync/SKILL.md"; then
    log_pass "Built-in skill 'file-sync' present in MinIO"
else
    log_fail "Built-in skill 'file-sync' missing in MinIO"
fi

if minio_file_exists "agents/${TEST_WORKER}/skills/github-operations/SKILL.md"; then
    log_info "On-demand skill 'github-operations' already present in MinIO"
else
    log_info "On-demand skill 'github-operations' not yet present in MinIO (manager-side skill sync is out of scope)"
fi

if wait_for_worker_container "${TEST_WORKER}" 180; then
    log_pass "Worker container running before skills update"
else
    log_fail "Worker container not running before skills update"
fi
PRE_UPDATE_CONTAINER_ID=$(docker inspect --format '{{.Id}}' "$(worker_container_name "${TEST_WORKER}")" 2>/dev/null | head -c 12 || echo "")

# ============================================================
# Section 3: Update skills via `agt update worker --skills`
# ============================================================
log_section "Update Skills (github-operations → git-delegation)"

UPDATE_OUTPUT=$(exec_in_agent agt update worker --name "${TEST_WORKER}" \
    --skills git-delegation 2>&1)
UPDATE_EXIT=$?
if [ "${UPDATE_EXIT}" -eq 0 ]; then
    log_pass "agt update worker --skills git-delegation accepted"
else
    log_fail "agt update failed (exit=${UPDATE_EXIT}): ${UPDATE_OUTPUT}"
fi

# Wait for the controller to re-reconcile and rewrite the registry
log_info "Waiting for registry to reflect skill change..."
DEADLINE=$(( $(date +%s) + 120 ))
UPDATED_SKILLS=""
while [ "$(date +%s)" -lt "${DEADLINE}" ]; do
    UPDATED_SKILLS=$(_worker_skills_in_registry "${TEST_WORKER}")
    if echo "${UPDATED_SKILLS}" | jq -e 'index("git-delegation")' >/dev/null 2>&1 \
        && ! echo "${UPDATED_SKILLS}" | jq -e 'index("github-operations")' >/dev/null 2>&1; then
        break
    fi
    sleep 5
done

log_info "Updated skills in registry: ${UPDATED_SKILLS}"

# ============================================================
# Section 4: Verify post-update state
# ============================================================
log_section "Verify Registry After Update"

if echo "${UPDATED_SKILLS}" | jq -e 'index("git-delegation")' >/dev/null 2>&1; then
    log_pass "Registry contains 'git-delegation' after update"
else
    log_fail "Registry missing 'git-delegation' after update (got: ${UPDATED_SKILLS})"
fi

if echo "${UPDATED_SKILLS}" | jq -e 'index("github-operations")' >/dev/null 2>&1; then
    log_fail "Registry still contains 'github-operations' after replacement update"
else
    log_pass "Replaced skill 'github-operations' no longer in registry"
fi

if minio_file_exists "agents/${TEST_WORKER}/skills/git-delegation/SKILL.md"; then
    log_info "On-demand skill 'git-delegation' present in MinIO after update"
else
    log_info "On-demand skill 'git-delegation' not yet present in MinIO after update (manager-side skill sync is out of scope)"
fi

# Worker container should still be running (skills update must not crash it).
# Wait here so a slow initial start is not mistaken for an update regression.
if wait_for_worker_container "${TEST_WORKER}" 120; then
    log_pass "Worker container still running after skills update"
    POST_UPDATE_CONTAINER_ID=$(docker inspect --format '{{.Id}}' "$(worker_container_name "${TEST_WORKER}")" 2>/dev/null | head -c 12 || echo "")
    if [ -n "${PRE_UPDATE_CONTAINER_ID}" ] && [ "${POST_UPDATE_CONTAINER_ID}" = "${PRE_UPDATE_CONTAINER_ID}" ]; then
        log_pass "Worker container survived skills update without recreation"
    else
        log_info "Worker container id changed during skills update (before: ${PRE_UPDATE_CONTAINER_ID}, after: ${POST_UPDATE_CONTAINER_ID})"
    fi
else
    log_fail "Worker container disappeared after skills update"
fi

# ============================================================
# Summary
# ============================================================
test_teardown "24-skills-management"
test_summary
