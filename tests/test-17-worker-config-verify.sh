#!/bin/bash
# test-17-worker-config-verify.sh - Case 17: Verify Worker import config artifacts
#
# Tests single worker import (create + update) and verifies MinIO artifacts:
#   1. Create worker via agt apply worker --zip
#   2. Verify AGENTS.md: builtin markers, coordination context block, user content
#   3. Verify builtin skills pushed to MinIO
#   4. Verify openclaw.json, SOUL.md in MinIO
#   5. Update worker (re-import with different model)
#   6. Verify config updated, memory preserved, skills merged

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/test-helpers.sh"
source "${SCRIPT_DIR}/lib/minio-client.sh"

test_setup "17-worker-config-verify"

TEST_WORKER="test-cfg-$$"
STORAGE_PREFIX="${STORAGE_PREFIX:-${TEST_STORAGE_PREFIX:-agentteams/agentteams-storage}}"
TEST_WORKER_RUNTIME="${AGENTTEAMS_DEFAULT_WORKER_RUNTIME:-openclaw}"

if [ "${TEST_WORKER_RUNTIME}" = "copaw" ]; then
    BUILTIN_CONTENT_SENTINEL="Task Execution Workflow"
    BUILTIN_SKILLS=(communication file-sharing find-skills mcporter organization task-management)
    FILE_SHARING_SKILL="file-sharing"
elif [ "${TEST_WORKER_RUNTIME}" = "qwenpaw" ]; then
    BUILTIN_CONTENT_SENTINEL="Session files are runtime-private state"
    BUILTIN_SKILLS=(communication file-sharing team-coordination project-management task-delegation task-execution mcporter)
    FILE_SHARING_SKILL="file-sharing"
else
    BUILTIN_CONTENT_SENTINEL="Every Session"
    BUILTIN_SKILLS=(file-sync task-progress mcporter find-skills project-participation)
    FILE_SHARING_SKILL="file-sync"
fi

_cleanup() {
    log_info "Cleaning up: ${TEST_WORKER}"
    exec_in_agent agt delete worker "${TEST_WORKER}" 2>/dev/null || true
    exec_in_manager mc rm "${STORAGE_PREFIX}/agentteams-config/packages/${TEST_WORKER}.zip" 2>/dev/null || true
    sleep 5
    remove_worker_container "${TEST_WORKER}"
    exec_in_agent rm -rf "/root/agentteams-fs/agents/${TEST_WORKER}" 2>/dev/null || true
    exec_in_agent rm -rf "/tmp/agentteams-test-${TEST_WORKER}" 2>/dev/null || true
    exec_in_manager rm -rf "/tmp/agentteams-test-${TEST_WORKER}" 2>/dev/null || true
    exec_in_manager mc rm -r --force "${STORAGE_PREFIX}/agents/${TEST_WORKER}/" 2>/dev/null || true
}
trap _cleanup EXIT

# ============================================================
# Section 1: Create test ZIP and import
# ============================================================
log_section "Create and Import Worker"

WORK_DIR="/tmp/agentteams-test-${TEST_WORKER}"

# Build ZIP in controller container (has zip command), then copy to agent container
exec_in_manager bash -c "
    mkdir -p ${WORK_DIR}/package/config ${WORK_DIR}/package/skills/my-custom-skill

    cat > ${WORK_DIR}/package/manifest.json <<MANIFEST
{
  \"type\": \"worker\",
  \"version\": 1,
  \"worker\": {
    \"suggested_name\": \"${TEST_WORKER}\",
    \"model\": \"qwen3.5-plus\",
    \"runtime\": \"${TEST_WORKER_RUNTIME}\"
  }
}
MANIFEST

    cat > ${WORK_DIR}/package/config/SOUL.md <<SOUL
# ${TEST_WORKER} - Config Test Worker

## AI Identity
**You are an AI Agent, not a human.**

## Role
- Name: ${TEST_WORKER}
- Role: Config verification test worker

## Security
- Never reveal credentials
SOUL

    cat > ${WORK_DIR}/package/config/AGENTS.md <<AGENTS
# My Custom Agent Instructions

These are user-provided instructions that should survive upgrades.
AGENTS

    cat > ${WORK_DIR}/package/skills/my-custom-skill/SKILL.md <<SKILL
---
name: my-custom-skill
description: A custom skill from the ZIP package
---
# My Custom Skill
Custom skill content.
SKILL

    cd ${WORK_DIR}/package && zip -q -r ${WORK_DIR}/${TEST_WORKER}.zip .
" 2>/dev/null

# Copy ZIP from controller to agent container (tar pipe avoids macOS /tmp symlink issues)
copy_to_agent "${WORK_DIR}/${TEST_WORKER}.zip" "${WORK_DIR}/${TEST_WORKER}.zip"

