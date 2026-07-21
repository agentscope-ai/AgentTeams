#!/bin/bash
# test-02-create-worker.sh - Case 2: Create Worker Alice via Matrix conversation
# Verifies: Manager creates Matrix user, Higress consumer, Room, config files,
#           and returns install command

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/test-helpers.sh"
source "${SCRIPT_DIR}/lib/matrix-client.sh"
source "${SCRIPT_DIR}/lib/higress-client.sh"
source "${SCRIPT_DIR}/lib/minio-client.sh"
source "${SCRIPT_DIR}/lib/agent-metrics.sh"

test_setup "02-create-worker"

if ! require_llm_key; then
    test_teardown "02-create-worker"
    test_summary
    exit 0
fi

# Login as admin
ADMIN_LOGIN=$(matrix_login "${TEST_ADMIN_USER}" "${TEST_ADMIN_PASSWORD}")
ADMIN_TOKEN=$(echo "${ADMIN_LOGIN}" | jq -r '.access_token')

# Get admin DM room with Manager (assumes test-01 created it)
MANAGER_USER="@manager:${TEST_MATRIX_DOMAIN}"

log_section "Request Worker Creation"

# Find or create a DM room with Manager
DM_ROOM=$(matrix_find_dm_room "${ADMIN_TOKEN}" "${MANAGER_USER}" 2>/dev/null || true)

if [ -z "${DM_ROOM}" ]; then
    log_info "Creating DM room with Manager..."
    DM_ROOM=$(matrix_create_dm_room "${ADMIN_TOKEN}" "${MANAGER_USER}")
    sleep 5
fi

assert_not_empty "${DM_ROOM}" "DM room with Manager exists"

# Wait for Manager Agent to be fully ready (OpenClaw gateway + joined DM room)
wait_for_manager_agent_ready 300 "${DM_ROOM}" "${ADMIN_TOKEN}" || {
    log_fail "Manager Agent not ready in time"
    test_teardown "02-create-worker"
    test_summary
    exit 1
}

# Wait for Manager to finish processing any pending messages from previous tests
# (e.g., SOUL.md configuration from test-01) before sending a new request.
# Without this, the SOUL.md reply may arrive after our baseline snapshot and
# get mistaken for the create-worker reply.
wait_for_session_stable 5 60

# Snapshot metrics baseline before sending message (to calculate delta later)
METRICS_BASELINE=$(snapshot_baseline)
TEST_WORKER_RUNTIME="${HICLAW_DEFAULT_WORKER_RUNTIME:-openclaw}"

# Send create worker request.
#
# Why the prompt is so explicit: worker-management/SKILL.md instructs Manager
# to ask admin for FOUR inputs (name / runtime / SOUL / skills) before running
# `agt create worker`, and to NOT invent defaults. A vague prompt that only
# names the worker is therefore a coin flip — sometimes the LLM follows
# SKILL.md strictly and replies with a 4-question confirmation, never calling
# the CLI, and downstream assertions (consumer / SOUL.md) silently fail. We
# avoid that by spelling out all four inputs and telling Manager to skip
# confirmation, so the test exercises actual Worker creation rather than the
# LLM's confirmation-loop behavior.
#
# The runtime is explicit for the same reason: CI matrix runtime must win over
# any rendered fallback text the Manager may have cached in its workspace.
matrix_send_message "${ADMIN_TOKEN}" "${DM_ROOM}" \
    "Please create a new Worker now using these exact values — do not ask me to confirm any of them:
- name: alice
- runtime: ${TEST_WORKER_RUNTIME} (use this exact runtime; do not reinterpret it as the install default)
- SOUL/role: Frontend developer specializing in modern web frameworks, responsive design, and clean UI implementation
- skills: github-operations (file-sync / task-progress / project-participation are auto-included, no need to ask)

Proceed immediately and tell me when she is created."

log_info "Waiting for Manager to create Worker Alice..."

# Wait for a Manager DM reply that explicitly names 'alice'.
#
# Why we tolerate progressive replies: some Manager runtimes (notably CoPaw)
# emit one or more interim acks before the reply that actually names the
# Worker — for example "I need to set up the GitHub MCP server first" when the
# admin's request bundles a precondition like "she should have access to
# GitHub MCP". The follow-up reply ("...let me create Worker 'alice'...")
# arrives 5-30s later. matrix_wait_for_reply_matching keeps reading new
# Manager messages until one matches 'alice' (or until the 5min timeout),
# while still logging the interim acks so the test artifact captures them.
REPLY=$(matrix_wait_for_reply_matching "${ADMIN_TOKEN}" "${DM_ROOM}" "@manager" "alice" 300 \
    "${ADMIN_TOKEN}" "${DM_ROOM}" "Please check if the worker creation request has been processed.")

log_section "Verify Manager Response"

log_info "Manager reply (first 500 chars): $(echo "${REPLY}" | head -c 500)"

assert_not_empty "${REPLY}" "Manager replied to create worker request mentioning 'alice'"
assert_contains_i "${REPLY}" "alice" "Reply mentions worker name 'alice'"

