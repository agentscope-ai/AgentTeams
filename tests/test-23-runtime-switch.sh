#!/bin/bash
# test-23-runtime-switch.sh - Case 23: Switch worker runtime in-place
#
# Verifies the controller recreates a worker's container with the new
# runtime image when spec.runtime changes, while preserving identity
# (Matrix roomID, Higress consumer name) and user data in MinIO.
#
# The flow exercises:
#   1. create worker runtime=openclaw → container image is openclaw
#   2. write sentinel file to MinIO agents/<name>/
#   3. apply worker runtime=copaw → SpecChanged triggers recreate
#   4. new container image is copaw; sentinel preserved; consumer unchanged
#   5. persist real .copaw workspace/session/secret state
#   6. apply worker runtime=qwenpaw and verify that state is active under
#      .qwenpaw with the same content and one session store
#
# This is a controller-cr style test — no LLM required.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/test-helpers.sh"
source "${SCRIPT_DIR}/lib/minio-client.sh"
source "${SCRIPT_DIR}/lib/higress-client.sh"

test_setup "23-runtime-switch"

TEST_WORKER="test-rt-$$"
STORAGE_PREFIX="${STORAGE_PREFIX:-${TEST_STORAGE_PREFIX:-agentteams/agentteams-storage}}"

_cleanup() {
    log_info "Cleaning up: ${TEST_WORKER}"
    exec_in_agent agt delete worker "${TEST_WORKER}" 2>/dev/null || true
    sleep 5
    remove_worker_container "${TEST_WORKER}"
    exec_in_manager mc rm -r --force "${STORAGE_PREFIX}/agents/${TEST_WORKER}/" 2>/dev/null || true
    exec_in_manager mc rm "${STORAGE_PREFIX}/agentteams-config/workers/${TEST_WORKER}.yaml" 2>/dev/null || true
}
trap _cleanup EXIT

minio_setup

_get_higress_consumers_or_fail() {
    local label="$1"
    local consumers

    if ! higress_login "${TEST_ADMIN_USER}" "${TEST_ADMIN_PASSWORD}" > /dev/null 2>&1; then
        log_fail "Unable to log in to Higress before ${label}"
        return 1
    fi

    if ! consumers=$(higress_get_consumers 2>/dev/null); then
        log_fail "Unable to query Higress consumers during ${label}"
        return 1
    fi

    if ! echo "${consumers}" | jq -e '.data | type == "array"' >/dev/null 2>&1; then
        log_fail "Higress consumers response during ${label} is not valid JSON with a data array"
        return 1
    fi

    HIGRESS_CONSUMERS_JSON="${consumers}"
}

# ============================================================
# Section 1: Create worker with openclaw runtime
# ============================================================
log_section "Create Worker (runtime=openclaw)"

# apply (not create) so the second invocation can update in place
CREATE_OUTPUT=$(exec_in_agent agt apply worker --name "${TEST_WORKER}" --runtime openclaw 2>&1)
CREATE_EXIT=$?
if [ "${CREATE_EXIT}" -eq 0 ]; then
    log_pass "agt apply (openclaw) accepted"
else
    log_fail "agt apply (openclaw) failed: ${CREATE_OUTPUT}"
    test_teardown "23-runtime-switch"; test_summary; exit 1
fi

if wait_worker_provisioned "${TEST_WORKER}" 180; then
    log_pass "Worker provisioned"
else
    log_fail "Worker did not reach provisioned state"
    test_teardown "23-runtime-switch"; test_summary; exit 1
fi

if wait_for_worker_container "${TEST_WORKER}" 120; then
    log_pass "Container started under openclaw runtime"
else
    log_fail "Container did not start under openclaw"
fi

# ============================================================
# Section 2: Snapshot pre-switch state
# ============================================================
log_section "Snapshot Pre-Switch State"

OLD_CONTAINER="$(worker_container_name "${TEST_WORKER}")"
OLD_IMAGE=$(docker inspect --format '{{.Config.Image}}' "${OLD_CONTAINER}" 2>/dev/null || echo "")
OLD_CONTAINER_ID=$(docker inspect --format '{{.Id}}' "${OLD_CONTAINER}" 2>/dev/null | head -c 12 || echo "")
log_info "Pre-switch image: ${OLD_IMAGE}"
log_info "Pre-switch container ID (short): ${OLD_CONTAINER_ID}"

if echo "${OLD_IMAGE}" | grep -qi "openclaw\|worker-agent"; then
    log_pass "Pre-switch container is openclaw image"
else
    log_info "Pre-switch image label does not obviously identify openclaw (${OLD_IMAGE}); continuing"
fi

