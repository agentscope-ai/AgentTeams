#!/bin/bash
# test-06-multi-worker.sh - Case 6: Create Bob, assign collaborative task
# Verifies: Second Worker creation, both Workers collaborate via shared MinIO files

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/test-helpers.sh"
source "${SCRIPT_DIR}/lib/matrix-client.sh"
source "${SCRIPT_DIR}/lib/higress-client.sh"
source "${SCRIPT_DIR}/lib/minio-client.sh"
source "${SCRIPT_DIR}/lib/agent-metrics.sh"

test_setup "06-multi-worker"

if ! require_llm_key; then
    test_teardown "06-multi-worker"
    test_summary
    exit 0
fi

ADMIN_LOGIN=$(matrix_login "${TEST_ADMIN_USER}" "${TEST_ADMIN_PASSWORD}")
ADMIN_TOKEN=$(echo "${ADMIN_LOGIN}" | jq -r '.access_token')

MANAGER_USER="@manager:${TEST_MATRIX_DOMAIN}"

log_section "Create Worker Bob"

DM_ROOM=$(matrix_find_dm_room "${ADMIN_TOKEN}" "${MANAGER_USER}" 2>/dev/null || true)
assert_not_empty "${DM_ROOM}" "DM room with Manager found"

# Wait for Manager Agent to be fully ready (OpenClaw gateway + joined DM room)
wait_for_manager_agent_ready 300 "${DM_ROOM}" "${ADMIN_TOKEN}" || {
    log_fail "Manager Agent not ready in time"
    test_teardown "06-multi-worker"
    test_summary
    exit 1
}

# Alice is running from previous tests; bob will be created below (offset=0 is correct for new workers)
wait_for_worker_container "alice" 60
METRICS_BASELINE=$(snapshot_baseline "alice" "bob")
matrix_send_message "${ADMIN_TOKEN}" "${DM_ROOM}" \
    "Create a new Worker for backend development. The worker's name (username) must be exactly 'bob'. He should have access to GitHub MCP."

log_info "Waiting for Manager to create Worker Bob..."
REPLY=$(matrix_wait_for_reply "${ADMIN_TOKEN}" "${DM_ROOM}" "@manager" 180 \
    "${ADMIN_TOKEN}" "${DM_ROOM}" "Please check if the request has been processed.")

assert_not_empty "${REPLY}" "Manager replied to create bob request"
assert_contains_i "${REPLY}" "bob" "Reply mentions worker name 'bob'"

# Verify Bob's infrastructure (may take a moment for LLM to complete setup)
sleep 30
higress_login "${TEST_ADMIN_USER}" "${TEST_ADMIN_PASSWORD}" > /dev/null
CONSUMERS=$(higress_get_consumers)
assert_contains_i "${CONSUMERS}" "worker-bob" "Higress consumer 'worker-bob' exists"

minio_setup
minio_wait_for_file "agents/bob/SOUL.md" 60
BOB_EXISTS=$?
assert_eq "0" "${BOB_EXISTS}" "Worker Bob SOUL.md exists in MinIO"

log_section "Assign Collaborative Task"

matrix_send_message "${ADMIN_TOKEN}" "${DM_ROOM}" \
    "I need Alice and Bob to collaborate on a task: Build a simple REST API. Alice handles the frontend HTML page, Bob handles the backend API endpoint. They should coordinate via shared files."

log_info "Waiting for Manager to acknowledge task..."
REPLY=$(matrix_wait_for_reply "${ADMIN_TOKEN}" "${DM_ROOM}" "@manager" 300)

