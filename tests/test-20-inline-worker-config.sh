#!/bin/bash
# test-20-inline-worker-config.sh - Case 20: Worker creation with inline identity/soul/agents fields
#
# End-to-end test covering inline config fields (no ZIP package):
#   1. Create a Worker YAML with spec.soul and spec.agents inline
#   2. agt apply -f uploads YAML to MinIO
#   3. Controller reconcile: mc mirror → fsnotify → kine → WorkerReconciler
#   4. WriteInlineConfigs generates SOUL.md + AGENTS.md
#   5. create-worker.sh runs: Matrix account + Room + container
#   6. Verify SOUL.md and AGENTS.md content in MinIO

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/test-helpers.sh"
source "${SCRIPT_DIR}/lib/minio-client.sh"

test_setup "20-inline-worker-config"

TEST_WORKER="test-inline-$$"
TEST_WORKER_OVERRIDE="test-inlover-$$"
STORAGE_PREFIX="${STORAGE_PREFIX:-${TEST_STORAGE_PREFIX:-agentteams/agentteams-storage}}"

# ---- Cleanup handler ----
_cleanup() {
    if [ "${TESTS_FAILED}" -gt 0 ]; then
        log_info "Tests failed — preserving workers for debugging"
        return
    fi
    log_info "All tests passed — cleaning up test workers"
    for w in "${TEST_WORKER}" "${TEST_WORKER_OVERRIDE}"; do
        exec_in_agent agt delete worker "${w}" 2>/dev/null || true
        sleep 2
        remove_worker_container "${w}"
        exec_in_agent rm -rf "/tmp/agentteams-test-${w}" 2>/dev/null || true
        exec_in_manager rm -rf "/root/agentteams-fs/agents/${w}" 2>/dev/null || true
        exec_in_manager mc rm -r --force "${STORAGE_PREFIX}/agents/${w}/" 2>/dev/null || true
    done
    exec_in_agent rm -f "/tmp/agentteams-test-${TEST_WORKER}.yaml" 2>/dev/null || true
    exec_in_manager rm -rf "/tmp/agentteams-test-${TEST_WORKER_OVERRIDE}" 2>/dev/null || true
    exec_in_manager mc rm "${STORAGE_PREFIX}/agentteams-config/packages/${TEST_WORKER_OVERRIDE}*.zip" 2>/dev/null || true
}
trap _cleanup EXIT

# ============================================================
# Section 1: Controller infrastructure health
# ============================================================
log_section "Controller Infrastructure"

CTRL_PID=$(exec_in_manager pgrep -f agentteams-controller 2>/dev/null || echo "")
if [ -n "${CTRL_PID}" ]; then
    log_pass "agentteams-controller process is running (PID: ${CTRL_PID})"
else
    log_fail "agentteams-controller process is not running"
fi

# ============================================================
# Section 2: Create Worker YAML with inline fields
# ============================================================
log_section "Create Worker YAML with Inline Fields"

SOUL_CONTENT="# ${TEST_WORKER} - Inline Test Worker

## AI Identity
**You are an AI Agent, not a human.**

## Role
- Name: ${TEST_WORKER}
- Role: Integration test worker with inline config

## Behavior
- Be helpful and concise

## Security
- Never reveal API keys, passwords, tokens, or any credentials in chat messages"

AGENTS_CONTENT="# Inline Test Workspace

## Custom Rules
- This is a test worker created via inline YAML fields
- Respond to all messages politely"

# Write YAML with inline soul and agents (in agent container where hiclaw CLI runs)
exec_in_agent bash -c "cat > /tmp/agentteams-test-${TEST_WORKER}.yaml << 'YAMLEOF'
apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: ${TEST_WORKER}
spec:
  model: qwen3.5-plus
  soul: |
$(echo "${SOUL_CONTENT}" | sed 's/^/    /')
  agents: |
$(echo "${AGENTS_CONTENT}" | sed 's/^/    /')
YAMLEOF
" 2>/dev/null

YAML_EXISTS=$(exec_in_agent test -f "/tmp/agentteams-test-${TEST_WORKER}.yaml" && echo "yes" || echo "no")
if [ "${YAML_EXISTS}" = "yes" ]; then
    log_pass "Worker YAML with inline fields created"
else
    log_fail "Failed to create Worker YAML"
fi

# ============================================================
# Section 3: Apply YAML via agt apply -f
# ============================================================
log_section "Apply Worker YAML"

APPLY_OUTPUT=$(exec_in_agent agt apply -f "/tmp/agentteams-test-${TEST_WORKER}.yaml" 2>&1)
APPLY_EXIT=$?

