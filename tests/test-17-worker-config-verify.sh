#!/bin/bash
# test-17-worker-config-verify.sh - Case 17: Verify Worker import config artifacts
#
# Tests single worker import (create + update) and verifies MinIO artifacts:
#   1. Create worker via hiclaw apply worker --zip
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
STORAGE_PREFIX="hiclaw/hiclaw-storage"

_cleanup() {
    log_info "Cleaning up: ${TEST_WORKER}"
    exec_in_manager hiclaw delete worker "${TEST_WORKER}" 2>/dev/null || true
    exec_in_manager mc rm "${STORAGE_PREFIX}/hiclaw-config/packages/${TEST_WORKER}.zip" 2>/dev/null || true
    sleep 5
    docker rm -f "hiclaw-worker-${TEST_WORKER}" 2>/dev/null || true
    exec_in_manager rm -rf "/root/hiclaw-fs/agents/${TEST_WORKER}" 2>/dev/null || true
    exec_in_manager rm -rf "/tmp/hiclaw-test-${TEST_WORKER}" 2>/dev/null || true
    exec_in_manager mc rm -r --force "${STORAGE_PREFIX}/agents/${TEST_WORKER}/" 2>/dev/null || true
}
trap _cleanup EXIT

# ============================================================
# Section 1: Create test ZIP and import
# ============================================================
log_section "Create and Import Worker"

WORK_DIR="/tmp/hiclaw-test-${TEST_WORKER}"

