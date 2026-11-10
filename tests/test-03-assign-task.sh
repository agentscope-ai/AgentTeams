#!/bin/bash
# test-03-assign-task.sh - Case 3: Assign task in Room, Worker completes
# Verifies: Manager relays task to Worker, task brief created in MinIO,
#           Worker completes and writes result

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/test-helpers.sh"
source "${SCRIPT_DIR}/lib/matrix-client.sh"
source "${SCRIPT_DIR}/lib/minio-client.sh"
source "${SCRIPT_DIR}/lib/agent-metrics.sh"

test_setup "03-assign-task"

if ! require_llm_key; then
    test_teardown "03-assign-task"
    test_summary
    exit 0
fi

ADMIN_LOGIN=$(matrix_login "${TEST_ADMIN_USER}" "${TEST_ADMIN_PASSWORD}")
ADMIN_TOKEN=$(echo "${ADMIN_LOGIN}" | jq -r '.access_token')

MANAGER_USER="@manager:${TEST_MATRIX_DOMAIN}"

log_section "Assign Task"

# Find Alice's Room (3-party room)
DM_ROOM=$(matrix_find_dm_room "${ADMIN_TOKEN}" "${MANAGER_USER}" 2>/dev/null || true)
assert_not_empty "${DM_ROOM}" "DM room with Manager found"

# Wait for Manager Agent to be fully ready (OpenClaw gateway + joined DM room)
wait_for_manager_agent_ready 300 "${DM_ROOM}" "${ADMIN_TOKEN}" || {
    log_fail "Manager Agent not ready in time"
    test_teardown "03-assign-task"
    test_summary
    exit 1
}

# Alice container should be running from test-02; wait to ensure it's up before snapshot
wait_for_worker_container "alice" 60
METRICS_BASELINE=$(snapshot_baseline "alice")
TASK_ID="test03-$(date +%s)-$$"
matrix_send_message "${ADMIN_TOKEN}" "${DM_ROOM}" \
    "Please assign Alice task ${TASK_ID}: Create shared/tasks/${TASK_ID}/README.md for a hello-world project. Include project name, description, usage instructions, and the exact marker ${TASK_ID}. Delegate the work to Alice and mention ${TASK_ID} in your acknowledgement."

log_info "Waiting for Manager to process task..."
REPLY=$(matrix_wait_for_reply_matching "${ADMIN_TOKEN}" "${DM_ROOM}" "@manager" "${TASK_ID}" 180)

assert_not_empty "${REPLY}" "Manager acknowledged task assignment"

log_section "Verify Task in MinIO"

minio_setup

log_info "Waiting for this task's actual README in MinIO..."
RESULT_PATH="shared/tasks/${TASK_ID}/README.md"
if minio_wait_for_file "${RESULT_PATH}" 300; then
    log_pass "Task README exists in shared storage"
    RESULT=$(minio_read_file "${RESULT_PATH}")
    assert_contains "${RESULT}" "${TASK_ID}" "Task README contains the current task marker"
else
    log_fail "Task README missing from shared storage"
fi

log_section "Collect Metrics"
wait_for_worker_session_stable "alice" 5 120
wait_for_session_stable 5 60
PREV_METRICS=$(cat "${TEST_OUTPUT_DIR}/metrics-03-assign-task.json" 2>/dev/null || true)
METRICS=$(collect_delta_metrics "03-assign-task" "$METRICS_BASELINE" "alice")
print_metrics_report "$METRICS" "$PREV_METRICS"
save_metrics_file "$METRICS" "03-assign-task"

test_teardown "03-assign-task"
test_summary
