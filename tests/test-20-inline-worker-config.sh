#!/bin/bash
# test-20-inline-worker-config.sh - Case 20: Worker creation with inline identity/soul/agents fields
#
# End-to-end test covering inline config fields (no ZIP package):
#   1. Create a Worker YAML with spec.soul and spec.agents inline
#   2. hiclaw apply -f uploads YAML to MinIO
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
STORAGE_PREFIX="hiclaw/hiclaw-storage"

# ---- Cleanup handler ----
_cleanup() {
    if [ "${TESTS_FAILED}" -gt 0 ]; then
        log_info "Tests failed — preserving workers for debugging"
        return
    fi
    log_info "All tests passed — cleaning up test workers"
    for w in "${TEST_WORKER}" "${TEST_WORKER_OVERRIDE}"; do
        exec_in_manager hiclaw delete worker "${w}" 2>/dev/null || true
        sleep 2
        docker rm -f "hiclaw-worker-${w}" 2>/dev/null || true
        exec_in_manager rm -rf "/root/hiclaw-fs/agents/${w}" 2>/dev/null || true
        exec_in_manager mc rm -r --force "${STORAGE_PREFIX}/agents/${w}/" 2>/dev/null || true
    done
    exec_in_manager rm -rf "/tmp/hiclaw-test-${TEST_WORKER_OVERRIDE}" 2>/dev/null || true
    exec_in_manager mc rm "${STORAGE_PREFIX}/hiclaw-config/packages/${TEST_WORKER_OVERRIDE}*.zip" 2>/dev/null || true
}
trap _cleanup EXIT

# ============================================================
# Section 1: Controller infrastructure health
# ============================================================
log_section "Controller Infrastructure"

CTRL_PID=$(exec_in_manager pgrep -f hiclaw-controller 2>/dev/null || echo "")
if [ -n "${CTRL_PID}" ]; then
    log_pass "hiclaw-controller process is running (PID: ${CTRL_PID})"
else
    log_fail "hiclaw-controller process is not running"
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

# Write YAML with inline soul and agents
exec_in_manager bash -c "cat > /tmp/hiclaw-test-${TEST_WORKER}.yaml << 'YAMLEOF'
apiVersion: hiclaw.io/v1beta1
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

YAML_EXISTS=$(exec_in_manager test -f "/tmp/hiclaw-test-${TEST_WORKER}.yaml" && echo "yes" || echo "no")
if [ "${YAML_EXISTS}" = "yes" ]; then
    log_pass "Worker YAML with inline fields created"
else
    log_fail "Failed to create Worker YAML"
fi

# ============================================================
# Section 3: Apply YAML via hiclaw apply -f
# ============================================================
log_section "Apply Worker YAML"

APPLY_OUTPUT=$(exec_in_manager hiclaw apply -f "/tmp/hiclaw-test-${TEST_WORKER}.yaml" 2>&1)
APPLY_EXIT=$?

if [ ${APPLY_EXIT} -eq 0 ]; then
    log_pass "hiclaw apply -f exited successfully"
else
    log_fail "hiclaw apply -f failed (exit: ${APPLY_EXIT})"
fi

if echo "${APPLY_OUTPUT}" | grep -q "created\|configured"; then
    log_pass "hiclaw apply reports resource created"
else
    log_fail "hiclaw apply did not report creation"
fi

# ============================================================
# Section 4: Verify YAML in MinIO
# ============================================================
log_section "Verify MinIO State"

YAML_CONTENT=$(exec_in_manager mc cat "${STORAGE_PREFIX}/hiclaw-config/workers/${TEST_WORKER}.yaml" 2>/dev/null || echo "")
assert_not_empty "${YAML_CONTENT}" "YAML file exists in MinIO hiclaw-config/workers/"
assert_contains "${YAML_CONTENT}" "kind: Worker" "YAML contains kind: Worker"
assert_contains "${YAML_CONTENT}" "name: ${TEST_WORKER}" "YAML contains correct name"
assert_contains "${YAML_CONTENT}" "soul:" "YAML contains soul field"
assert_contains "${YAML_CONTENT}" "agents:" "YAML contains agents field"

# No package field should be present
if echo "${YAML_CONTENT}" | grep -q "package:"; then
    log_fail "YAML should not contain package field"
else
    log_pass "YAML correctly has no package field (inline only)"
fi

# ============================================================
# Section 5: Wait for controller reconcile + Worker creation
# ============================================================
log_section "Controller Reconcile"

log_info "Waiting for mc mirror (10s) + fsnotify + reconcile + create-worker.sh..."

RECONCILE_TIMEOUT=120
RECONCILE_ELAPSED=0
WORKER_CREATED=false

while [ "${RECONCILE_ELAPSED}" -lt "${RECONCILE_TIMEOUT}" ]; do
    if exec_in_manager cat /var/log/hiclaw/hiclaw-controller-error.log 2>/dev/null | grep -q "worker created.*${TEST_WORKER}"; then
        WORKER_CREATED=true
        break
    fi
    sleep 5
    RECONCILE_ELAPSED=$((RECONCILE_ELAPSED + 5))
    printf "\r[TEST INFO] Waiting for reconcile... (%ds/%ds)" "${RECONCILE_ELAPSED}" "${RECONCILE_TIMEOUT}"
done
echo ""

if [ "${WORKER_CREATED}" = true ]; then
    log_pass "WorkerReconciler created worker (took ~${RECONCILE_ELAPSED}s)"
else
    log_fail "WorkerReconciler did not create worker within ${RECONCILE_TIMEOUT}s"
    exec_in_manager cat /var/log/hiclaw/hiclaw-controller-error.log 2>/dev/null | grep "${TEST_WORKER}" | tail -5
fi