exec_in_manager bash -c "
    mkdir -p ${WORK_DIR}/package/config ${WORK_DIR}/package/skills/my-custom-skill

    cat > ${WORK_DIR}/package/manifest.json <<MANIFEST
{
  \"type\": \"worker\",
  \"version\": 1,
  \"worker\": {
    \"suggested_name\": \"${TEST_WORKER}\",
    \"model\": \"qwen3.5-plus\"
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

APPLY_OUTPUT=$(exec_in_manager hiclaw apply worker --zip "${WORK_DIR}/${TEST_WORKER}.zip" --name "${TEST_WORKER}" 2>&1)
if echo "${APPLY_OUTPUT}" | grep -q "created"; then
    log_pass "Worker imported successfully"
else
    log_fail "Worker import failed: ${APPLY_OUTPUT}"
fi

# Wait for controller reconcile
log_info "Waiting for controller reconcile..."
TIMEOUT=120; ELAPSED=0
while [ "${ELAPSED}" -lt "${TIMEOUT}" ]; do
    if exec_in_manager cat /var/log/hiclaw/hiclaw-controller-error.log 2>/dev/null | grep -q "worker created.*${TEST_WORKER}"; then
        break
    fi
    sleep 5; ELAPSED=$((ELAPSED + 5))
done

if [ "${ELAPSED}" -lt "${TIMEOUT}" ]; then
    log_pass "Controller reconciled worker (took ~${ELAPSED}s)"
else
    log_fail "Controller did not reconcile within ${TIMEOUT}s"
fi

# ============================================================
# Section 2: Verify AGENTS.md structure
# ============================================================
log_section "Verify AGENTS.md"

AGENTS_CONTENT=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/${TEST_WORKER}/AGENTS.md" 2>/dev/null || echo "")
assert_not_empty "${AGENTS_CONTENT}" "AGENTS.md exists in MinIO"

# Builtin markers present
assert_contains "${AGENTS_CONTENT}" "hiclaw-builtin-start" "AGENTS.md has builtin-start marker"
assert_contains "${AGENTS_CONTENT}" "hiclaw-builtin-end" "AGENTS.md has builtin-end marker"

# Builtin content filled (not empty between markers)
assert_contains "${AGENTS_CONTENT}" "Every Session" "AGENTS.md builtin section has content (not empty)"

# Team-context coordination block present
assert_contains "${AGENTS_CONTENT}" "hiclaw-team-context-start" "AGENTS.md has team-context-start marker"
assert_contains "${AGENTS_CONTENT}" "hiclaw-team-context-end" "AGENTS.md has team-context-end marker"

# Standalone worker: coordinator should be Manager
assert_contains "${AGENTS_CONTENT}" "@manager:" "Coordination block references Manager as coordinator"

# User custom content preserved
assert_contains "${AGENTS_CONTENT}" "My Custom Agent Instructions" "User-provided AGENTS.md content preserved"

# No hardcoded "Manager" in builtin section (should use "coordinator")
BUILTIN_SECTION=$(echo "${AGENTS_CONTENT}" | sed -n '/hiclaw-builtin-start/,/hiclaw-builtin-end/p')
if echo "${BUILTIN_SECTION}" | grep -q "Manager"; then
    log_fail "Builtin section still contains hardcoded 'Manager' (should use 'coordinator')"
else
    log_pass "Builtin section uses generic 'coordinator' (no hardcoded Manager)"
fi

# ============================================================
# Section 3: Verify builtin skills in MinIO
# ============================================================
log_section "Verify Skills in MinIO"

# Builtin skills should be present
for skill in file-sync task-progress mcporter find-skills project-participation; do
    SKILL_EXISTS=$(exec_in_manager bash -c "mc ls '${STORAGE_PREFIX}/agents/${TEST_WORKER}/skills/${skill}/SKILL.md' >/dev/null 2>&1 && echo yes || echo no")
    if [ "${SKILL_EXISTS}" = "yes" ]; then
        log_pass "Builtin skill present: ${skill}"
    else
        log_fail "Builtin skill missing: ${skill}"
    fi
done

# Custom skill from ZIP should be present in skills/ (alongside builtins)
CUSTOM_SKILL=$(exec_in_manager bash -c "mc ls '${STORAGE_PREFIX}/agents/${TEST_WORKER}/skills/my-custom-skill/SKILL.md' >/dev/null 2>&1 && echo yes || echo no")
if [ "${CUSTOM_SKILL}" = "yes" ]; then
    log_pass "Custom skill from ZIP present: my-custom-skill"
else
    log_fail "Custom skill from ZIP missing: my-custom-skill"
fi

# Verify skill content uses "coordinator" not "Manager"
FILESYNC_CONTENT=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/${TEST_WORKER}/skills/file-sync/SKILL.md" 2>/dev/null || echo "")
if echo "${FILESYNC_CONTENT}" | grep -q "coordinator"; then
    log_pass "file-sync SKILL.md uses 'coordinator'"
else
    log_fail "file-sync SKILL.md does not use 'coordinator'"
fi

# ============================================================
# Section 4: Verify other MinIO artifacts
# ============================================================
log_section "Verify MinIO Artifacts"

SOUL_CONTENT=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/${TEST_WORKER}/SOUL.md" 2>/dev/null || echo "")
assert_contains "${SOUL_CONTENT}" "Config verification test worker" "SOUL.md has correct content from ZIP"

OPENCLAW_EXISTS=$(exec_in_manager bash -c "mc ls '${STORAGE_PREFIX}/agents/${TEST_WORKER}/openclaw.json' >/dev/null 2>&1 && echo yes || echo no")
if [ "${OPENCLAW_EXISTS}" = "yes" ]; then
    log_pass "openclaw.json exists in MinIO"
else
    log_fail "openclaw.json missing from MinIO"
fi

# Verify groupAllowFrom has Manager (standalone worker)
OPENCLAW_CONTENT=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/${TEST_WORKER}/openclaw.json" 2>/dev/null || echo "")
if echo "${OPENCLAW_CONTENT}" | jq -r '.channels.matrix.groupAllowFrom[]' 2>/dev/null | grep -q "@manager:"; then
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
    mkdir -p /root/hiclaw-fs/agents/${TEST_WORKER}/memory
    echo '# Memory from previous session' > /root/hiclaw-fs/agents/${TEST_WORKER}/memory/2026-03-26.md
    echo '# Long-term memory' > /root/hiclaw-fs/agents/${TEST_WORKER}/MEMORY.md
    mc cp /root/hiclaw-fs/agents/${TEST_WORKER}/memory/2026-03-26.md ${STORAGE_PREFIX}/agents/${TEST_WORKER}/memory/2026-03-26.md 2>/dev/null
    mc cp /root/hiclaw-fs/agents/${TEST_WORKER}/MEMORY.md ${STORAGE_PREFIX}/agents/${TEST_WORKER}/MEMORY.md 2>/dev/null
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

REIMPORT_OUTPUT=$(exec_in_manager hiclaw apply worker --zip "${WORK_DIR}/${TEST_WORKER}.zip" --name "${TEST_WORKER}" 2>&1)
assert_contains "${REIMPORT_OUTPUT}" "updated" "Re-import reports 'updated'"

# Wait for controller to reconcile the update (poll for "worker updated" in logs)
log_info "Waiting for controller to reconcile update..."
UPDATE_TIMEOUT=120; UPDATE_ELAPSED=0
UPDATE_DONE=false
while [ "${UPDATE_ELAPSED}" -lt "${UPDATE_TIMEOUT}" ]; do
    if exec_in_manager cat /var/log/hiclaw/hiclaw-controller-error.log 2>/dev/null | grep -q "worker updated.*${TEST_WORKER}"; then
        UPDATE_DONE=true
        break
    fi
    sleep 5; UPDATE_ELAPSED=$((UPDATE_ELAPSED + 5))
done

if [ "${UPDATE_DONE}" = true ]; then
    log_pass "Controller reconciled update (took ~${UPDATE_ELAPSED}s)"
else
    log_fail "Controller did not reconcile update within ${UPDATE_TIMEOUT}s"
    exec_in_manager cat /var/log/hiclaw/hiclaw-controller-error.log 2>/dev/null | grep "${TEST_WORKER}" | tail -5
fi

# Verify SOUL.md updated
SOUL_AFTER=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/${TEST_WORKER}/SOUL.md" 2>/dev/null || echo "")
assert_contains "${SOUL_AFTER}" "UPDATED Config Test Worker" "SOUL.md updated after re-import"

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
AGENTS_AFTER=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/${TEST_WORKER}/AGENTS.md" 2>/dev/null || echo "")
assert_contains "${AGENTS_AFTER}" "hiclaw-builtin-start" "AGENTS.md still has builtin markers after update"
assert_contains "${AGENTS_AFTER}" "hiclaw-team-context-start" "AGENTS.md still has team-context after update"
assert_contains "${AGENTS_AFTER}" "My Custom Agent Instructions" "User content still preserved after update"

# ============================================================
# Section 6: Delete
# ============================================================
log_section "Delete Worker"

DELETE_OUTPUT=$(exec_in_manager hiclaw delete worker "${TEST_WORKER}" 2>&1)
assert_contains "${DELETE_OUTPUT}" "deleted" "Worker deleted successfully"

# ============================================================
test_teardown "17-worker-config-verify"
test_summary