# Capture Matrix room ID and Higress consumer
OLD_ROOM_ID=$(get_worker_room_id "${TEST_WORKER}")
log_info "Pre-switch roomID: ${OLD_ROOM_ID}"

HIGRESS_CONSUMERS_JSON=""
if _get_higress_consumers_or_fail "pre-switch snapshot"; then
    OLD_CONSUMERS="${HIGRESS_CONSUMERS_JSON}"
    if echo "${OLD_CONSUMERS}" | jq -r '.data[]?.name // empty' 2>/dev/null | grep -Fxq "worker-${TEST_WORKER}"; then
        log_pass "Higress consumer present pre-switch"
    else
        log_fail "Higress consumer missing pre-switch"
    fi
fi

# Write sentinel file to MinIO (proxy for user data the controller must preserve)
exec_in_manager mc cp /etc/hostname \
    "${STORAGE_PREFIX}/agents/${TEST_WORKER}/runtime-switch-sentinel.txt" >/dev/null 2>&1 || true
if minio_file_exists "agents/${TEST_WORKER}/runtime-switch-sentinel.txt"; then
    log_pass "Sentinel file written to MinIO"
else
    log_fail "Sentinel file write failed"
fi

# ============================================================
# Section 3: Switch runtime to copaw
# ============================================================
log_section "Switch Runtime (openclaw → copaw)"

SWITCH_OUTPUT=$(exec_in_agent agt apply worker --name "${TEST_WORKER}" --runtime copaw 2>&1)
SWITCH_EXIT=$?
if [ "${SWITCH_EXIT}" -eq 0 ]; then
    log_pass "agt apply (copaw) accepted"
else
    log_fail "agt apply (copaw) failed: ${SWITCH_OUTPUT}"
fi

# Wait for the controller to recreate the container. We poll for either
# (a) the container ID changes, or (b) the image label contains "copaw".
log_info "Waiting for container recreation..."
DEADLINE=$(( $(date +%s) + 240 ))
NEW_CONTAINER_ID=""
NEW_IMAGE=""
while [ "$(date +%s)" -lt "${DEADLINE}" ]; do
    NEW_CONTAINER="$(worker_container_name "${TEST_WORKER}")"
    NEW_CONTAINER_ID=$(docker inspect --format '{{.Id}}' "${NEW_CONTAINER}" 2>/dev/null | head -c 12 || echo "")
    NEW_IMAGE=$(docker inspect --format '{{.Config.Image}}' "${NEW_CONTAINER}" 2>/dev/null || echo "")
    if [ -n "${NEW_CONTAINER_ID}" ] \
        && [ "${NEW_CONTAINER_ID}" != "${OLD_CONTAINER_ID}" ] \
        && [ -n "${NEW_IMAGE}" ]; then
        break
    fi
    sleep 5
done

# ============================================================
# Section 4: Verify post-switch state
# ============================================================
log_section "Verify Post-Switch State"

if [ -n "${NEW_CONTAINER_ID}" ] && [ "${NEW_CONTAINER_ID}" != "${OLD_CONTAINER_ID}" ]; then
    log_pass "Container recreated (id: ${OLD_CONTAINER_ID} → ${NEW_CONTAINER_ID})"
else
    log_fail "Container not recreated (id still ${OLD_CONTAINER_ID})"
fi

if echo "${NEW_IMAGE}" | grep -qi "copaw"; then
    log_pass "Post-switch image is copaw: ${NEW_IMAGE}"
else
    log_fail "Post-switch image does not look like copaw: ${NEW_IMAGE}"
fi

# Matrix room preserved
NEW_ROOM_ID=$(get_worker_room_id "${TEST_WORKER}")
if [ -n "${OLD_ROOM_ID}" ] && [ "${NEW_ROOM_ID}" = "${OLD_ROOM_ID}" ]; then
    log_pass "Matrix roomID preserved across runtime switch"
else
    log_fail "Matrix roomID changed (was: ${OLD_ROOM_ID}, now: ${NEW_ROOM_ID})"
fi

# Higress consumer preserved (same name)
HIGRESS_CONSUMERS_JSON=""
if _get_higress_consumers_or_fail "post-switch assertion"; then
    NEW_CONSUMERS="${HIGRESS_CONSUMERS_JSON}"
    if echo "${NEW_CONSUMERS}" | jq -r '.data[]?.name // empty' 2>/dev/null | grep -Fxq "worker-${TEST_WORKER}"; then
        log_pass "Higress consumer preserved across runtime switch"
    else
        log_fail "Higress consumer missing after runtime switch"
    fi
fi

