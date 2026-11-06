#!/bin/bash
# test-29-legacy-copaw-upgrade.sh - Case 29: Upgrade a legacy CoPaw worker to QwenPaw
#
# Simulates an EXISTING CoPaw worker from an older deployment (CoPaw is
# EOL — issue #1310). The legacy instance is created by inserting the
# Worker CR directly at the CRD layer (infrastructure-level path, via the
# embedded kube-apiserver) with a pinned legacy image. The product API's
# new-creation/switch guard intentionally does not apply to CR-level
# inserts — resources already present in old deployments must remain
# readable, visible, and upgradeable (expected semantics).
#
# The flow exercises:
#   1. pull the fixed legacy CoPaw image (agentteams-copaw-worker:v1.2.4)
#   2. seed Worker CR (runtime=copaw, image=legacy) via the REST API of
#      the embedded kube-apiserver
#   3. controller provisions the legacy instance; container runs the old image
#   4. persist real .copaw workspace/session/secret state (MinIO-backed)
#   5. upgrade to qwenpaw via update-worker-config.sh → container recreated
#   6. state migrated under .qwenpaw with the same content; room/consumer preserved
#   7. NEW CoPaw worker creation via the product API must be REJECTED
#
# This is a controller-cr style test — no LLM required.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/test-helpers.sh"
source "${SCRIPT_DIR}/lib/minio-client.sh"
source "${SCRIPT_DIR}/lib/higress-client.sh"
source "${SCRIPT_DIR}/lib/test29-evidence.sh"
source "${SCRIPT_DIR}/lib/test29-readiness.sh"

test_setup "29-legacy-copaw-upgrade"

TEST_WORKER="test-legacy-$$"
STORAGE_PREFIX="${STORAGE_PREFIX:-${TEST_STORAGE_PREFIX:-agentteams/agentteams-storage}}"

# Pinned legacy CoPaw image — the "existing instance" fingerprint.
# (May be pinned via @sha256: digest in the future.)
LEGACY_IMAGE="${COPAW_LEGACY_IMAGE:-higress-registry.cn-hangzhou.cr.aliyuncs.com/agentteams/agentteams-copaw-worker:v1.2.4}"

CTRL=agentteams-controller

# CI test-artifact directory (the workflow copies tests/output/* into the
# uploaded artifacts); failure evidence is written under it.
TEST_OUTPUT_DIR="${TEST_OUTPUT_DIR:-${SCRIPT_DIR%/*}/tests/output}"

# ============================================================
# Failure evidence collector (credential-safe; implementation in
# lib/test29-evidence.sh: diagnostic-field projection + unified
# redaction applied before any artifact is written)
# ============================================================
# When the test has failed, persist diagnostics for the failing Worker
# under ${TEST_OUTPUT_DIR}/test-29-evidence/ *before* cleanup deletes
# the Worker/CR/storage, so the CI artifacts contain the evidence.
# Never fails the test itself: every command is guarded.
EVIDENCE_DIR="${TEST_OUTPUT_DIR}/test-29-evidence"
EVIDENCE_COLLECTED=0

# One pass/fail line for a migrated object's presence in MinIO
# (expected vs actual), used by the per-file report below.
minio_presence_check() {
    local label="$1"
    local state="$2"
    if [ "${state}" = "present" ]; then
        log_pass "MinIO: ${label} present"
    else
        log_fail "MinIO: ${label} missing after migration (expected: present, got: absent)"
    fi
}