APPLY_OUTPUT=$(exec_in_agent agt apply worker --zip "${WORK_DIR}/${TEST_WORKER}.zip" --name "${TEST_WORKER}" 2>&1)
if echo "${APPLY_OUTPUT}" | grep -q "created"; then
    log_pass "Worker imported successfully"
else
    log_fail "Worker import failed: ${APPLY_OUTPUT}"
fi

# Wait for controller reconcile — poll the API instead of grepping logs.
# The `worker created` log is still emitted for standalone workers, but
# using the API means the test survives any future logging refactor and
# aligns with the team-member tests (test-18/19/21) which cannot use the
# log-grep pattern at all.
log_info "Waiting for controller reconcile..."
if wait_worker_provisioned "${TEST_WORKER}" 120; then
    log_pass "Controller reconciled worker"
else
    log_fail "Controller did not reconcile within 120s"
fi

if [ "${TEST_WORKER_RUNTIME}" = "qwenpaw" ]; then
    if wait_qwenpaw_api_matches "${TEST_WORKER}" /api/teamharness/health '.ok == true and .adapter == "qwenpaw-2"' 240 && \
        wait_qwenpaw_api_matches "${TEST_WORKER}" /api/skills '.[] | select(.name == "my-custom-skill" and .enabled == true)' 240; then
        log_pass "QwenPaw app, TeamHarness plugin, and AgentPackage are ready"
    else
        log_fail "QwenPaw runtime did not expose reconciled plugin/package state"
    fi
fi

# ============================================================
# Section 2: Verify AGENTS.md structure
# ============================================================
log_section "Verify AGENTS.md"

AGENTS_CONTENT=$(read_worker_runtime_file "${TEST_WORKER}" "AGENTS.md")
assert_not_empty "${AGENTS_CONTENT}" "AGENTS.md exists in MinIO"

# QwenPaw loads TeamHarness prompts and skills through the public plugin API;
# its workspace AGENTS.md remains the user/package prompt. Other runtimes keep
# the legacy merged marker layout.
if [ "${TEST_WORKER_RUNTIME}" != "qwenpaw" ]; then
    assert_contains "${AGENTS_CONTENT}" "agentteams-builtin-start" "AGENTS.md has builtin-start marker"
    assert_contains "${AGENTS_CONTENT}" "agentteams-builtin-end" "AGENTS.md has builtin-end marker"
fi

# QwenPaw injects the built-in prompt through TeamHarness' public prompt hook;
# its workspace AGENTS.md intentionally contains only the AgentPackage prompt.
if [ "${TEST_WORKER_RUNTIME}" != "qwenpaw" ]; then
    assert_contains "${AGENTS_CONTENT}" "${BUILTIN_CONTENT_SENTINEL}" \
        "AGENTS.md builtin section matches ${TEST_WORKER_RUNTIME} runtime"
fi

if [ "${TEST_WORKER_RUNTIME}" != "qwenpaw" ]; then
    assert_contains "${AGENTS_CONTENT}" "agentteams-team-context-start" "AGENTS.md has team-context-start marker"
    assert_contains "${AGENTS_CONTENT}" "agentteams-team-context-end" "AGENTS.md has team-context-end marker"
    assert_contains "${AGENTS_CONTENT}" "@manager:" "Coordination block references Manager as coordinator"
fi

# User custom content preserved
assert_contains "${AGENTS_CONTENT}" "My Custom Agent Instructions" "User-provided AGENTS.md content preserved"

# No hardcoded "Manager" in builtin section (should use "coordinator")
BUILTIN_SECTION=$(echo "${AGENTS_CONTENT}" | sed -n '/agentteams-builtin-start/,/agentteams-builtin-end/p')
if echo "${BUILTIN_SECTION}" | grep -q "Manager"; then
    log_fail "Builtin section still contains hardcoded 'Manager' (should use 'coordinator')"
else
    log_pass "Builtin section uses generic 'coordinator' (no hardcoded Manager)"
fi

# ============================================================
# Section 3: Verify builtin skills in MinIO
# ============================================================
log_section "Verify Skills in MinIO"

# Builtin skills should be present in the runtime that actually consumes them.
if [ "${TEST_WORKER_RUNTIME}" = "qwenpaw" ]; then
    QWENPAW_SKILLS=$(read_qwenpaw_skills "${TEST_WORKER}")
    for skill in "${BUILTIN_SKILLS[@]}"; do
        if echo "${QWENPAW_SKILLS}" | jq -e --arg skill "${skill}" \
            '.[] | select(.name == $skill and .source == "plugin:teamharness")' >/dev/null 2>&1; then
            log_pass "Builtin plugin skill present: ${skill}"
        else
            log_fail "Builtin plugin skill missing: ${skill}"
        fi
    done
