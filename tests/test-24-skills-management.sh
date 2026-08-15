#!/bin/bash
# test-24-skills-management.sh - Case 24: Worker skills round-trip via CLI
#
# Verifies the `--skills` flag on `agt create worker` and
# `agt update worker` flows through the controller and is reflected in
# the source-of-truth Worker CR. For QwenPaw, it also verifies the public
# Manager skill-distribution command reaches the running Worker's skill API.
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
MANAGER_SKILL="test24-manager-skill"
STORAGE_PREFIX="${STORAGE_PREFIX:-${TEST_STORAGE_PREFIX:-agentteams/agentteams-storage}}"
TEST_WORKER_RUNTIME="${AGENTTEAMS_DEFAULT_WORKER_RUNTIME:-openclaw}"
if [ "${TEST_WORKER_RUNTIME}" = "copaw" ] || [ "${TEST_WORKER_RUNTIME}" = "qwenpaw" ]; then
    BASELINE_SKILL="file-sharing"
else
    BASELINE_SKILL="file-sync"
fi

_cleanup() {
    log_info "Cleaning up: ${TEST_WORKER}"
    exec_in_agent agt delete worker "${TEST_WORKER}" 2>/dev/null || true
    sleep 5
    remove_worker_container "${TEST_WORKER}"
    exec_in_agent rm -rf "/root/manager-workspace/worker-skills/${MANAGER_SKILL}" 2>/dev/null || true
    exec_in_agent rm -rf "/tmp/${MANAGER_SKILL}-package" "/tmp/${MANAGER_SKILL}.zip" 2>/dev/null || true
    exec_in_manager mc rm -r --force "${STORAGE_PREFIX}/agents/${TEST_WORKER}/" 2>/dev/null || true
    exec_in_manager mc rm "${STORAGE_PREFIX}/agentteams-config/workers/${TEST_WORKER}.yaml" 2>/dev/null || true
}
trap _cleanup EXIT

minio_setup

# Helper: read Worker CR skills through the Controller API.
_worker_skills_in_api() {
    local worker="$1"
    exec_in_agent agt get workers "${worker}" -o json 2>/dev/null | jq -c '.skills // []'
}

# ============================================================
# Section 1: Create worker with --skills github-operations
# ============================================================
log_section "Create Worker with --skills github-operations"