if [ -z "${REPLY}" ]; then
    log_info "No DM reply yet, checking if Manager created a Project Room instead..."
    MANAGER_TOKEN=$(docker exec "${TEST_MANAGER_CONTAINER}" \
        jq -r '.channels.matrix.accessToken // empty' /root/manager-workspace/openclaw.json 2>/dev/null || true)
    if [ -n "${MANAGER_TOKEN}" ]; then
        PROJECT_ROOM=$(matrix_find_room_by_name "${MANAGER_TOKEN}" "Project:" 2>/dev/null || true)
        if [ -n "${PROJECT_ROOM}" ]; then
            log_info "Project room found: ${PROJECT_ROOM}, checking for task assignment messages..."
            REPLY=$(matrix_read_messages "${MANAGER_TOKEN}" "${PROJECT_ROOM}" 20 2>/dev/null | \
                jq -r --arg u "@manager" \
                '[.chunk[] | select(.sender | startswith($u)) | .content.body] | first // empty' 2>/dev/null || true)
        fi
    fi
fi

assert_not_empty "${REPLY}" "Manager acknowledged collaborative task"

log_section "Wait for Task Completion"

# Get Manager token if not already available
if [ -z "${MANAGER_TOKEN:-}" ]; then
    log_info "Waiting for Manager token (timeout: 120s)..."
    DEADLINE=$(( $(date +%s) + 120 ))
    while [ "$(date +%s)" -lt "${DEADLINE}" ]; do
        MANAGER_TOKEN=$(docker exec "${TEST_MANAGER_CONTAINER}" \
            jq -r '.channels.matrix.accessToken // empty' /root/manager-workspace/openclaw.json 2>/dev/null || true)
        [ -n "${MANAGER_TOKEN}" ] && break
        sleep 5
    done
fi

# Find project room if not already found
if [ -z "${PROJECT_ROOM:-}" ] && [ -n "${MANAGER_TOKEN:-}" ]; then
    log_info "Waiting for project room to be created (timeout: 300s)..."
    DEADLINE=$(( $(date +%s) + 300 ))
    while [ "$(date +%s)" -lt "${DEADLINE}" ]; do
        PROJECT_ROOM=$(matrix_find_room_by_name "${MANAGER_TOKEN}" "Project:" 2>/dev/null || true)
        [ -n "${PROJECT_ROOM}" ] && break
        sleep 10
    done
fi

# Wait for completion in project room (with nudge via DM), or fall back to sleep
if [ -n "${PROJECT_ROOM:-}" ] && [ -n "${MANAGER_TOKEN:-}" ]; then
    log_info "Waiting for task completion in project room (timeout: 1800s)..."
    COMPLETION_MSG=$(matrix_wait_for_message_containing "${MANAGER_TOKEN}" "${PROJECT_ROOM}" "@manager" \
        "complete\|done\|finished\|已完成\|完成" 1800 \
        "${ADMIN_TOKEN}" "${DM_ROOM}" \
        "Please check the project room and continue coordinating the collaborative task. If any worker message was missed, please follow up." \
        2>/dev/null || true)
    if [ -n "${COMPLETION_MSG}" ]; then
        log_pass "Task completed — Manager's message: $(echo "${COMPLETION_MSG}" | head -c 200)"
    else
        log_info "No completion message detected within timeout, proceeding to verify artifacts"
    fi
else
    log_info "No project room found, waiting 60s for task processing..."
    sleep 60
fi

log_section "Verify Shared Coordination"
TASKS=$(minio_list_dir "shared/tasks/" 2>/dev/null || echo "")
log_info "Shared tasks directory: ${TASKS}"

log_section "Collect Metrics"
wait_for_worker_session_stable "alice" 5 120
wait_for_worker_session_stable "bob" 5 120
wait_for_session_stable 5 60
PREV_METRICS=$(cat "${TEST_OUTPUT_DIR}/metrics-06-multi-worker.json" 2>/dev/null || true)
METRICS=$(collect_delta_metrics "06-multi-worker" "$METRICS_BASELINE" "alice" "bob")
print_metrics_report "$METRICS" "$PREV_METRICS"
save_metrics_file "$METRICS" "06-multi-worker"

test_teardown "06-multi-worker"
test_summary