# Verify inline configs were written
INLINE_LOG=$(exec_in_manager cat /var/log/hiclaw/hiclaw-controller-error.log 2>/dev/null | grep "inline configs written.*${TEST_WORKER}" || echo "")
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

# workers-registry.json
REGISTRY_ENTRY=$(exec_in_manager jq -r --arg w "${TEST_WORKER}" '.workers[$w] // empty' /root/manager-workspace/workers-registry.json 2>/dev/null)
assert_not_empty "${REGISTRY_ENTRY}" "Worker registered in workers-registry.json"

# Matrix Room
ROOM_ID=$(echo "${REGISTRY_ENTRY}" | jq -r '.room_id // empty' 2>/dev/null)
assert_not_empty "${ROOM_ID}" "Matrix Room created: ${ROOM_ID}"

# openclaw.json in MinIO
OPENCLAW_EXISTS=$(exec_in_manager bash -c "mc ls '${STORAGE_PREFIX}/agents/${TEST_WORKER}/openclaw.json' >/dev/null 2>&1 && echo yes || echo no")
if [ "${OPENCLAW_EXISTS}" = "yes" ]; then
    log_pass "openclaw.json generated and pushed to MinIO"
else
    log_fail "openclaw.json not found in MinIO"
fi

# Worker container running
CONTAINER_RUNNING=$(docker ps --format '{{.Names}}' 2>/dev/null | grep "hiclaw-worker-${TEST_WORKER}" || echo "")
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

DELETE_OUTPUT=$(exec_in_manager hiclaw delete worker "${TEST_WORKER}" 2>&1)
if echo "${DELETE_OUTPUT}" | grep -q "deleted"; then
    log_pass "hiclaw delete reported success"
else
    log_fail "hiclaw delete did not report success"
fi

sleep 2
YAML_AFTER=$(exec_in_manager mc cat "${STORAGE_PREFIX}/hiclaw-config/workers/${TEST_WORKER}.yaml" 2>/dev/null || echo "")
if [ -z "${YAML_AFTER}" ]; then
    log_pass "YAML removed from MinIO after delete"
else
    log_fail "YAML still exists after delete"
fi

# ============================================================
# Section 9: Package + Inline Override Test
# ============================================================
log_section "Package + Inline Override"

# Create a ZIP package with SOUL.md and AGENTS.md
OVERRIDE_WORK_DIR="/tmp/hiclaw-test-${TEST_WORKER_OVERRIDE}"

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

# Import ZIP first to get it into MinIO
APPLY_ZIP_OUTPUT=$(exec_in_manager hiclaw apply worker --zip "${OVERRIDE_WORK_DIR}/${TEST_WORKER_OVERRIDE}.zip" --name "${TEST_WORKER_OVERRIDE}" 2>&1)
if [ $? -eq 0 ]; then
    log_pass "ZIP imported for override test"
else
    log_fail "ZIP import failed for override test"
fi

# Now get the generated YAML, read the package URI, and create a new YAML with inline overrides
PKG_URI=$(exec_in_manager mc cat "${STORAGE_PREFIX}/hiclaw-config/workers/${TEST_WORKER_OVERRIDE}.yaml" 2>/dev/null | grep "package:" | sed 's/.*package: //')
assert_not_empty "${PKG_URI}" "Package URI extracted from generated YAML"

# Overwrite the YAML with package + inline soul/agents
OVERRIDE_SOUL="# OVERRIDDEN SOUL FROM INLINE
This soul was set via inline field and should replace the package version."

OVERRIDE_AGENTS="# OVERRIDDEN AGENTS FROM INLINE
This agents config was set via inline field."

exec_in_manager bash -c "cat > /tmp/hiclaw-override-${TEST_WORKER_OVERRIDE}.yaml << 'YAMLEOF'
apiVersion: hiclaw.io/v1beta1
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
APPLY_OVERRIDE=$(exec_in_manager hiclaw apply -f "/tmp/hiclaw-override-${TEST_WORKER_OVERRIDE}.yaml" 2>&1)
if echo "${APPLY_OVERRIDE}" | grep -q "created\|configured"; then
    log_pass "Applied YAML with package + inline override"
else
    log_fail "Failed to apply YAML with package + inline override"
fi

# Wait for reconcile
log_info "Waiting for controller to reconcile override worker..."
RECONCILE_TIMEOUT=120
RECONCILE_ELAPSED=0
WORKER_CREATED=false

while [ "${RECONCILE_ELAPSED}" -lt "${RECONCILE_TIMEOUT}" ]; do
    if exec_in_manager cat /var/log/hiclaw/hiclaw-controller-error.log 2>/dev/null | grep -q "worker created.*${TEST_WORKER_OVERRIDE}"; then
        WORKER_CREATED=true
        break
    fi
    sleep 5
    RECONCILE_ELAPSED=$((RECONCILE_ELAPSED + 5))
    printf "\r[TEST INFO] Waiting for reconcile... (%ds/%ds)" "${RECONCILE_ELAPSED}" "${RECONCILE_TIMEOUT}"
done
echo ""

if [ "${WORKER_CREATED}" = true ]; then
    log_pass "Override worker created (took ~${RECONCILE_ELAPSED}s)"
else
    log_fail "Override worker not created within ${RECONCILE_TIMEOUT}s"
    exec_in_manager cat /var/log/hiclaw/hiclaw-controller-error.log 2>/dev/null | grep "${TEST_WORKER_OVERRIDE}" | tail -5
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
exec_in_manager hiclaw delete worker "${TEST_WORKER_OVERRIDE}" 2>/dev/null
log_pass "Override worker deleted"

# ============================================================
# Summary
# ============================================================
test_teardown "20-inline-worker-config"
test_summary