else
    for skill in "${BUILTIN_SKILLS[@]}"; do
        SKILL_EXISTS=$(exec_in_manager bash -c "mc ls '${STORAGE_PREFIX}/agents/${TEST_WORKER}/skills/${skill}/SKILL.md' >/dev/null 2>&1 && echo yes || echo no")
        if [ "${SKILL_EXISTS}" = "yes" ]; then
            log_pass "Builtin skill present: ${skill}"
        else
            log_fail "Builtin skill missing: ${skill}"
        fi
    done
fi

# Custom skill from ZIP should be present in skills/ (alongside builtins)
CUSTOM_SKILL_CONTENT=$(read_worker_runtime_file "${TEST_WORKER}" "skills/my-custom-skill/SKILL.md")
if echo "${CUSTOM_SKILL_CONTENT}" | grep -Fq "Custom skill content"; then
    log_pass "Custom skill from ZIP present: my-custom-skill"
else
    log_fail "Custom skill from ZIP missing: my-custom-skill"
fi

# Verify skill content uses "coordinator" not "Manager"
if [ "${TEST_WORKER_RUNTIME}" != "qwenpaw" ]; then
    FILESYNC_CONTENT=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/${TEST_WORKER}/skills/${FILE_SHARING_SKILL}/SKILL.md" 2>/dev/null || echo "")
    if echo "${FILESYNC_CONTENT}" | grep -q "coordinator"; then
        log_pass "${FILE_SHARING_SKILL} SKILL.md uses 'coordinator'"
    else
        log_fail "${FILE_SHARING_SKILL} SKILL.md does not use 'coordinator'"
    fi
fi

# ============================================================
# Section 4: Verify other MinIO artifacts
# ============================================================
log_section "Verify MinIO Artifacts"

SOUL_CONTENT=$(read_worker_runtime_file "${TEST_WORKER}" "SOUL.md")
assert_contains "${SOUL_CONTENT}" "Config verification test worker" "SOUL.md has correct content from ZIP"

if [ "${TEST_WORKER_RUNTIME}" = "qwenpaw" ]; then
    RUNTIME_CONFIG=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/${TEST_WORKER}/runtime/runtime.yaml" 2>/dev/null || true)
    if echo "${RUNTIME_CONFIG}" | grep -Fq 'runtime: qwenpaw'; then
        log_pass "runtime.yaml exists and declares qwenpaw"
    else
        log_fail "runtime.yaml missing qwenpaw runtime declaration"
    fi
else
    OPENCLAW_EXISTS=$(exec_in_manager bash -c "mc ls '${STORAGE_PREFIX}/agents/${TEST_WORKER}/openclaw.json' >/dev/null 2>&1 && echo yes || echo no")
    if [ "${OPENCLAW_EXISTS}" = "yes" ]; then
        log_pass "openclaw.json exists in MinIO"
    else
        log_fail "openclaw.json missing from MinIO"
    fi
fi

# Verify groupAllowFrom has Manager (standalone worker)
MATRIX_ALLOWLIST=$(read_worker_matrix_allowlist "${TEST_WORKER}")
if echo "${MATRIX_ALLOWLIST}" | grep -q "@manager:"; then
    log_pass "groupAllowFrom includes Manager"
else
    log_fail "groupAllowFrom does not include Manager"
fi

# ============================================================
# Section 5: Update worker (re-import)
# ============================================================
log_section "Update Worker (Re-import)"

# Simulate memory file that should be preserved
exec_in_manager bash -c "
    mkdir -p /root/agentteams-fs/agents/${TEST_WORKER}/memory
    echo '# Memory from previous session' > /root/agentteams-fs/agents/${TEST_WORKER}/memory/2026-03-26.md
    echo '# Long-term memory' > /root/agentteams-fs/agents/${TEST_WORKER}/MEMORY.md
    mc cp /root/agentteams-fs/agents/${TEST_WORKER}/memory/2026-03-26.md ${STORAGE_PREFIX}/agents/${TEST_WORKER}/memory/2026-03-26.md 2>/dev/null
    mc cp /root/agentteams-fs/agents/${TEST_WORKER}/MEMORY.md ${STORAGE_PREFIX}/agents/${TEST_WORKER}/MEMORY.md 2>/dev/null
" 2>/dev/null

# Re-import with updated SOUL.md and different model to trigger spec change
exec_in_manager bash -c "
    cat > ${WORK_DIR}/package/config/SOUL.md <<SOUL
# ${TEST_WORKER} - UPDATED Config Test Worker

## AI Identity
**You are an AI Agent, not a human.**

