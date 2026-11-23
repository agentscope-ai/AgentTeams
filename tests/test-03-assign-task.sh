#!/bin/bash
# test-03-assign-task.sh - Case 3: Assign task in Room, Worker completes
# Verifies: Manager acknowledges the current task, and Worker publishes the
#           requested README with its unique task marker in shared storage.

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
ALICE_ROOM=$(get_worker_room_id "alice")
assert_not_empty "${ALICE_ROOM}" "Alice's registered Worker room found"
METRICS_BASELINE=$(snapshot_baseline "alice")
TASK_ID="test03-$(date +%s)-$$"
RESULT_PATH="shared/test-fixtures/${TASK_ID}/README.md"
matrix_send_message "${ADMIN_TOKEN}" "${DM_ROOM}" \
    "Send this one-line request verbatim using Matrix target room:${ALICE_ROOM}, then acknowledge with marker ${TASK_ID}. This is a standalone Worker request with no separate Task record. Message: @alice:${TEST_MATRIX_DOMAIN} Write a hello-world README with name, description, usage and marker ${TASK_ID} at ${RESULT_PATH} in your workspace. Run filesync push then filesync stat for ${RESULT_PATH}. Confirm the remote file exists before replying."

log_info "Waiting for Manager to process task..."
REPLY=$(matrix_wait_for_reply_matching "${ADMIN_TOKEN}" "${DM_ROOM}" "@manager" "${TASK_ID}" 180)

assert_not_empty "${REPLY}" "Manager acknowledged task assignment"

log_section "Verify Task in MinIO"

minio_setup

log_info "Waiting for this task's actual README in MinIO..."
if minio_wait_for_file "${RESULT_PATH}" 300 "${TASK_ID}"; then
    log_pass "Task README exists in shared storage"
    RESULT=$(minio_read_file "${RESULT_PATH}")
    assert_contains "${RESULT}" "${TASK_ID}" "Task README contains the current task marker"
else
    log_fail "Task README missing from shared storage"
fi

if matrix_read_messages "${ADMIN_TOKEN}" "${ALICE_ROOM}" 50 | jq -e \
    --arg worker "@alice:${TEST_MATRIX_DOMAIN}" --arg manager "${MANAGER_USER}" --arg marker "${TASK_ID}" '
    any(.chunk[]?; .type == "m.room.message" and .sender == $manager and
        ((.content["m.mentions"].user_ids // []) | index($worker)) != null and
        ((.content["org.agentteams.report"].text // .content.body // "") | contains($marker)))' >/dev/null; then
    log_pass "Manager assigned the task in Alice's registered Worker room"
else
    log_fail "No task assignment mention reached Alice's registered Worker room"
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