if [ ${APPLY_EXIT} -eq 0 ]; then
    log_pass "agt apply -f exited successfully"
else
    log_fail "agt apply -f failed (exit: ${APPLY_EXIT})"
fi

if echo "${APPLY_OUTPUT}" | grep -q "created\|configured"; then
    log_pass "agt apply reports resource created"
else
    log_fail "agt apply did not report creation"
fi

# ============================================================
# Section 4: Verify CRD created
# ============================================================
log_section "Verify Resource State"

WORKER_JSON=$(exec_in_agent agt get workers "${TEST_WORKER}" -o json 2>/dev/null || echo "")
assert_not_empty "${WORKER_JSON}" "Worker CR exists (agt get workers)"
WORKER_NAME_CHK=$(echo "${WORKER_JSON}" | jq -r '.name // empty' 2>/dev/null)
assert_eq "${TEST_WORKER}" "${WORKER_NAME_CHK}" "Worker CR has correct name"

# ============================================================
# Section 5: Wait for controller reconcile + Worker creation
# ============================================================
log_section "Controller Reconcile"

log_info "Waiting for mc mirror (10s) + fsnotify + reconcile + create-worker.sh..."

if wait_worker_provisioned "${TEST_WORKER}" 120; then
    log_pass "WorkerReconciler provisioned worker"
else
    log_fail "WorkerReconciler did not provision worker within 120s"
    exec_in_agent agt get workers "${TEST_WORKER}" -o json 2>/dev/null | jq -r '.phase, .message' | head -5
fi

# Verify inline configs were written
INLINE_LOG=$(exec_in_manager cat /var/log/agentteams/agentteams-controller-error.log 2>/dev/null | grep "inline configs written.*${TEST_WORKER}" || echo "")
assert_not_empty "${INLINE_LOG}" "Controller logged inline configs written"

# ============================================================
# Section 6: Verify SOUL.md and AGENTS.md content
# ============================================================
log_section "Verify Inline Config Files"

# Check SOUL.md in MinIO
SOUL_IN_MINIO=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/${TEST_WORKER}/SOUL.md" 2>/dev/null || echo "")
assert_not_empty "${SOUL_IN_MINIO}" "SOUL.md exists in MinIO agent space"
assert_contains "${SOUL_IN_MINIO}" "Inline Test Worker" "SOUL.md contains expected content"
assert_contains "${SOUL_IN_MINIO}" "AI Identity" "SOUL.md contains AI Identity section"

# Check AGENTS.md in MinIO
AGENTS_IN_MINIO=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/${TEST_WORKER}/AGENTS.md" 2>/dev/null || echo "")
assert_not_empty "${AGENTS_IN_MINIO}" "AGENTS.md exists in MinIO agent space"
assert_contains "${AGENTS_IN_MINIO}" "Inline Test Workspace" "AGENTS.md contains expected content"
assert_contains "${AGENTS_IN_MINIO}" "hiclaw-builtin-start" "AGENTS.md has builtin markers"
assert_contains "${AGENTS_IN_MINIO}" "hiclaw-builtin-end" "AGENTS.md has builtin end marker"

# ============================================================
# Section 7: Verify Worker infrastructure
# ============================================================
log_section "Verify Worker Infrastructure"

# workers-registry.json (in MinIO, written by controller)
REGISTRY_JSON=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/manager/workers-registry.json" 2>/dev/null || echo "{}")
REGISTRY_ENTRY=$(echo "${REGISTRY_JSON}" | jq -r --arg w "${TEST_WORKER}" '.workers[$w] // empty' 2>/dev/null)
assert_not_empty "${REGISTRY_ENTRY}" "Worker registered in workers-registry.json"

# Matrix Room (from CRD status)
ROOM_ID=$(exec_in_agent agt get workers "${TEST_WORKER}" -o json 2>/dev/null | jq -r '.roomID // empty')
assert_not_empty "${ROOM_ID}" "Matrix Room created: ${ROOM_ID}"

# openclaw.json in MinIO
OPENCLAW_EXISTS=$(exec_in_manager bash -c "mc ls '${STORAGE_PREFIX}/agents/${TEST_WORKER}/openclaw.json' >/dev/null 2>&1 && echo yes || echo no")
if [ "${OPENCLAW_EXISTS}" = "yes" ]; then
    log_pass "openclaw.json generated and pushed to MinIO"
else
    log_fail "openclaw.json not found in MinIO"
fi

