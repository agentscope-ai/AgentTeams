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

# test-05 can leave CoPaw Manager finishing heartbeat / pending-worker cleanup
# replies in the admin DM. Let that prior turn go quiet before measuring Bob's
# create-worker ack/provisioning SLA; the post-request waits below stay strict.
if ! matrix_wait_for_sender_quiet "${ADMIN_TOKEN}" "${DM_ROOM}" "@manager" 20 180; then
    log_fail "Manager DM did not become quiet before Bob create request"
    test_teardown "06-multi-worker"
    test_summary
    exit 1
fi

# Alice is running from previous tests; bob will be created below (offset=0 is correct for new workers)
wait_for_worker_container "alice" 60
METRICS_BASELINE=$(snapshot_baseline "alice" "bob")
TEST_WORKER_RUNTIME="${HICLAW_DEFAULT_WORKER_RUNTIME:-openclaw}"
# worker-management/SKILL.md tells Manager to ask admin for FOUR inputs
# (name / runtime / SOUL / skills) before running `agt create worker`
# and not to invent defaults. A vague prompt that only names the worker is
# therefore a coin flip — sometimes Manager replies with a confirmation
# request, never calls the CLI, and the consumer/SOUL.md polls below
# silently time out. Spell out all four inputs and tell Manager to skip
# confirmation so this test exercises actual Worker creation.
#
# The runtime is explicit because the CI matrix runtime is the source of truth;
# rendered Manager workspace text may contain fallback defaults.
matrix_send_message "${ADMIN_TOKEN}" "${DM_ROOM}" \
    "Please create a new Worker now using these exact values — do not ask me to confirm any of them:
- name: bob
- runtime: ${TEST_WORKER_RUNTIME} (use this exact runtime; do not reinterpret it as the install default)
- SOUL/role: Backend developer specializing in REST APIs, server-side logic, and data persistence
- skills: github-operations (file-sync / task-progress / project-participation are auto-included, no need to ask)

Proceed immediately and tell me when he is created."

log_info "Waiting for Manager to create Worker Bob..."
REPLY=$(matrix_wait_for_reply_matching "${ADMIN_TOKEN}" "${DM_ROOM}" "@manager" \
    "bob.*(accepted|created|creating|pending|running|ready)" 300 \
    "${ADMIN_TOKEN}" "${DM_ROOM}" "Please check if the request to create worker bob has been processed.")

assert_not_empty "${REPLY}" "Manager replied to create bob request"
assert_contains_i "${REPLY}" "bob" "Reply mentions worker name 'bob'"

# Verify Bob's infrastructure. Worker creation is asynchronous, so wait on
# persisted provisioning state and gateway side effects instead of sleeping.
BOB_PROVISION_TIMEOUT=60
if echo "${REPLY}" | grep -qiE "bob.*(accepted|creating|pending)" 2>/dev/null; then
    BOB_PROVISION_TIMEOUT=180
fi
if wait_worker_provisioned "bob" "${BOB_PROVISION_TIMEOUT}"; then
    log_pass "Worker Bob provisioned (roomID + matrixUserID populated)"
else
    log_fail "Worker Bob did not reach provisioned state in ${BOB_PROVISION_TIMEOUT}s"
fi

BOB_WORKER_JSON=$(exec_in_agent agt get workers bob -o json 2>/dev/null || echo "{}")
BOB_RUNTIME=$(echo "${BOB_WORKER_JSON}" | jq -r '.runtime // empty')
assert_eq "${TEST_WORKER_RUNTIME}" "${BOB_RUNTIME}" \
    "Worker Bob runtime matches test matrix (got: '${BOB_RUNTIME}', want: '${TEST_WORKER_RUNTIME}')"

higress_login "${TEST_ADMIN_USER}" "${TEST_ADMIN_PASSWORD}" > /dev/null
CONSUMERS=""
DEADLINE=$(( $(date +%s) + 120 ))
while [ "$(date +%s)" -lt "${DEADLINE}" ]; do
    if CONSUMERS=$(higress_get_consumers 2>/dev/null) \
        && echo "${CONSUMERS}" | grep -qi "worker-bob"; then
        break
    fi
    sleep 5
done
if ! echo "${CONSUMERS}" | grep -qi "worker-bob"; then
    dump_manager_dm_messages "${ADMIN_TOKEN}" "${DM_ROOM}" "worker-bob consumer missing"
fi
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
    MANAGER_TOKEN=$(docker exec "${TEST_AGENT_CONTAINER}" \
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
        MANAGER_TOKEN=$(docker exec "${TEST_AGENT_CONTAINER}" \
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
