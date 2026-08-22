#!/bin/bash
# test-15-import-worker-zip.sh - Case 15: Full Worker import via ZIP + reconcile + messaging
#
# End-to-end test covering the complete declarative import flow:
#   1. Create a test ZIP package (manifest.json + SOUL.md + custom skill)
#   2. hiclaw apply worker --zip uploads ZIP + YAML to MinIO
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
STORAGE_PREFIX="hiclaw/hiclaw-storage"

# ---- Cleanup handler (only clean up on success) ----
_cleanup() {
    # Check if all tests passed before cleaning up
    if [ "${TESTS_FAILED}" -gt 0 ]; then
        log_info "Tests failed — preserving worker ${TEST_WORKER} for debugging"
        log_info "  Container: hiclaw-worker-${TEST_WORKER}"
        log_info "  MinIO YAML: ${STORAGE_PREFIX}/hiclaw-config/workers/${TEST_WORKER}.yaml"
        log_info "  Agent dir: ${STORAGE_PREFIX}/agents/${TEST_WORKER}/"
        return
    fi
    log_info "All tests passed — cleaning up test worker: ${TEST_WORKER}"
    exec_in_manager hiclaw delete worker "${TEST_WORKER}" 2>/dev/null || true
    exec_in_manager mc rm "${STORAGE_PREFIX}/hiclaw-config/packages/${TEST_WORKER}*.zip" 2>/dev/null || true
    sleep 5
    docker rm -f "hiclaw-worker-${TEST_WORKER}" 2>/dev/null || true
    exec_in_manager rm -rf "/root/hiclaw-fs/agents/${TEST_WORKER}" 2>/dev/null || true
    exec_in_manager rm -rf "/tmp/hiclaw-test-${TEST_WORKER}" 2>/dev/null || true
    exec_in_manager mc rm -r --force "${STORAGE_PREFIX}/agents/${TEST_WORKER}/" 2>/dev/null || true
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

KAPI_PID=$(exec_in_manager pgrep -f kube-apiserver 2>/dev/null || echo "")
if [ -n "${KAPI_PID}" ]; then
    log_pass "kube-apiserver process is running"
else
    log_fail "kube-apiserver process is not running"
fi

HICLAW_HELP=$(exec_in_manager hiclaw --help 2>&1 | head -1 || echo "")
if echo "${HICLAW_HELP}" | grep -qi "hiclaw\|declarative\|resource"; then
    log_pass "hiclaw CLI is available"
else
    log_fail "hiclaw CLI is not available"
fi

# ============================================================
# Section 2: Create test ZIP package
# ============================================================
log_section "Create Test ZIP Package"

WORK_DIR="/tmp/hiclaw-test-${TEST_WORKER}"

exec_in_manager bash -c "
    mkdir -p ${WORK_DIR}/package/config ${WORK_DIR}/package/skills/test-skill

    cat > ${WORK_DIR}/package/manifest.json <<MANIFEST
{
  \"type\": \"worker\",
  \"version\": 1,
  \"worker\": {
    \"suggested_name\": \"${TEST_WORKER}\",
    \"model\": \"qwen3.5-plus\"
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

# ============================================================
# Section 3: Import via hiclaw apply worker --zip
# ============================================================
log_section "Import Worker via hiclaw apply worker --zip"

APPLY_OUTPUT=$(exec_in_manager hiclaw apply worker --zip "${WORK_DIR}/${TEST_WORKER}.zip" --name "${TEST_WORKER}" 2>&1)
APPLY_EXIT=$?

if [ ${APPLY_EXIT} -eq 0 ]; then
    log_pass "hiclaw apply worker --zip exited successfully"
else
    log_fail "hiclaw apply worker --zip failed (exit: ${APPLY_EXIT})"
fi

if echo "${APPLY_OUTPUT}" | grep -q "created\|applied\|configured"; then
    log_pass "hiclaw apply worker --zip reports resource created"
else
    log_fail "hiclaw apply worker --zip did not report creation"
fi

# ============================================================
# Section 4: Verify YAML + ZIP in MinIO
# ============================================================
log_section "Verify MinIO State"

YAML_CONTENT=$(exec_in_manager mc cat "${STORAGE_PREFIX}/hiclaw-config/workers/${TEST_WORKER}.yaml" 2>/dev/null || echo "")
assert_not_empty "${YAML_CONTENT}" "YAML file exists in MinIO hiclaw-config/workers/"
assert_contains "${YAML_CONTENT}" "kind: Worker" "YAML contains kind: Worker"
assert_contains "${YAML_CONTENT}" "name: ${TEST_WORKER}" "YAML contains correct name"

PKG_EXISTS=$(exec_in_manager bash -c "mc ls '${STORAGE_PREFIX}/hiclaw-config/packages/${TEST_WORKER}.zip' >/dev/null 2>&1 && echo yes || echo no")
if [ "${PKG_EXISTS}" = "yes" ]; then
    log_pass "ZIP package uploaded to MinIO"
else
    log_fail "ZIP package not found in MinIO"
fi

# ============================================================
# Section 5: Verify hiclaw get (CLI reads from MinIO)
# ============================================================
log_section "Verify hiclaw get"

GET_LIST=$(exec_in_manager hiclaw get workers 2>&1)
assert_contains "${GET_LIST}" "${TEST_WORKER}" "Worker visible in 'hiclaw get workers'"

# ============================================================
# Section 6: Idempotency
# ============================================================
log_section "Idempotency"

REIMPORT_OUTPUT=$(exec_in_manager hiclaw apply worker --zip "${WORK_DIR}/${TEST_WORKER}.zip" --name "${TEST_WORKER}" 2>&1)
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

# Verify file watcher detected the change
SYNC_LOG=$(exec_in_manager cat /var/log/hiclaw/hiclaw-controller-error.log 2>/dev/null | grep "syncing resource.*${TEST_WORKER}" || echo "")
assert_not_empty "${SYNC_LOG}" "File watcher detected and synced resource"

# ============================================================
# Section 8: Verify Worker infrastructure
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
# Section 9: Admin sends message to Worker, Worker replies
# ============================================================
log_section "Admin ↔ Worker Messaging"

# Skip if no LLM key (Worker needs LLM to reply)
if ! require_llm_key; then
    log_info "Skipping messaging test (no LLM API key)"
else
    # Login as admin
    ADMIN_LOGIN=$(matrix_login "${TEST_ADMIN_USER}" "${TEST_ADMIN_PASSWORD}" 2>/dev/null)
    ADMIN_TOKEN=$(echo "${ADMIN_LOGIN}" | jq -r '.access_token // empty')
    assert_not_empty "${ADMIN_TOKEN}" "Admin Matrix login successful"

    if [ -n "${ADMIN_TOKEN}" ] && [ "${ADMIN_TOKEN}" != "null" ] && [ -n "${ROOM_ID}" ]; then
        # Wait for Worker to join the room (not just container running, but Matrix sync active)
        ROOM_ENC="$(_encode_room_id "${ROOM_ID}")"
        WORKER_MATRIX_ID="@${TEST_WORKER}:${TEST_MATRIX_DOMAIN}"

        # Admin must join the Worker Room first (create-worker.sh only invites)
        exec_in_manager curl -s -X POST \
            "${TEST_MATRIX_DIRECT_URL}/_matrix/client/v3/rooms/${ROOM_ENC}/join" \
            -H "Authorization: Bearer ${ADMIN_TOKEN}" \
            -H 'Content-Type: application/json' -d '{}' 2>/dev/null | jq -r '.room_id // empty' > /dev/null
        log_info "Admin joined Worker Room"

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

        # Send message with @mention (Worker requires m.mentions to wake up)
        MESSAGE_BODY="${WORKER_MATRIX_ID} Hello! Please reply with a short greeting."
        TXN_ID="$(date +%s%N)"
        ROOM_ENC="$(_encode_room_id "${ROOM_ID}")"

        SEND_RESULT=$(exec_in_manager curl -s -X PUT \
            "${TEST_MATRIX_DIRECT_URL}/_matrix/client/v3/rooms/${ROOM_ENC}/send/m.room.message/${TXN_ID}" \
            -H "Authorization: Bearer ${ADMIN_TOKEN}" \
            -H 'Content-Type: application/json' \
            -d '{
                "msgtype": "m.text",
                "body": "'"${MESSAGE_BODY}"'",
                "m.mentions": {
                    "user_ids": ["'"${WORKER_MATRIX_ID}"'"]
                }
            }' 2>&1)

        SEND_EVENT=$(echo "${SEND_RESULT}" | jq -r '.event_id // empty' 2>/dev/null)
        if [ -n "${SEND_EVENT}" ] && [ "${SEND_EVENT}" != "null" ]; then
            log_pass "Admin sent message to Worker Room (event: ${SEND_EVENT})"
        else
            log_fail "Failed to send message to Worker Room"
            log_info "Send result: ${SEND_RESULT}"
        fi

        # Wait for Worker reply
        if [ -n "${SEND_EVENT}" ]; then
            log_info "Waiting for Worker reply (timeout: 120s)..."
            REPLY=$(matrix_wait_for_reply "${ADMIN_TOKEN}" "${ROOM_ID}" "@${TEST_WORKER}" 120)

            if [ -n "${REPLY}" ]; then
                log_pass "Worker replied: $(echo "${REPLY}" | head -1 | cut -c1-80)..."
            else
                log_fail "Worker did not reply within 120s"
                # Show recent messages for debugging
                log_info "Recent messages in room:"
                matrix_read_messages "${ADMIN_TOKEN}" "${ROOM_ID}" 5 2>/dev/null | \
                    jq -r '.chunk[] | "\(.sender): \(.content.body // "(no body)")"' 2>/dev/null | head -5
            fi
        fi
    else
        log_info "Skipping messaging (no admin token or room ID)"
    fi
fi

# ============================================================
# Section 10: Delete and verify cleanup
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
# Summary
# ============================================================
test_teardown "15-import-worker-zip"
test_summary