# Sentinel preserved
if minio_file_exists "agents/${TEST_WORKER}/runtime-switch-sentinel.txt"; then
    log_pass "Sentinel file preserved across runtime switch"
else
    log_fail "Sentinel file lost during runtime switch"
fi

# openclaw.json should still exist (controller's source-of-truth config)
if minio_file_exists "agents/${TEST_WORKER}/openclaw.json"; then
    log_pass "openclaw.json present post-switch (controller-managed config)"
else
    log_fail "openclaw.json missing post-switch"
fi

# ============================================================
# Section 5: Persist CoPaw runtime state
# ============================================================
log_section "Persist CoPaw Runtime State"

COPAW_CONTAINER="$(worker_container_name "${TEST_WORKER}")"
if docker exec "${COPAW_CONTAINER}" sh -c '
    set -e
    root="/root/.copaw-worker/'"${TEST_WORKER}"'"
    mkdir -p "${root}/.copaw/workspaces/default/sessions" "${root}/.copaw.secret"
    printf "%s\n" "COPAW_WORKSPACE_STATE_23" > "${root}/.copaw/workspaces/default/runtime-switch-state.txt"
    printf "%s\n" "{\"chats\":[]}" > "${root}/.copaw/workspaces/default/chats.json"
    printf "%s\n" "COPAW_SESSION_STATE_23" > "${root}/.copaw/workspaces/default/sessions/runtime-switch.jsonl"
    printf "%s\n" "COPAW_SECRET_STATE_23" > "${root}/.copaw.secret/runtime-switch-secret.txt"
'; then
    log_pass "CoPaw workspace, session, and secret state created"
else
    log_fail "Unable to create CoPaw runtime state"
fi

log_info "Waiting for CoPaw state persistence..."
DEADLINE=$(( $(date +%s) + 60 ))
while [ "$(date +%s)" -lt "${DEADLINE}" ]; do
    if minio_file_exists "agents/${TEST_WORKER}/.copaw/workspaces/default/runtime-switch-state.txt" \
        && minio_file_exists "agents/${TEST_WORKER}/.copaw/workspaces/default/chats.json" \
        && minio_file_exists "agents/${TEST_WORKER}/.copaw/workspaces/default/sessions/runtime-switch.jsonl" \
        && minio_file_exists "agents/${TEST_WORKER}/.copaw.secret/runtime-switch-secret.txt"; then
        break
    fi
    sleep 5
done

if minio_file_exists "agents/${TEST_WORKER}/.copaw/workspaces/default/runtime-switch-state.txt" \
    && minio_file_exists "agents/${TEST_WORKER}/.copaw/workspaces/default/chats.json" \
    && minio_file_exists "agents/${TEST_WORKER}/.copaw/workspaces/default/sessions/runtime-switch.jsonl" \
    && minio_file_exists "agents/${TEST_WORKER}/.copaw.secret/runtime-switch-secret.txt"; then
    log_pass "CoPaw runtime state persisted to MinIO"
else
    log_fail "CoPaw runtime state was not persisted to MinIO"
fi

# ============================================================
# Section 6: Switch runtime to QwenPaw and verify active state
# ============================================================
log_section "Switch Runtime (copaw → qwenpaw)"

QWEN_SWITCH_OUTPUT=$(exec_in_agent bash \
    /opt/agentteams/agent/skills/worker-management/scripts/update-worker-config.sh \
    --name "${TEST_WORKER}" --runtime qwenpaw 2>&1)
QWEN_SWITCH_EXIT=$?
if [ "${QWEN_SWITCH_EXIT}" -eq 0 ]; then
    log_pass "Worker management runtime switch to qwenpaw accepted"
else
    log_fail "Worker management runtime switch to qwenpaw failed: ${QWEN_SWITCH_OUTPUT}"
fi

COPAW_CONTAINER_ID="${NEW_CONTAINER_ID}"
DEADLINE=$(( $(date +%s) + 240 ))
QWEN_CONTAINER_ID=""
QWEN_IMAGE=""
while [ "$(date +%s)" -lt "${DEADLINE}" ]; do
    QWEN_CONTAINER="$(worker_container_name "${TEST_WORKER}")"
    QWEN_CONTAINER_ID=$(docker inspect --format '{{.Id}}' "${QWEN_CONTAINER}" 2>/dev/null | head -c 12 || echo "")
    QWEN_IMAGE=$(docker inspect --format '{{.Config.Image}}' "${QWEN_CONTAINER}" 2>/dev/null || echo "")
    if [ -n "${QWEN_CONTAINER_ID}" ] \
        && [ "${QWEN_CONTAINER_ID}" != "${COPAW_CONTAINER_ID}" ] \
        && echo "${QWEN_IMAGE}" | grep -qi "qwenpaw"; then
        break
    fi
    sleep 5