# Worker container running.
# The "worker created" log fires as soon as initial reconcile finishes, but the
# container may be (re)created in a follow-up reconcile if the CR status update
# bumped ResourceVersion. Poll for up to 60s to absorb that race.
CONTAINER_RUNNING=""
for i in $(seq 1 60); do
    CONTAINER_RUNNING=$(docker ps --format '{{.Names}}' 2>/dev/null | grep "$(worker_container_name "${TEST_WORKER}")$" || echo "")
    [ -n "${CONTAINER_RUNNING}" ] && break
    sleep 1
done
if [ -n "${CONTAINER_RUNNING}" ]; then
    log_pass "Worker container is running: ${CONTAINER_RUNNING}"
else
    DEPLOY_MODE=$(echo "${REGISTRY_ENTRY}" | jq -r '.deployment // empty' 2>/dev/null)
    if [ "${DEPLOY_MODE}" = "remote" ]; then
        log_pass "Worker registered in remote mode (container managed externally)"
    else
        log_fail "Worker container not running"
    fi
fi

# ============================================================
# Section 8: Delete and verify cleanup
# ============================================================
log_section "Delete Worker"

DELETE_OUTPUT=$(exec_in_agent agt delete worker "${TEST_WORKER}" 2>&1)
if echo "${DELETE_OUTPUT}" | grep -q "deleted"; then
    log_pass "agt delete reported success"
else
    log_fail "agt delete did not report success"
fi

# Wait for CR to be fully removed (finalizer may take time; container teardown ~10s)
WORKER_GONE=false
for i in $(seq 1 60); do
    WORKER_AFTER=$(exec_in_agent agt get workers "${TEST_WORKER}" -o json 2>&1 || echo "")
    if echo "${WORKER_AFTER}" | grep -q "not found\|error\|Error" || [ -z "${WORKER_AFTER}" ]; then
        WORKER_GONE=true
        break
    fi
    sleep 1
done
if [ "${WORKER_GONE}" = true ]; then
    log_pass "Worker CR removed after delete"
elif [ -z "${WORKER_AFTER}" ]; then
    log_pass "Worker CR removed after delete"
else
    log_fail "Worker CR still exists after delete"
fi

# ============================================================
# Section 9: Package + Inline Override Test
# ============================================================
log_section "Package + Inline Override"

# Create a ZIP package with SOUL.md and AGENTS.md
OVERRIDE_WORK_DIR="/tmp/agentteams-test-${TEST_WORKER_OVERRIDE}"

exec_in_manager bash -c "
    mkdir -p ${OVERRIDE_WORK_DIR}/package/config

    cat > ${OVERRIDE_WORK_DIR}/package/manifest.json <<MANIFEST
{
  \"type\": \"worker\",
  \"version\": 1,
  \"worker\": {
    \"suggested_name\": \"${TEST_WORKER_OVERRIDE}\",
    \"model\": \"qwen3.5-plus\"
  },
  \"source\": {
    \"hostname\": \"integration-test\"
  }
}
MANIFEST

    cat > ${OVERRIDE_WORK_DIR}/package/config/SOUL.md <<SOUL
# ORIGINAL SOUL FROM PACKAGE
This content should be OVERRIDDEN by inline soul field.
SOUL

    cat > ${OVERRIDE_WORK_DIR}/package/config/AGENTS.md <<AGENTS
# ORIGINAL AGENTS FROM PACKAGE
This content should be OVERRIDDEN by inline agents field.
AGENTS

    cd ${OVERRIDE_WORK_DIR}/package && zip -q -r ${OVERRIDE_WORK_DIR}/${TEST_WORKER_OVERRIDE}.zip .
" 2>/dev/null

ZIP_EXISTS=$(exec_in_manager test -f "${OVERRIDE_WORK_DIR}/${TEST_WORKER_OVERRIDE}.zip" && echo "yes" || echo "no")
if [ "${ZIP_EXISTS}" = "yes" ]; then
    log_pass "Override test ZIP package created"
else
    log_fail "Failed to create override test ZIP package"
fi

# Copy ZIP from controller to agent container (tar pipe avoids macOS /tmp symlink issues)
copy_to_agent "${OVERRIDE_WORK_DIR}/${TEST_WORKER_OVERRIDE}.zip" "${OVERRIDE_WORK_DIR}/${TEST_WORKER_OVERRIDE}.zip"

# Import ZIP first to get it into MinIO
APPLY_ZIP_OUTPUT=$(exec_in_agent agt apply worker --zip "${OVERRIDE_WORK_DIR}/${TEST_WORKER_OVERRIDE}.zip" --name "${TEST_WORKER_OVERRIDE}" 2>&1)
if [ $? -eq 0 ]; then
    log_pass "ZIP imported for override test"