CREATE_OUTPUT=$(exec_in_agent agt create worker --name "${TEST_WORKER}" \
    --runtime "${TEST_WORKER_RUNTIME}" \
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
# Section 2: Worker CR reflects initial skills
# ============================================================
log_section "Verify Worker CR After Create"

# Give the controller a moment after provisioning to serve the resource.
DEADLINE=$(( $(date +%s) + 60 ))
INITIAL_SKILLS=""
while [ "$(date +%s)" -lt "${DEADLINE}" ]; do
    INITIAL_SKILLS=$(_worker_skills_in_api "${TEST_WORKER}")
    [ -n "${INITIAL_SKILLS}" ] && [ "${INITIAL_SKILLS}" != "null" ] && break
    sleep 5
done

log_info "Initial skills in Worker CR: ${INITIAL_SKILLS}"
if echo "${INITIAL_SKILLS}" | jq -e 'index("github-operations")' >/dev/null 2>&1; then
    log_pass "Worker CR contains 'github-operations' for ${TEST_WORKER}"
else
    log_fail "Worker CR missing 'github-operations' (got: ${INITIAL_SKILLS})"
fi

if [ "${TEST_WORKER_RUNTIME}" = "qwenpaw" ]; then
    if wait_qwenpaw_api_matches "${TEST_WORKER}" /api/skills ".[] | select(.name == \"${BASELINE_SKILL}\" and .source == \"plugin:teamharness\")" 240; then
        log_pass "QwenPaw plugin skills reconciled"
    else
        log_fail "QwenPaw plugin skills did not reconcile"
    fi
fi

# Built-in baseline skill should be present in the runtime that consumes it.
if [ "${TEST_WORKER_RUNTIME}" = "qwenpaw" ]; then
    QWENPAW_SKILLS=$(read_qwenpaw_skills "${TEST_WORKER}")
    if echo "${QWENPAW_SKILLS}" | jq -e --arg skill "${BASELINE_SKILL}" \
        '.[] | select(.name == $skill and .source == "plugin:teamharness")' >/dev/null 2>&1; then
        log_pass "Built-in plugin skill '${BASELINE_SKILL}' visible through QwenPaw API"
    else
        log_fail "Built-in plugin skill '${BASELINE_SKILL}' missing from QwenPaw API"
    fi
elif minio_file_exists "agents/${TEST_WORKER}/skills/${BASELINE_SKILL}/SKILL.md"; then
    log_pass "Built-in skill '${BASELINE_SKILL}' present in MinIO for ${TEST_WORKER_RUNTIME} runtime"
else
    log_fail "Built-in skill '${BASELINE_SKILL}' missing in MinIO for ${TEST_WORKER_RUNTIME} runtime"
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
# Section 3: Manager distributes a custom skill to QwenPaw
# ============================================================
if [ "${TEST_WORKER_RUNTIME}" = "qwenpaw" ]; then
    log_section "Manager Distributes a Custom Skill"

    MANAGER_SKILL_PACKAGE_DIR="/tmp/${MANAGER_SKILL}-package/${MANAGER_SKILL}"
    MANAGER_SKILL_ARCHIVE="/tmp/${MANAGER_SKILL}.zip"
    exec_in_agent mkdir -p "${MANAGER_SKILL_PACKAGE_DIR}"
    exec_in_agent sh -c "printf '%s\n' \
        '---' \
        'name: ${MANAGER_SKILL}' \
        'description: Verify Manager-to-Worker skill distribution.' \
        '---' \
        '# Manager-distributed test skill' > '${MANAGER_SKILL_PACKAGE_DIR}/SKILL.md'"
    exec_in_agent python3 -c \
        'import pathlib, sys, zipfile; root=pathlib.Path(sys.argv[1]); archive=zipfile.ZipFile(sys.argv[2], "w"); [archive.write(path, path.relative_to(root)) for path in root.rglob("*") if path.is_file()]; archive.close()' \
        "/tmp/${MANAGER_SKILL}-package" "${MANAGER_SKILL_ARCHIVE}"

    INSTALL_OUTPUT=$(exec_in_agent bash \
        /opt/agentteams/agent/skills/worker-management/scripts/install-worker-skill.sh \
        --worker "${TEST_WORKER}" --archive "${MANAGER_SKILL_ARCHIVE}" --no-notify 2>&1)
    INSTALL_EXIT=$?
    if [ "${INSTALL_EXIT}" -eq 0 ]; then
        log_pass "Manager safely installed and distributed a Skill without assign_when"
    else
        log_fail "Manager Skill installation command failed: ${INSTALL_OUTPUT}"
    fi

    DEADLINE=$(( $(date +%s) + 120 ))
    MANAGER_ASSIGNED_SKILLS=""
    while [ "$(date +%s)" -lt "${DEADLINE}" ]; do
        MANAGER_ASSIGNED_SKILLS=$(_worker_skills_in_api "${TEST_WORKER}")
        if echo "${MANAGER_ASSIGNED_SKILLS}" | jq -e --arg skill "${MANAGER_SKILL}" \
            'index($skill)' >/dev/null 2>&1; then
            break
        fi
        sleep 5
    done
    if echo "${MANAGER_ASSIGNED_SKILLS}" | jq -e --arg skill "${MANAGER_SKILL}" \
        'index($skill)' >/dev/null 2>&1; then
        log_pass "Manager updated Worker.spec.skills"
    else
        log_fail "Worker.spec.skills missing Manager-distributed skill (got: ${MANAGER_ASSIGNED_SKILLS})"
    fi

    if minio_file_exists "agents/${TEST_WORKER}/skills/${MANAGER_SKILL}/SKILL.md"; then
        log_pass "Manager uploaded the custom skill to Worker storage"
    else
        log_fail "Manager-distributed skill missing from Worker storage"
    fi

    if wait_qwenpaw_api_matches "${TEST_WORKER}" /api/skills \
        ".[] | select(.name == \"${MANAGER_SKILL}\" and .enabled == true)" 240; then
        log_pass "QwenPaw loaded and enabled the Manager-distributed skill"
    else
        log_fail "QwenPaw did not load and enable the Manager-distributed skill"
    fi
fi

# ============================================================
# Section 4: Update skills via `agt update worker --skills`
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

# Wait for the Controller API to reflect the update.
log_info "Waiting for Worker CR to reflect skill change..."
DEADLINE=$(( $(date +%s) + 120 ))
UPDATED_SKILLS=""
while [ "$(date +%s)" -lt "${DEADLINE}" ]; do
    UPDATED_SKILLS=$(_worker_skills_in_api "${TEST_WORKER}")
    if echo "${UPDATED_SKILLS}" | jq -e 'index("git-delegation")' >/dev/null 2>&1 \
        && ! echo "${UPDATED_SKILLS}" | jq -e 'index("github-operations")' >/dev/null 2>&1; then
        break
    fi
    sleep 5
done

log_info "Updated skills in Worker CR: ${UPDATED_SKILLS}"

# ============================================================
# Section 5: Verify post-update state
# ============================================================
log_section "Verify Worker CR After Update"

if echo "${UPDATED_SKILLS}" | jq -e 'index("git-delegation")' >/dev/null 2>&1; then
    log_pass "Worker CR contains 'git-delegation' after update"
else
    log_fail "Worker CR missing 'git-delegation' after update (got: ${UPDATED_SKILLS})"
fi

if echo "${UPDATED_SKILLS}" | jq -e 'index("github-operations")' >/dev/null 2>&1; then
    log_fail "Worker CR still contains 'github-operations' after replacement update"
else
    log_pass "Replaced skill 'github-operations' no longer in Worker CR"
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