done

if [ -n "${QWEN_CONTAINER_ID}" ] && [ "${QWEN_CONTAINER_ID}" != "${COPAW_CONTAINER_ID}" ]; then
    log_pass "Container recreated for QwenPaw (id: ${COPAW_CONTAINER_ID} → ${QWEN_CONTAINER_ID})"
else
    log_fail "Container was not recreated for QwenPaw"
fi

if echo "${QWEN_IMAGE}" | grep -qi "qwenpaw"; then
    log_pass "Post-migration image is QwenPaw: ${QWEN_IMAGE}"
else
    log_fail "Post-migration image does not look like QwenPaw: ${QWEN_IMAGE}"
fi

if wait_worker_provisioned "${TEST_WORKER}" 180; then
    log_pass "QwenPaw Worker returned to provisioned state"
else
    log_fail "QwenPaw Worker did not return to provisioned state"
fi

QWEN_ROOM_ID=$(get_worker_room_id "${TEST_WORKER}")
if [ -n "${OLD_ROOM_ID}" ] && [ "${QWEN_ROOM_ID}" = "${OLD_ROOM_ID}" ]; then
    log_pass "Matrix roomID remains unchanged after QwenPaw migration"
else
    log_fail "Matrix roomID changed after QwenPaw migration (was: ${OLD_ROOM_ID}, now: ${QWEN_ROOM_ID})"
fi

HIGRESS_CONSUMERS_JSON=""
if _get_higress_consumers_or_fail "QwenPaw migration assertion"; then
    QWEN_CONSUMERS="${HIGRESS_CONSUMERS_JSON}"
    if echo "${QWEN_CONSUMERS}" | jq -r '.data[]?.name // empty' 2>/dev/null | grep -Fxq "worker-${TEST_WORKER}"; then
        log_pass "Higress consumer remains unchanged after QwenPaw migration"
    else
        log_fail "Higress consumer missing after QwenPaw migration"
    fi
fi

if docker exec "${QWEN_CONTAINER}" sh -c "
    set -e
    worker_root=\"/root/agentteams-fs/agents/${TEST_WORKER}\"
    qwen_root=\"\${worker_root}/.qwenpaw\"
    qwen_secret=\"\${worker_root}/.qwenpaw.secret\"
    test \"\$(cat \"\${qwen_root}/workspaces/default/runtime-switch-state.txt\")\" = \"COPAW_WORKSPACE_STATE_23\"
    jq -e '.chats | type == \"array\"' \"\${qwen_root}/workspaces/default/chats.json\" >/dev/null
    test \"\$(cat \"\${qwen_root}/workspaces/default/sessions/runtime-switch.jsonl\")\" = \"COPAW_SESSION_STATE_23\"
    test \"\$(cat \"\${qwen_secret}/runtime-switch-secret.txt\")\" = \"COPAW_SECRET_STATE_23\"
    expected_workspace=\"\${qwen_root}/workspaces/default\"
    test \"\$(jq -r '.workspace_dir' \"\${qwen_root}/workspaces/default/agent.json\")\" = \"\${expected_workspace}\"
    test \"\$(jq -r '.agents.profiles.default.workspace_dir' \"\${qwen_root}/config.json\")\" = \"\${expected_workspace}\"
    test -f \"\${qwen_root}/.copaw-migrated\"
    test ! -e \"\${worker_root}/.copaw\"
    test ! -e \"\${worker_root}/.copaw.secret\"
"; then
    log_pass "CoPaw workspace, session, and secret state is active in QwenPaw"
else
    log_fail "Migrated CoPaw state is not fully active in QwenPaw"
fi

if minio_file_exists "agents/${TEST_WORKER}/.qwenpaw/workspaces/default/runtime-switch-state.txt" \
    && minio_file_exists "agents/${TEST_WORKER}/.qwenpaw/workspaces/default/chats.json" \
    && minio_file_exists "agents/${TEST_WORKER}/.qwenpaw/workspaces/default/sessions/runtime-switch.jsonl" \
    && minio_file_exists "agents/${TEST_WORKER}/.qwenpaw.secret/runtime-switch-secret.txt" \
    && minio_file_exists "agents/${TEST_WORKER}/.qwenpaw/.copaw-migrated"; then
    log_pass "Migrated QwenPaw state and completion marker persisted to MinIO"
else
    log_fail "Migrated QwenPaw state is incomplete in MinIO"
fi

# ============================================================
# Summary
# ============================================================
test_teardown "23-runtime-switch"
test_summary