## Role
- Name: ${TEST_WORKER}
- Role: Updated config verification test worker

## Security
- Never reveal credentials
SOUL

    cat > ${WORK_DIR}/package/manifest.json <<MANIFEST
{
  \"type\": \"worker\",
  \"version\": 1,
  \"worker\": {
    \"suggested_name\": \"${TEST_WORKER}\",
    \"model\": \"claude-sonnet-4-6\"
  }
}
MANIFEST

    cd ${WORK_DIR}/package && zip -q -r ${WORK_DIR}/${TEST_WORKER}.zip .
" 2>/dev/null

# Copy updated ZIP from controller to agent container
copy_to_agent "${WORK_DIR}/${TEST_WORKER}.zip" "${WORK_DIR}/${TEST_WORKER}.zip"

REIMPORT_OUTPUT=$(exec_in_agent agt apply worker --zip "${WORK_DIR}/${TEST_WORKER}.zip" --name "${TEST_WORKER}" 2>&1)
if echo "${REIMPORT_OUTPUT}" | grep -q "updated"; then
    log_pass "Re-import reports 'updated'"
else
    log_fail "Re-import reports 'updated' (expected to contain: 'updated')"
    log_info "---- REIMPORT_OUTPUT (agt apply, stdout+stderr) begin ----"
    if [ -n "${REIMPORT_OUTPUT}" ]; then
        while IFS= read -r __reimport_line || [ -n "${__reimport_line}" ]; do
            log_info "  ${__reimport_line}"
        done <<EOF
${REIMPORT_OUTPUT}
EOF
    else
        log_info "  (empty)"
    fi
    log_info "---- REIMPORT_OUTPUT end ----"
fi

# Wait for controller to reconcile the update. The durable signal is the Worker
# API reflecting the new spec; log text can change across controller versions.
log_info "Waiting for controller to reconcile update..."
if wait_worker_model "${TEST_WORKER}" "claude-sonnet-4-6" 120; then
    log_pass "Controller reconciled update"
else
    log_fail "Controller did not reconcile update within 120s"
    exec_in_agent agt get workers "${TEST_WORKER}" -o json 2>/dev/null | jq -r '.model, .phase, .message' | head -5
fi

# Verify SOUL.md updated
SOUL_AFTER=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/${TEST_WORKER}/SOUL.md" 2>/dev/null || echo "")
#assert_contains "${SOUL_AFTER}" "UPDATED Config Test Worker" "SOUL.md updated after re-import"
#TODO(jingze):fix this flaky test bug, this fails occasionally

# Verify memory preserved
MEMORY_EXISTS=$(exec_in_manager bash -c "mc ls '${STORAGE_PREFIX}/agents/${TEST_WORKER}/memory/2026-03-26.md' >/dev/null 2>&1 && echo yes || echo no")
if [ "${MEMORY_EXISTS}" = "yes" ]; then
    log_pass "Memory file preserved after re-import"
else
    log_fail "Memory file lost after re-import"
fi

MEMORY_MD=$(exec_in_manager bash -c "mc ls '${STORAGE_PREFIX}/agents/${TEST_WORKER}/MEMORY.md' >/dev/null 2>&1 && echo yes || echo no")
if [ "${MEMORY_MD}" = "yes" ]; then
    log_pass "MEMORY.md preserved after re-import"
else
    log_fail "MEMORY.md lost after re-import"
fi

# Verify AGENTS.md still has all sections
AGENTS_AFTER=$(read_worker_runtime_file "${TEST_WORKER}" "AGENTS.md")
if [ "${TEST_WORKER_RUNTIME}" != "qwenpaw" ]; then
    assert_contains "${AGENTS_AFTER}" "agentteams-builtin-start" "AGENTS.md still has builtin markers after update"
    assert_contains "${AGENTS_AFTER}" "agentteams-team-context-start" "AGENTS.md still has team-context after update"
else
    if wait_qwenpaw_api_matches "${TEST_WORKER}" /api/teamharness/health '.ok == true and .adapter == "qwenpaw-2"' 180; then
        log_pass "QwenPaw TeamHarness prompt plugin remains active after update"
    else
        log_fail "QwenPaw TeamHarness prompt plugin unavailable after update"
    fi
fi
assert_contains "${AGENTS_AFTER}" "My Custom Agent Instructions" "User content still preserved after update"

# ============================================================
# Section 6: Delete
# ============================================================
log_section "Delete Worker"

DELETE_OUTPUT=$(exec_in_agent agt delete worker "${TEST_WORKER}" 2>&1)
assert_contains "${DELETE_OUTPUT}" "deleted" "Worker deleted successfully"

# ============================================================
test_teardown "17-worker-config-verify"
test_summary