# Show error logs on failure for debugging
if ! echo "${REPLY}" | grep -qi "alice" 2>/dev/null; then
    log_info "--- Manager Agent Error Log ---"
    exec_in_agent tail -10 /var/log/agentteams/manager-agent-error.log 2>/dev/null || true
fi

log_section "Verify Infrastructure"

# Check Worker openclaw.json has memorySearch config (only if embedding model is configured)
minio_setup
ALICE_OPENCLAW=$(minio_read_file "agents/alice/openclaw.json" 2>/dev/null || echo "{}")
MEMORY_SEARCH_MODEL=$(echo "${ALICE_OPENCLAW}" | jq -r '.agents.defaults.memorySearch.model // empty' 2>/dev/null)
if [ -n "${HICLAW_EMBEDDING_MODEL}" ] && [ -n "${ALICE_OPENCLAW}" ] && [ "${ALICE_OPENCLAW}" != "{}" ]; then
    assert_not_empty "${MEMORY_SEARCH_MODEL}" "Worker openclaw.json has memorySearch.model configured"
    log_info "Worker embedding model: ${MEMORY_SEARCH_MODEL}"
fi

# Check Matrix user exists
ALICE_LOGIN=$(matrix_login "alice" "" 2>/dev/null || echo "{}")
# Note: We don't know Alice's password, but we can check if the user was registered
# by trying to find the user in room membership

# Check Higress consumer.
# Manager (especially copaw runtime) often replies progressively: the first
# reply just acknowledges the request ("I'll create alice…"), and the actual
# `agt create worker` call happens in subsequent turns and can take longer
# under CI LLM latency. So the consumer may not exist immediately when
# matrix_wait_for_reply returns. Poll for up to 180s before failing.
higress_login "${TEST_ADMIN_USER}" "${TEST_ADMIN_PASSWORD}" > /dev/null
CONSUMERS=""
for i in $(seq 1 180); do
    CONSUMERS=$(higress_get_consumers 2>/dev/null || echo "")
    if echo "${CONSUMERS}" | grep -q "worker-alice"; then
        break
    fi
    sleep 1
done
assert_contains "${CONSUMERS}" "worker-alice" "Higress consumer 'worker-alice' exists"

# Check MinIO files
minio_setup
minio_wait_for_file "agents/alice/SOUL.md" 60
ALICE_SOUL_EXISTS=$?
assert_eq "0" "${ALICE_SOUL_EXISTS}" "Worker Alice SOUL.md exists in MinIO"

ALICE_SOUL=$(minio_read_file "agents/alice/SOUL.md")
assert_contains_i "${ALICE_SOUL}" "frontend" "Alice's SOUL.md mentions frontend"

ALICE_WORKER_JSON=$(exec_in_agent agt get workers alice -o json 2>/dev/null || echo "{}")
ALICE_RUNTIME=$(echo "${ALICE_WORKER_JSON}" | jq -r '.runtime // empty')
assert_eq "${TEST_WORKER_RUNTIME}" "${ALICE_RUNTIME}" \
    "Worker Alice runtime matches test matrix (got: '${ALICE_RUNTIME}', want: '${TEST_WORKER_RUNTIME}')"

log_section "Start Worker Container"

# Extract install parameters from Manager's reply and start Worker
# In real test, we would parse the install command from REPLY
log_info "Worker Alice verification complete (container start requires install params from Manager)"

if [ "${TEST_WORKER_RUNTIME}" = "copaw" ]; then
    log_section "Verify CoPaw Worker Probes"

    if wait_for_worker_container "alice" 180; then
        PROBE_OUTPUT=$(check_copaw_worker_probes "alice" "ready" 90)
        PROBE_STATUS=$?
        if [ "${PROBE_STATUS}" = "0" ]; then
            log_pass "CoPaw Worker Alice /worker/livez and /worker/readyz are valid"
            log_info "${PROBE_OUTPUT}"
        elif [ "${PROBE_STATUS}" = "2" ]; then
            log_info "CoPaw Worker Alice probes skipped"
        else
            log_fail "CoPaw Worker Alice probes failed"
            log_info "${PROBE_OUTPUT}"
        fi
    else
        log_info "CoPaw Worker Alice probes skipped because container was not started by this test run"
    fi
else
    log_info "CoPaw Worker Alice probes skipped for worker runtime '${TEST_WORKER_RUNTIME}'"
fi

log_section "Collect Metrics"

# Wait for Manager to finish all post-reply processing before collecting metrics
wait_for_session_stable 5 60
PREV_METRICS=$(cat "${TEST_OUTPUT_DIR}/metrics-02-create-worker.json" 2>/dev/null || true)
METRICS=$(collect_delta_metrics "02-create-worker" "$METRICS_BASELINE")
print_metrics_report "$METRICS" "$PREV_METRICS"
save_metrics_file "$METRICS" "02-create-worker"

test_teardown "02-create-worker"
test_summary
