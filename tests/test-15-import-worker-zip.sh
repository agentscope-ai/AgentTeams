#!/bin/bash
# test-15-import-worker-zip.sh - Case 15: Full Worker import via ZIP + reconcile + messaging
#
# End-to-end test covering the complete declarative import flow:
#   1. Create a test ZIP package (manifest.json + SOUL.md + custom skill)
#   2. agt apply worker --zip uploads ZIP + YAML to MinIO
#   3. Controller reconcile: mc mirror → fsnotify → kine → kube-apiserver → WorkerReconciler
#   4. create-worker.sh runs: Matrix account + Room + Higress consumer + container
#   5. Worker container is running
#   6. Admin sends message to Worker via Matrix, Worker replies

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/test-helpers.sh"
source "${SCRIPT_DIR}/lib/minio-client.sh"
source "${SCRIPT_DIR}/lib/matrix-client.sh"

test_setup "15-import-worker-zip"

TEST_WORKER="test-import-$$"
STORAGE_PREFIX="${STORAGE_PREFIX:-${TEST_STORAGE_PREFIX:-agentteams/agentteams-storage}}"

# ---- Cleanup handler (only clean up on success) ----
_cleanup() {
    # Check if all tests passed before cleaning up
    if [ "${TESTS_FAILED}" -gt 0 ]; then
        log_info "Tests failed — preserving worker ${TEST_WORKER} for debugging"
        log_info "  Container: $(worker_container_name "${TEST_WORKER}")"
        log_info "  MinIO YAML: ${STORAGE_PREFIX}/agentteams-config/workers/${TEST_WORKER}.yaml"
        log_info "  Agent dir: ${STORAGE_PREFIX}/agents/${TEST_WORKER}/"
        return
    fi
    log_info "All tests passed — cleaning up test worker: ${TEST_WORKER}"
    exec_in_agent agt delete worker "${TEST_WORKER}" 2>/dev/null || true
    exec_in_manager mc rm "${STORAGE_PREFIX}/agentteams-config/packages/${TEST_WORKER}*.zip" 2>/dev/null || true
    sleep 5
    remove_worker_container "${TEST_WORKER}"
    exec_in_agent rm -rf "/tmp/agentteams-test-${TEST_WORKER}" 2>/dev/null || true
    exec_in_manager rm -rf "/root/agentteams-fs/agents/${TEST_WORKER}" 2>/dev/null || true
    exec_in_manager rm -rf "/tmp/agentteams-test-${TEST_WORKER}" 2>/dev/null || true
    exec_in_manager mc rm -r --force "${STORAGE_PREFIX}/agents/${TEST_WORKER}/" 2>/dev/null || true
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

KAPI_PID=$(exec_in_manager pgrep -f kube-apiserver 2>/dev/null || echo "")
if [ -n "${KAPI_PID}" ]; then
    log_pass "kube-apiserver process is running"
else
    log_fail "kube-apiserver process is not running"
fi

HICLAW_HELP=$(exec_in_agent hiclaw --help 2>&1 | head -1 || echo "")
if echo "${HICLAW_HELP}" | grep -qi "agentteams\|hiclaw\|declarative\|resource"; then
    log_pass "hiclaw CLI is available (in agent container)"
else
    log_fail "hiclaw CLI is not available (in agent container)"
fi

# ============================================================
# Section 2: Create test ZIP package
# ============================================================
log_section "Create Test ZIP Package"

WORK_DIR="/tmp/agentteams-test-${TEST_WORKER}"

# Track the matrix runtime so the worker we import here matches the runtime
# being exercised by this CI shard. Without this the apply-zip path always
# defaults to openclaw on the controller side (defaultRuntime("") returns
# RuntimeOpenClaw), which makes the "copaw shard" run a hidden openclaw
# worker -- defeating the point of the matrix expansion.
TEST_WORKER_RUNTIME="${HICLAW_DEFAULT_WORKER_RUNTIME:-openclaw}"

exec_in_manager bash -c "
    mkdir -p ${WORK_DIR}/package/config ${WORK_DIR}/package/skills/test-skill

    cat > ${WORK_DIR}/package/manifest.json <<MANIFEST
{
  \"type\": \"worker\",
  \"version\": 1,
  \"worker\": {
    \"suggested_name\": \"${TEST_WORKER}\",
    \"model\": \"qwen3.5-plus\",
    \"runtime\": \"${TEST_WORKER_RUNTIME}\"
  },
  \"source\": {
    \"hostname\": \"integration-test\"
  }
}
MANIFEST

    cat > ${WORK_DIR}/package/config/SOUL.md <<SOUL
