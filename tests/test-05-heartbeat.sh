#!/bin/bash
# test-05-heartbeat.sh - Case 5: On-demand Manager heartbeat checklist
# Verifies: the Manager finishes the current checklist and returns its final report.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/test-helpers.sh"
source "${SCRIPT_DIR}/lib/matrix-client.sh"
source "${SCRIPT_DIR}/lib/agent-metrics.sh"

test_setup "05-heartbeat"

if ! require_llm_key; then
    test_teardown "05-heartbeat"
    test_summary
    exit 0
fi

ADMIN_LOGIN=$(matrix_login "${TEST_ADMIN_USER}" "${TEST_ADMIN_PASSWORD}")
ADMIN_TOKEN=$(echo "${ADMIN_LOGIN}" | jq -r '.access_token')

MANAGER_USER="@manager:${TEST_MATRIX_DOMAIN}"

log_section "Find Manager Conversation"

DM_ROOM=$(matrix_find_dm_room "${ADMIN_TOKEN}" "${MANAGER_USER}" 2>/dev/null || true)
assert_not_empty "${DM_ROOM}" "DM room with Manager found"

# Wait for Manager Agent to be fully ready (OpenClaw gateway + joined DM room)
wait_for_manager_agent_ready 300 "${DM_ROOM}" "${ADMIN_TOKEN}" || {
    log_fail "Manager Agent not ready in time"
    test_teardown "05-heartbeat"
    test_summary
    exit 1
}

METRICS_BASELINE=$(snapshot_baseline)
log_section "Run Current Heartbeat Checklist"
# A lengthy research assignment is unrelated to heartbeat execution. Request
# the same checklist explicitly on every runtime and wait for its final report.
COMPLETION_MARKER="AT_HEARTBEAT_DONE_$(date +%s)_$$"
matrix_send_message "${ADMIN_TOKEN}" "${DM_ROOM}" \
    "Please execute your heartbeat check now. Read HEARTBEAT.md in your current workspace and follow the full checklist. Report findings here. After all checks finish, end your final report with this exact plain-text line: ${COMPLETION_MARKER}"
REPORT=$(matrix_wait_for_reply_matching "${ADMIN_TOKEN}" "${DM_ROOM}" "@manager:" \
    "(^|[[:space:]])${COMPLETION_MARKER}[[:space:]]*$" 900 "" "" "" 600 "")
assert_not_empty "${REPORT}" "Manager finished the heartbeat checklist and sent the final report"

log_section "Collect Metrics"
wait_for_session_stable 5 60
PREV_METRICS=$(cat "${TEST_OUTPUT_DIR}/metrics-05-heartbeat.json" 2>/dev/null || true)
METRICS=$(collect_delta_metrics "05-heartbeat" "$METRICS_BASELINE")
print_metrics_report "$METRICS" "$PREV_METRICS"
save_metrics_file "$METRICS" "05-heartbeat"

test_teardown "05-heartbeat"
test_summary
