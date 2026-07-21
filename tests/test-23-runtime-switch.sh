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
# Summary
# ============================================================
test_teardown "23-runtime-switch"
test_summary