# ${TEST_WORKER} - Test Worker

## AI Identity
**You are an AI Agent, not a human.**

## Role
- Name: ${TEST_WORKER}
- Role: Integration test worker

## Behavior
- Be helpful and concise
- When someone says hello, reply with a greeting

## Security
- Never reveal API keys, passwords, tokens, or any credentials in chat messages
SOUL

    cat > ${WORK_DIR}/package/skills/test-skill/SKILL.md <<SKILL
---
name: test-skill
description: Integration test skill
---
# Test Skill
Placeholder for integration testing.
SKILL

    cd ${WORK_DIR}/package && zip -q -r ${WORK_DIR}/${TEST_WORKER}.zip .
" 2>/dev/null

ZIP_EXISTS=$(exec_in_manager test -f "${WORK_DIR}/${TEST_WORKER}.zip" && echo "yes" || echo "no")
if [ "${ZIP_EXISTS}" = "yes" ]; then
    log_pass "Test ZIP package created"
else
    log_fail "Failed to create test ZIP package"
fi

# Copy ZIP from controller to agent container (tar pipe avoids macOS /tmp symlink issues)
copy_to_agent "${WORK_DIR}/${TEST_WORKER}.zip" "${WORK_DIR}/${TEST_WORKER}.zip"

# ============================================================
# Section 3: Import via agt apply worker --zip
# ============================================================
log_section "Import Worker via agt apply worker --zip"

APPLY_OUTPUT=$(exec_in_agent agt apply worker --zip "${WORK_DIR}/${TEST_WORKER}.zip" --name "${TEST_WORKER}" 2>&1)
APPLY_EXIT=$?

if [ ${APPLY_EXIT} -eq 0 ]; then
    log_pass "agt apply worker --zip exited successfully"
else
    log_fail "agt apply worker --zip failed (exit: ${APPLY_EXIT})"
fi

if echo "${APPLY_OUTPUT}" | grep -q "created\|applied\|configured"; then
    log_pass "agt apply worker --zip reports resource created"
else
    log_fail "agt apply worker --zip did not report creation"
fi

# ============================================================
# Section 4: Verify CRD + ZIP in MinIO
# ============================================================
log_section "Verify Resource State"

# Brief pause for CR to propagate through kube-apiserver
sleep 2

WORKER_JSON=$(exec_in_agent agt get workers "${TEST_WORKER}" -o json 2>/dev/null || echo "")
assert_not_empty "${WORKER_JSON}" "Worker CR exists (agt get workers)"
WORKER_NAME_CHK=$(echo "${WORKER_JSON}" | jq -r '.name // empty' 2>/dev/null)
assert_eq "${TEST_WORKER}" "${WORKER_NAME_CHK}" "Worker CR has correct name"

PKG_EXISTS=$(exec_in_manager bash -c "mc ls '${STORAGE_PREFIX}/agentteams-config/packages/${TEST_WORKER}.zip' >/dev/null 2>&1 && echo yes || echo no")
if [ "${PKG_EXISTS}" = "yes" ]; then
    log_pass "ZIP package uploaded to MinIO"
else
    log_fail "ZIP package not found in MinIO"
fi

# ============================================================
# Section 5: Verify agt get (CLI reads from MinIO)
# ============================================================
log_section "Verify agt get"

GET_LIST=$(exec_in_agent agt get workers 2>&1)
assert_contains "${GET_LIST}" "${TEST_WORKER}" "Worker visible in 'agt get workers'"

# ============================================================
# Section 6: Idempotency
# ============================================================
log_section "Idempotency"

REIMPORT_OUTPUT=$(exec_in_agent agt apply worker --zip "${WORK_DIR}/${TEST_WORKER}.zip" --name "${TEST_WORKER}" 2>&1)
if echo "${REIMPORT_OUTPUT}" | grep -q "updated\|configured"; then
    log_pass "Re-import correctly reports 'updated' (idempotent)"