_cleanup() {
    log_info "Cleaning up: ${TEST_WORKER}"
    # Capture failure evidence while the Worker still exists; the deletes
    # below would make it unrecoverable.
    if [ "${TESTS_FAILED:-0}" -gt 0 ]; then
        collect_test29_evidence "${TEST_WORKER}"
    fi
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
# Section 1: Pull fixed legacy CoPaw image
# ============================================================
log_section "Pull Fixed Legacy CoPaw Image"

if docker pull "${LEGACY_IMAGE}" >/dev/null 2>&1; then
    log_pass "Legacy CoPaw image pulled: ${LEGACY_IMAGE}"
elif [ "${COPAW_LEGACY_IMAGE_OPTIONAL:-0}" = "1" ]; then
    # Fork PR runs receive no registry credentials and may be unable to reach
    # the registry at all; skip explicitly instead of failing the shard.
    log_info "SKIP: unable to pull legacy CoPaw image and COPAW_LEGACY_IMAGE_OPTIONAL=1: ${LEGACY_IMAGE}"
    test_teardown "29-legacy-copaw-upgrade"
    test_summary
    exit 0
else
    log_fail "Unable to pull legacy CoPaw image: ${LEGACY_IMAGE}"
    test_teardown "29-legacy-copaw-upgrade"; test_summary; exit 1
fi

LEGACY_DIGEST=$(docker image inspect --format='{{index .RepoDigests 0}}' "${LEGACY_IMAGE}" 2>/dev/null || echo "")
log_info "Legacy image digest: ${LEGACY_DIGEST:-<unavailable>}"

# ============================================================
# Section 2: Seed the existing CoPaw worker at the CR layer
# ============================================================
log_section "Seed Existing CoPaw Worker (CR-layer insert)"

DATA_DIR=$(docker exec "$CTRL" sh -c 'printf %s "${AGENTTEAMS_DATA_DIR:-/data/agentteams-controller}"')
TOKEN=$(docker exec "$CTRL" cat "${DATA_DIR}/admin-token" 2>/dev/null || true)
MODEL=$(docker exec "$CTRL" printenv AGENTTEAMS_DEFAULT_MODEL 2>/dev/null || true)
MODEL="${MODEL:-qwen3.6-plus}"
log_info "Seeding Worker CR: name=${TEST_WORKER} runtime=copaw image=${LEGACY_IMAGE} model=${MODEL}"

if [ -z "${TOKEN}" ]; then
    log_fail "Could not read admin token from ${DATA_DIR}/admin-token"
    test_teardown "29-legacy-copaw-upgrade"; test_summary; exit 1
fi

# The embedded controller image ships the kube-apiserver and curl but not
# kubectl; create the CR through the apiserver REST API instead.
CR_BODY=$(cat <<JSON
{"apiVersion":"agentteams.io/v1beta1","kind":"Worker","metadata":{"name":"${TEST_WORKER}","namespace":"default"},"spec":{"runtime":"copaw","image":"${LEGACY_IMAGE}","model":"${MODEL}","workerName":"${TEST_WORKER}"}}
JSON
)
CR_CODE=$(printf '%s' "${CR_BODY}" | docker exec -i "$CTRL" curl -sS -o /tmp/legacy-worker-seed.json -w '%{http_code}' \
    --cacert "${DATA_DIR}/pki/ca.crt" \
    -H "Authorization: Bearer ${TOKEN}" \
    -H "Content-Type: application/json" \
    -X POST "https://127.0.0.1:6443/apis/agentteams.io/v1beta1/namespaces/default/workers" \
    --data-binary @-)
if [ "${CR_CODE}" != "200" ] && [ "${CR_CODE}" != "201" ]; then
    CR_DETAIL=$(docker exec "$CTRL" cat /tmp/legacy-worker-seed.json 2>/dev/null | head -c 400 || true)
    log_fail "REST create of legacy Worker CR failed (HTTP ${CR_CODE}): ${CR_DETAIL}"
    test_teardown "29-legacy-copaw-upgrade"; test_summary; exit 1
fi
log_pass "Legacy Worker CR created via the embedded kube-apiserver REST API"

# ============================================================
# Section 3: Legacy instance provisioned on the pinned image
# ============================================================
log_section "Verify Legacy Instance (fixed old image)"

if wait_for_worker_container "${TEST_WORKER}" 120; then
    log_pass "Legacy CoPaw container started"
else
    log_fail "Legacy CoPaw container did not start"
    test_teardown "29-legacy-copaw-upgrade"; test_summary; exit 1
fi

if wait_worker_provisioned "${TEST_WORKER}" 180; then
    log_pass "Legacy CoPaw Worker provisioned"
else
    log_fail "Legacy CoPaw Worker did not reach provisioned state"
    test_teardown "29-legacy-copaw-upgrade"; test_summary; exit 1
fi

COPAW_CONTAINER="$(worker_container_name "${TEST_WORKER}")"
LEGACY_ACTUAL_IMAGE=$(docker inspect --format '{{.Config.Image}}' "${COPAW_CONTAINER}" 2>/dev/null || echo "")
NEW_CONTAINER_ID=$(docker inspect --format '{{.Id}}' "${COPAW_CONTAINER}" 2>/dev/null | head -c 12 || echo "")
log_info "Legacy container image: ${LEGACY_ACTUAL_IMAGE}"
log_info "Legacy container ID (short): ${NEW_CONTAINER_ID}"

if [ "${LEGACY_ACTUAL_IMAGE}" = "${LEGACY_IMAGE}" ]; then
    log_pass "Container runs the pinned legacy image (existing instance fingerprint)"
else
    log_fail "Container image is not the pinned legacy image (expected: ${LEGACY_IMAGE}, got: ${LEGACY_ACTUAL_IMAGE})"
    test_teardown "29-legacy-copaw-upgrade"; test_summary; exit 1
fi

# ------------------------------------------------------------
# Readiness before seeding: CR "provisioned" only means the room and
# Matrix user were persisted — the application (and its first Matrix
# sync) may still be starting. Wait (bounded) until BOTH hold:
#   (a) the adapter probes report ready
#   (b) the Worker's Matrix room ID is available and stable
#       (identical across two consecutive observations)
# On timeout: fail with the last observed value and collect evidence.
# ------------------------------------------------------------
READINESS_TIMEOUT=240
# The gate lives in lib/test29-readiness.sh so the poll sequence can be driven
# deterministically (see tests/check-test29-readiness.sh).
log_info "Waiting for Worker readiness (probes ready + stable roomID) (timeout: ${READINESS_TIMEOUT}s)..."
if test29_wait_worker_ready "${TEST_WORKER}" "${READINESS_TIMEOUT}"; then
    log_pass "Worker ready before seeding (probes ready; stable roomID: ${TEST29_READY_ROOM})"
else
    PROBE_TAIL=$(printf '%s\n' "${TEST29_PROBE_LAST}" | tail -n 2 | tr '\n' ' ')
    log_fail "Worker not ready before seeding (timeout: ${READINESS_TIMEOUT}s; probes: not ready [last probe: ${PROBE_TAIL:-<no probe output>}] roomID: stable=no last_observed='${TEST29_LAST_ROOM:-<empty>}'"
    collect_test29_evidence "${TEST_WORKER}"
    test_teardown "29-legacy-copaw-upgrade"; test_summary; exit 1
fi

# ============================================================
# Section 4: Snapshot pre-upgrade state
# ============================================================
log_section "Snapshot Pre-Upgrade State"

OLD_ROOM_ID=$(get_worker_room_id "${TEST_WORKER}")
log_info "Pre-upgrade roomID: ${OLD_ROOM_ID}"

HIGRESS_CONSUMERS_JSON=""
if _get_higress_consumers_or_fail "pre-upgrade snapshot"; then
    OLD_CONSUMERS="${HIGRESS_CONSUMERS_JSON}"
    if echo "${OLD_CONSUMERS}" | jq -r '.data[]?.name // empty' 2>/dev/null | grep -Fxq "worker-${TEST_WORKER}"; then
        log_pass "Higress consumer present pre-upgrade"
    else
        log_fail "Higress consumer missing pre-upgrade"
    fi
fi

# ============================================================
# Section 5: Persist Legacy CoPaw State (existing instance)
# ============================================================
log_section "Persist Legacy CoPaw State (existing instance)"

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
    dump_diagnostics worker "${TEST_WORKER}"
    test_teardown "29-legacy-copaw-upgrade"; test_summary; exit 1
fi

# ============================================================
# Section 6: Upgrade runtime to QwenPaw and verify active state
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

# ------------------------------------------------------------
# Migrated state in the QwenPaw container — bounded poll, then one
# pass/fail line per file/predicate (expected vs actual). Migration
# runs during container startup, so do not assert on the first tick.
# ------------------------------------------------------------
QWEN_WORKER_ROOT="/root/agentteams-fs/agents/${TEST_WORKER}"
QWEN_STATE_ROOT="${QWEN_WORKER_ROOT}/.qwenpaw"
QWEN_SECRET_ROOT="${QWEN_WORKER_ROOT}/.qwenpaw.secret"
EXPECTED_WORKSPACE="${QWEN_STATE_ROOT}/workspaces/default"

MIG_STATE=""
MIG_CHATS_TYPE=""
MIG_SESSIONS=""
MIG_SECRET=""
MIG_AGENT_WS=""
MIG_CONFIG_WS=""
MIG_MARKER="absent"
MIG_LEGACY_COPAW="absent"
MIG_LEGACY_SECRET="absent"
MIG_ALL_OK=0

MIGRATION_TIMEOUT=180
MIGRATION_DEADLINE=$(( $(date +%s) + MIGRATION_TIMEOUT ))
while [ "$(date +%s)" -lt "${MIGRATION_DEADLINE}" ]; do
    MIG_STATE=$(docker exec "${QWEN_CONTAINER}" cat "${QWEN_STATE_ROOT}/workspaces/default/runtime-switch-state.txt" 2>/dev/null || true)
    MIG_CHATS_TYPE=$(docker exec "${QWEN_CONTAINER}" jq -r '.chats | type' "${QWEN_STATE_ROOT}/workspaces/default/chats.json" 2>/dev/null || true)
    MIG_SESSIONS=$(docker exec "${QWEN_CONTAINER}" cat "${QWEN_STATE_ROOT}/workspaces/default/sessions/runtime-switch.jsonl" 2>/dev/null || true)
    MIG_SECRET=$(docker exec "${QWEN_CONTAINER}" cat "${QWEN_SECRET_ROOT}/runtime-switch-secret.txt" 2>/dev/null || true)
    MIG_AGENT_WS=$(docker exec "${QWEN_CONTAINER}" jq -r '.workspace_dir' "${QWEN_STATE_ROOT}/workspaces/default/agent.json" 2>/dev/null || true)
    MIG_CONFIG_WS=$(docker exec "${QWEN_CONTAINER}" jq -r '.agents.profiles.default.workspace_dir' "${QWEN_STATE_ROOT}/config.json" 2>/dev/null || true)
    docker exec "${QWEN_CONTAINER}" test -f "${QWEN_STATE_ROOT}/.copaw-migrated" 2>/dev/null && MIG_MARKER="present" || MIG_MARKER="absent"
    docker exec "${QWEN_CONTAINER}" test ! -e "${QWEN_WORKER_ROOT}/.copaw" 2>/dev/null && MIG_LEGACY_COPAW="absent" || MIG_LEGACY_COPAW="present"
    docker exec "${QWEN_CONTAINER}" test ! -e "${QWEN_WORKER_ROOT}/.copaw.secret" 2>/dev/null && MIG_LEGACY_SECRET="absent" || MIG_LEGACY_SECRET="present"

    MIG_ALL_OK=1
    [ "${MIG_STATE}" = "COPAW_WORKSPACE_STATE_23" ] || MIG_ALL_OK=0
    [ "${MIG_CHATS_TYPE}" = "array" ] || MIG_ALL_OK=0
    [ "${MIG_SESSIONS}" = "COPAW_SESSION_STATE_23" ] || MIG_ALL_OK=0
    [ "${MIG_SECRET}" = "COPAW_SECRET_STATE_23" ] || MIG_ALL_OK=0
    [ "${MIG_AGENT_WS}" = "${EXPECTED_WORKSPACE}" ] || MIG_ALL_OK=0
    [ "${MIG_CONFIG_WS}" = "${EXPECTED_WORKSPACE}" ] || MIG_ALL_OK=0
    [ "${MIG_MARKER}" = "present" ] || MIG_ALL_OK=0
    [ "${MIG_LEGACY_COPAW}" = "absent" ] || MIG_ALL_OK=0
    [ "${MIG_LEGACY_SECRET}" = "absent" ] || MIG_ALL_OK=0
    if [ "${MIG_ALL_OK}" -eq 1 ]; then
        break
    fi
    sleep 5
done

# Per-predicate report: one pass/fail line per file/predicate,
# each with expected vs actual (the last observed value).
assert_eq "COPAW_WORKSPACE_STATE_23" "${MIG_STATE}" "Migrated workspace state: .qwenpaw/workspaces/default/runtime-switch-state.txt"
assert_eq "array" "${MIG_CHATS_TYPE}" "Migrated workspace state: .qwenpaw/workspaces/default/chats.json (.chats type)"
assert_eq "COPAW_SESSION_STATE_23" "${MIG_SESSIONS}" "Migrated session state: .qwenpaw/workspaces/default/sessions/runtime-switch.jsonl"
assert_eq "COPAW_SECRET_STATE_23" "${MIG_SECRET}" "Migrated secret state: .qwenpaw.secret/runtime-switch-secret.txt"
assert_eq "${EXPECTED_WORKSPACE}" "${MIG_AGENT_WS}" "Migrated workspace pointer: .qwenpaw/workspaces/default/agent.json (.workspace_dir)"
assert_eq "${EXPECTED_WORKSPACE}" "${MIG_CONFIG_WS}" "Migrated config pointer: .qwenpaw/config.json (.agents.profiles.default.workspace_dir)"
if [ "${MIG_MARKER}" = "present" ]; then
    log_pass "Migration completion marker present: .qwenpaw/.copaw-migrated"
else
    log_fail "Migration completion marker missing: .qwenpaw/.copaw-migrated (expected: present, got: absent)"
fi
if [ "${MIG_LEGACY_COPAW}" = "absent" ]; then
    log_pass "Legacy .copaw removed from worker root"
else
    log_fail "Legacy .copaw still present in worker root (expected: absent, got: present)"
fi
if [ "${MIG_LEGACY_SECRET}" = "absent" ]; then
    log_pass "Legacy .copaw.secret removed from worker root"
else
    log_fail "Legacy .copaw.secret still present in worker root (expected: absent, got: present)"
fi
if [ "${MIG_ALL_OK}" -ne 1 ]; then
    log_fail "Migrated CoPaw state not fully active in QwenPaw after ${MIGRATION_TIMEOUT}s (last observed: state='${MIG_STATE}' chats='${MIG_CHATS_TYPE}' sessions='${MIG_SESSIONS}' secret='${MIG_SECRET}' agent_ws='${MIG_AGENT_WS}' config_ws='${MIG_CONFIG_WS}' marker='${MIG_MARKER}' legacy_copaw='${MIG_LEGACY_COPAW}' legacy_secret='${MIG_LEGACY_SECRET}')"
fi

# ------------------------------------------------------------
# Migrated state in MinIO — bounded poll, then one pass/fail line
# per object (expected vs actual).
# ------------------------------------------------------------
MINIO_MIGRATION_TIMEOUT=120
MINIO_M_STATE="absent"
MINIO_M_CHATS="absent"
MINIO_M_SESSIONS="absent"
MINIO_M_SECRET="absent"
MINIO_M_MARKER="absent"
MINIO_M_ALL_OK=0

MINIO_MIGRATION_DEADLINE=$(( $(date +%s) + MINIO_MIGRATION_TIMEOUT ))
while [ "$(date +%s)" -lt "${MINIO_MIGRATION_DEADLINE}" ]; do
    minio_file_exists "agents/${TEST_WORKER}/.qwenpaw/workspaces/default/runtime-switch-state.txt" && MINIO_M_STATE="present" || MINIO_M_STATE="absent"
    minio_file_exists "agents/${TEST_WORKER}/.qwenpaw/workspaces/default/chats.json" && MINIO_M_CHATS="present" || MINIO_M_CHATS="absent"
    minio_file_exists "agents/${TEST_WORKER}/.qwenpaw/workspaces/default/sessions/runtime-switch.jsonl" && MINIO_M_SESSIONS="present" || MINIO_M_SESSIONS="absent"
    minio_file_exists "agents/${TEST_WORKER}/.qwenpaw.secret/runtime-switch-secret.txt" && MINIO_M_SECRET="present" || MINIO_M_SECRET="absent"
    minio_file_exists "agents/${TEST_WORKER}/.qwenpaw/.copaw-migrated" && MINIO_M_MARKER="present" || MINIO_M_MARKER="absent"

    MINIO_M_ALL_OK=1
    [ "${MINIO_M_STATE}" = "present" ] || MINIO_M_ALL_OK=0
    [ "${MINIO_M_CHATS}" = "present" ] || MINIO_M_ALL_OK=0
    [ "${MINIO_M_SESSIONS}" = "present" ] || MINIO_M_ALL_OK=0
    [ "${MINIO_M_SECRET}" = "present" ] || MINIO_M_ALL_OK=0
    [ "${MINIO_M_MARKER}" = "present" ] || MINIO_M_ALL_OK=0
    if [ "${MINIO_M_ALL_OK}" -eq 1 ]; then
        break
    fi
    sleep 5
done

minio_presence_check "agents/${TEST_WORKER}/.qwenpaw/workspaces/default/runtime-switch-state.txt" "${MINIO_M_STATE}"
minio_presence_check "agents/${TEST_WORKER}/.qwenpaw/workspaces/default/chats.json" "${MINIO_M_CHATS}"
minio_presence_check "agents/${TEST_WORKER}/.qwenpaw/workspaces/default/sessions/runtime-switch.jsonl" "${MINIO_M_SESSIONS}"
minio_presence_check "agents/${TEST_WORKER}/.qwenpaw.secret/runtime-switch-secret.txt" "${MINIO_M_SECRET}"
minio_presence_check "agents/${TEST_WORKER}/.qwenpaw/.copaw-migrated" "${MINIO_M_MARKER}"
if [ "${MINIO_M_ALL_OK}" -ne 1 ]; then
    log_fail "Migrated QwenPaw state incomplete in MinIO after ${MINIO_MIGRATION_TIMEOUT}s (last observed: state='${MINIO_M_STATE}' chats='${MINIO_M_CHATS}' sessions='${MINIO_M_SESSIONS}' secret='${MINIO_M_SECRET}' marker='${MINIO_M_MARKER}')"
fi

# ============================================================
# Section 7: New CoPaw worker must be rejected (EOL guard, #1310)
# ============================================================
log_section "New CoPaw Worker (must be rejected)"
new_attempt=$(exec_in_agent agt apply worker --name "${TEST_WORKER}-new" --runtime copaw 2>&1)
new_exit=$?
if [ "${new_exit}" -ne 0 ] && echo "${new_attempt}" | grep -qi "qwenpaw"; then
    log_pass "New CoPaw worker creation rejected with QwenPaw guidance"
else
    log_fail "New CoPaw worker creation was not rejected as expected: ${new_attempt}"
fi

# ============================================================
# Summary
# ============================================================
test_teardown "29-legacy-copaw-upgrade"
test_summary