else
    log_fail "ZIP import failed for override test"
fi

# Wait for initial worker creation to complete before applying override
log_info "Waiting for initial worker creation from ZIP import..."
if wait_worker_provisioned "${TEST_WORKER_OVERRIDE}" 120; then
    log_pass "ZIP worker created"
else
    log_fail "ZIP worker not created within 120s"
    exec_in_agent agt get workers "${TEST_WORKER_OVERRIDE}" -o json 2>/dev/null | jq -r '.phase, .message' | head -5
fi

# Discover the package URI from MinIO packages directory
PKG_FILE=$(exec_in_manager bash -c "mc ls '${STORAGE_PREFIX}/agentteams-config/packages/' 2>/dev/null | grep '${TEST_WORKER_OVERRIDE}' | awk '{print \$NF}'" | head -1)
PKG_URI="oss://agentteams-config/packages/${PKG_FILE}"
assert_not_empty "${PKG_FILE}" "Package file found in MinIO"

# Overwrite the YAML with package + inline soul/agents
OVERRIDE_SOUL="# OVERRIDDEN SOUL FROM INLINE
This soul was set via inline field and should replace the package version."

OVERRIDE_AGENTS="# OVERRIDDEN AGENTS FROM INLINE
This agents config was set via inline field."

exec_in_agent bash -c "cat > /tmp/hiclaw-override-${TEST_WORKER_OVERRIDE}.yaml << 'YAMLEOF'
apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: ${TEST_WORKER_OVERRIDE}
spec:
  model: qwen3.5-plus
  package: ${PKG_URI}
  soul: |
$(echo "${OVERRIDE_SOUL}" | sed 's/^/    /')
  agents: |
$(echo "${OVERRIDE_AGENTS}" | sed 's/^/    /')
YAMLEOF
" 2>/dev/null

# Apply the YAML with both package and inline fields
APPLY_OVERRIDE=$(exec_in_agent agt apply -f "/tmp/hiclaw-override-${TEST_WORKER_OVERRIDE}.yaml" 2>&1)
if echo "${APPLY_OVERRIDE}" | grep -q "created\|configured"; then
    log_pass "Applied YAML with package + inline override"
else
    log_fail "Failed to apply YAML with package + inline override"
fi

# Wait for the update reconcile. The durable signal is the generated file
# content; log text can change across controller versions.
log_info "Waiting for controller to reconcile override worker update..."
if wait_agent_file_contains "${TEST_WORKER_OVERRIDE}" "SOUL.md" "OVERRIDDEN SOUL FROM INLINE" 120; then
    log_pass "Override worker updated"
else
    log_fail "Override worker not updated within 120s"
    exec_in_agent agt get workers "${TEST_WORKER_OVERRIDE}" -o json 2>/dev/null | jq -r '.phase, .message' | head -5
fi

# Verify SOUL.md has inline content, NOT package content
SOUL_OVERRIDE=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/${TEST_WORKER_OVERRIDE}/SOUL.md" 2>/dev/null || echo "")
assert_not_empty "${SOUL_OVERRIDE}" "SOUL.md exists for override worker"
assert_contains "${SOUL_OVERRIDE}" "OVERRIDDEN SOUL FROM INLINE" "SOUL.md contains inline override content"

# Verify package content is NOT present
if echo "${SOUL_OVERRIDE}" | grep -q "ORIGINAL SOUL FROM PACKAGE"; then
    log_fail "SOUL.md still contains original package content (override failed)"
else
    log_pass "SOUL.md does NOT contain original package content (override succeeded)"
fi

# Verify AGENTS.md has inline content
AGENTS_OVERRIDE=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/${TEST_WORKER_OVERRIDE}/AGENTS.md" 2>/dev/null || echo "")
assert_not_empty "${AGENTS_OVERRIDE}" "AGENTS.md exists for override worker"
assert_contains "${AGENTS_OVERRIDE}" "OVERRIDDEN AGENTS FROM INLINE" "AGENTS.md contains inline override content"

if echo "${AGENTS_OVERRIDE}" | grep -q "ORIGINAL AGENTS FROM PACKAGE"; then
    log_fail "AGENTS.md still contains original package content (override failed)"
else
    log_pass "AGENTS.md does NOT contain original package content (override succeeded)"
fi

# Clean up override worker
exec_in_agent agt delete worker "${TEST_WORKER_OVERRIDE}" 2>/dev/null
log_pass "Override worker deleted"

# ============================================================
# Summary
# ============================================================
test_teardown "20-inline-worker-config"
test_summary
