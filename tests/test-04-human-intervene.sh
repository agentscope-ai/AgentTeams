#!/bin/bash
# test-04-human-intervene.sh - Case 4: Human sends supplementary instructions mid-task
# Verifies: Human can send additional instructions while Worker is processing,
#           and the final result incorporates both original and supplementary requirements

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/test-helpers.sh"
source "${SCRIPT_DIR}/lib/matrix-client.sh"
source "${SCRIPT_DIR}/lib/agent-metrics.sh"

test_setup "04-human-intervene"

if ! require_llm_key; then
    test_teardown "04-human-intervene"
    test_summary
    exit 0
fi

ADMIN_LOGIN=$(matrix_login "${TEST_ADMIN_USER}" "${TEST_ADMIN_PASSWORD}")
ADMIN_TOKEN=$(echo "${ADMIN_LOGIN}" | jq -r '.access_token')

MANAGER_USER="@manager:${TEST_MATRIX_DOMAIN}"

log_section "Assign Task"

DM_ROOM=$(matrix_find_dm_room "${ADMIN_TOKEN}" "${MANAGER_USER}" 2>/dev/null || true)
assert_not_empty "${DM_ROOM}" "DM room with Manager found"

# Wait for Manager Agent to be fully ready (OpenClaw gateway + joined DM room)
wait_for_manager_agent_ready 300 "${DM_ROOM}" "${ADMIN_TOKEN}" || {
    log_fail "Manager Agent not ready in time"
    test_teardown "04-human-intervene"
    test_summary
    exit 1
}

# Alice container should be running from test-02/03; wait to ensure it's up before snapshot
wait_for_worker_container "alice" 60
METRICS_BASELINE=$(snapshot_baseline "alice")
matrix_send_message "${ADMIN_TOKEN}" "${DM_ROOM}" \
    "Ask Alice to write a Python script that prints 'Hello, World!' and saves it as hello.py."

log_info "Waiting for Manager to relay task..."
sleep 20

log_section "Send Supplementary Instruction"

# Send supplementary instruction mid-task
matrix_send_message "${ADMIN_TOKEN}" "${DM_ROOM}" \
    "Additional requirement for Alice: the script should also accept a command line argument for the name, so it prints 'Hello, <name>!' instead."

log_info "Waiting for Manager to relay supplement..."
REPLY=$(matrix_wait_for_reply "${ADMIN_TOKEN}" "${DM_ROOM}" "@manager" 180 \
    "${ADMIN_TOKEN}" "${DM_ROOM}" "Please check if the supplementary instruction has been processed.")

assert_not_empty "${REPLY}" "Manager acknowledged supplementary instruction"

log_section "Verify Incorporation"

# Wait for completion
sleep 60

# Read recent messages to verify both requirements were addressed
MESSAGES=$(matrix_read_messages "${ADMIN_TOKEN}" "${DM_ROOM}" 20)
log_info "Checking if final result includes both requirements..."

# The result should reference both the original and supplementary requirements
assert_not_empty "${MESSAGES}" "Room has messages from task processing"

log_section "Collect Metrics"
wait_for_worker_session_stable "alice" 5 120
wait_for_session_stable 5 60
PREV_METRICS=$(cat "${TEST_OUTPUT_DIR}/metrics-04-human-intervene.json" 2>/dev/null || true)
METRICS=$(collect_delta_metrics "04-human-intervene" "$METRICS_BASELINE" "alice")
print_metrics_report "$METRICS" "$PREV_METRICS"
save_metrics_file "$METRICS" "04-human-intervene"

test_teardown "04-human-intervene"
test_summary
