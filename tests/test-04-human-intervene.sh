#!/bin/bash
# test-04-human-intervene.sh - Case 4: Human sends supplementary instructions mid-task
# Verifies: Human can send additional instructions while Worker is processing,
#           and the final result incorporates both original and supplementary requirements

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/test-helpers.sh"
source "${SCRIPT_DIR}/lib/matrix-client.sh"
source "${SCRIPT_DIR}/lib/agent-metrics.sh"
source "${SCRIPT_DIR}/lib/minio-client.sh"

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
RUN_ID="test04-$(date +%s)-$$"
RESULT_PATH="shared/test-fixtures/${RUN_ID}/hello.py"
ALICE_ROOM=$(get_worker_room_id alice)
assert_not_empty "${ALICE_ROOM}" "Alice's registered Worker room found"
minio_setup

# The Worker acknowledges a paused request first. The supplement releases it;
# no guessed duration or unrelated old chat message can stand in for completion.
matrix_send_message "${ADMIN_TOKEN}" "${DM_ROOM}" \
    "Forward this standalone request verbatim to Alice in room:${ALICE_ROOM}: @alice:${TEST_MATRIX_DOMAIN} For request ${RUN_ID}, prepare to write a Python hello script at ${RESULT_PATH}. First reply with ${RUN_ID}_READY and end your turn. Wait for the supplementary requirement before creating the file. Do not create a separate Task or Project."

READY=$(matrix_wait_for_reply_matching "${ADMIN_TOKEN}" "${ALICE_ROOM}" "@alice:" "${RUN_ID}_READY" 180 "" "" "" 600 "")
assert_not_empty "${READY}" "Alice acknowledged the initial request before the supplement"
if [ -z "${READY}" ]; then
    test_teardown "04-human-intervene"
    test_summary
    exit 1
fi

log_section "Send Supplementary Instruction"
matrix_send_message "${ADMIN_TOKEN}" "${DM_ROOM}" \
    "Forward this supplement verbatim to Alice in room:${ALICE_ROOM}: @alice:${TEST_MATRIX_DOMAIN} Supplement for ${RUN_ID}: write ${RESULT_PATH} now. With no arguments it must print Hello, World!; with one argument it must print Hello, <name>!. Include the literal marker ${RUN_ID} in a Python comment. Publish the file using the TeamHarness filesync tool action=push, path=${RESULT_PATH}, then verify with action=stat. Do not use bulk private workspace sync."

log_section "Verify Incorporation"
if minio_wait_for_file "${RESULT_PATH}" 300 "${RUN_ID}"; then
    # Check both behaviors in the delivered script, rather than accepting any
    # nonempty Matrix history as a successful intervention.
    SCRIPT_FILE="${TEST_OUTPUT_DIR}/${RUN_ID}-hello.py"
    minio_read_file "${RESULT_PATH}" > "${SCRIPT_FILE}"
    ORIGINAL=$(python3 -c 'import subprocess, sys; sys.exit(subprocess.call([sys.executable, *sys.argv[1:]], timeout=10))' "${SCRIPT_FILE}" 2>/dev/null)
    assert_eq "0" "$?" "Delivered Python script executes without arguments"
    SUPPLEMENT=$(python3 -c 'import subprocess, sys; sys.exit(subprocess.call([sys.executable, *sys.argv[1:]], timeout=10))' "${SCRIPT_FILE}" AgentTeams 2>/dev/null)
    assert_eq "0" "$?" "Delivered Python script executes with a name"
    assert_eq "Hello, World!" "${ORIGINAL}" "Original requirement preserved"
    assert_eq "Hello, AgentTeams!" "${SUPPLEMENT}" "Supplementary requirement incorporated"
else
    log_fail "This request's supplemented script was not delivered within 300s"
fi

log_section "Collect Metrics"
wait_for_worker_session_stable "alice" 5 120
wait_for_session_stable 5 60
PREV_METRICS=$(cat "${TEST_OUTPUT_DIR}/metrics-04-human-intervene.json" 2>/dev/null || true)
METRICS=$(collect_delta_metrics "04-human-intervene" "$METRICS_BASELINE" "alice")
print_metrics_report "$METRICS" "$PREV_METRICS"
save_metrics_file "$METRICS" "04-human-intervene"

test_teardown "04-human-intervene"
test_summary