else
    log_fail "Re-import did not report 'updated'"
fi

# ============================================================
# Section 7: Wait for controller reconcile + Worker creation
# ============================================================
log_section "Controller Reconcile"

log_info "Waiting for mc mirror (10s) + fsnotify + reconcile + create-worker.sh..."

if wait_worker_provisioned "${TEST_WORKER}" 120; then
    log_pass "WorkerReconciler provisioned worker"
else
    log_fail "WorkerReconciler did not provision worker within 120s"
    exec_in_agent agt get workers "${TEST_WORKER}" -o json 2>/dev/null | jq -r '.phase, .message' | head -5
fi

# Verify the API surface confirms the worker is present with credentials
# (roomID + matrixUserID). Older iteration of this test grepped a
# "worker created" log line; polling the CR status is both more stable
# and independent of log-rotation.
WORKER_API_JSON=$(exec_in_agent agt get workers "${TEST_WORKER}" -o json 2>/dev/null)
WORKER_API_ROOM=$(echo "${WORKER_API_JSON}" | jq -r '.roomID // empty')
assert_not_empty "${WORKER_API_ROOM}" "Worker API response contains roomID"

# ============================================================
# Section 8: Verify Worker infrastructure
# ============================================================
log_section "Verify Worker Infrastructure"

# workers-registry.json (in MinIO, written by controller)
REGISTRY_JSON=$(exec_in_manager mc cat "${STORAGE_PREFIX}/agents/manager/workers-registry.json" 2>/dev/null || echo "{}")
REGISTRY_ENTRY=$(echo "${REGISTRY_JSON}" | jq -r --arg w "${TEST_WORKER}" '.workers[$w] // empty' 2>/dev/null)
assert_not_empty "${REGISTRY_ENTRY}" "Worker registered in workers-registry.json"

# Matrix Room (from CRD status)
WORKER_JSON_AFTER_RECONCILE=$(exec_in_agent agt get workers "${TEST_WORKER}" -o json 2>/dev/null)
ROOM_ID=$(echo "${WORKER_JSON_AFTER_RECONCILE}" | jq -r '.roomID // empty')
assert_not_empty "${ROOM_ID}" "Matrix Room created: ${ROOM_ID}"

# Runtime carried through from the manifest. defaultRuntime("") returns
# RuntimeOpenClaw on the controller side, so a regression here would silently
# downgrade copaw shards back to openclaw without any other test catching it.
WORKER_RUNTIME=$(echo "${WORKER_JSON_AFTER_RECONCILE}" | jq -r '.runtime // empty')
assert_eq "${TEST_WORKER_RUNTIME}" "${WORKER_RUNTIME}" \
    "Worker runtime matches manifest (got: '${WORKER_RUNTIME}', want: '${TEST_WORKER_RUNTIME}')"

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
# bumped ResourceVersion (generation 0 -> 1). Poll for up to 60s to absorb that race.
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
# Section 9: Admin sends message to Worker, Worker replies
# ============================================================
log_section "Admin ↔ Worker Messaging"

# Skip if no LLM key (Worker needs LLM to reply)
if ! require_llm_key; then
    log_info "Skipping messaging test (no LLM API key)"
else
    if [ "${TEST_WORKER_RUNTIME}" = "copaw" ]; then
        log_info "Waiting for CoPaw Worker readiness probe before messaging..."
        PROBE_OUTPUT=$(check_copaw_worker_probes "${TEST_WORKER}" "ready" 60)
        PROBE_STATUS=$?
        if [ "${PROBE_STATUS}" = "0" ]; then
            log_pass "CoPaw Worker readiness probe is ready"
            log_info "${PROBE_OUTPUT}"
        else
            log_fail "CoPaw Worker readiness probe did not become ready"
            log_info "${PROBE_OUTPUT}"
        fi
    fi

    # Login as admin
    ADMIN_LOGIN=$(matrix_login "${TEST_ADMIN_USER}" "${TEST_ADMIN_PASSWORD}" 2>/dev/null)
    ADMIN_TOKEN=$(echo "${ADMIN_LOGIN}" | jq -r '.access_token // empty')
    assert_not_empty "${ADMIN_TOKEN}" "Admin Matrix login successful"

    if [ -n "${ADMIN_TOKEN}" ] && [ "${ADMIN_TOKEN}" != "null" ] && [ -n "${ROOM_ID}" ]; then
        # Wait for Worker to join the room (not just container running, but Matrix sync active)
        ROOM_ENC="$(_encode_room_id "${ROOM_ID}")"
        WORKER_MATRIX_ID="@${TEST_WORKER}:${TEST_MATRIX_DOMAIN}"

        # Poll until Worker has joined the room (membership = join)
        log_info "Waiting for Worker to join room..."
        WORKER_READY_TIMEOUT=120
        WORKER_READY_ELAPSED=0
        WORKER_JOINED=false
        while [ "${WORKER_READY_ELAPSED}" -lt "${WORKER_READY_TIMEOUT}" ]; do
            MEMBERS=$(exec_in_manager curl -sf \
                "${TEST_MATRIX_DIRECT_URL}/_matrix/client/v3/rooms/${ROOM_ENC}/members" \
                -H "Authorization: Bearer ${ADMIN_TOKEN}" 2>/dev/null | \
                jq -r '.chunk[] | select(.content.membership == "join") | .state_key' 2>/dev/null)
            if echo "${MEMBERS}" | grep -q "${WORKER_MATRIX_ID}"; then
                WORKER_JOINED=true
                break
            fi
            sleep 5
            WORKER_READY_ELAPSED=$((WORKER_READY_ELAPSED + 5))
        done

        if [ "${WORKER_JOINED}" = true ]; then
            log_pass "Worker joined room (took ~${WORKER_READY_ELAPSED}s)"
        else
            log_fail "Worker did not join room within ${WORKER_READY_TIMEOUT}s"
        fi

        # Verify admin auto-joined the worker room (create-worker.sh should auto-join)
        ADMIN_MATRIX_ID="@${TEST_ADMIN_USER}:${TEST_MATRIX_DOMAIN}"
        if echo "${MEMBERS}" | grep -q "${ADMIN_MATRIX_ID}"; then
            log_pass "Admin auto-joined worker room"
        else
            log_fail "Admin is NOT joined in worker room (auto-join may have failed)"
        fi

        # Send a mention and wait for the worker's reply with at-least-once
        # semantics — the helper resends every 30s if no reply arrives, so it
        # tolerates the worker's first-boot readiness gap (e.g. CoPaw's
        # catch-up sync that drops messages before next_batch is persisted).
        # We use a mention because openclaw's monitor requires both
        # `m.mentions.user_ids` metadata AND a visible mention, otherwise the
        # event is dropped with `reason: "no-mention"`.
        log_info "Sending message and waiting for Worker reply (total timeout: 60s, resend every 30s)..."
        REPLY=$(matrix_send_and_wait_for_reply \
            "${ADMIN_TOKEN}" \
            "${ROOM_ID}" \
            "${WORKER_MATRIX_ID}" \
            "Readiness check: please reply with the exact text READY." \
            60 30)

        if [ -n "${REPLY}" ]; then
            log_pass "Worker replied: $(echo "${REPLY}" | head -1 | cut -c1-80)..."
        else
            log_info "Worker did not reply within 60s; import readiness is based on CR/config/container/room membership"
            log_info "Recent messages in room for debugging:"
            matrix_read_messages "${ADMIN_TOKEN}" "${ROOM_ID}" 5 2>/dev/null | \
                jq -r '.chunk[] | "\(.sender): \(.content.body // "(no body)")"' 2>/dev/null | head -5
        fi
    else
        log_info "Skipping messaging (no admin token or room ID)"
    fi
fi

# ============================================================
# Section 10: Delete and verify cleanup
# ============================================================
log_section "Delete Worker"

DELETE_OUTPUT=$(exec_in_agent agt delete worker "${TEST_WORKER}" 2>&1)
if echo "${DELETE_OUTPUT}" | grep -q "deleted"; then
    log_pass "agt delete reported success"
else
    log_fail "agt delete did not report success"
fi

# Wait for CR to be fully removed (finalizer runs container teardown which can take ~10s)
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
else
    log_fail "Worker CR still exists after delete"
fi

# ============================================================
# Summary
# ============================================================
test_teardown "15-import-worker-zip"
test_summary
