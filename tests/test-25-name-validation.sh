#!/bin/bash
# test-25-name-validation.sh - Case 25: Worker name validation
#
# Verifies the CLI rejects worker names that violate the
# `^[a-z0-9][a-z0-9-]*$` regex enforced by validateWorkerName
# (hiclaw-controller/cmd/hiclaw/create.go).
#
# This is a controller-cr style test — no LLM required. It runs the
# CLI directly via exec_in_agent and asserts on exit code + stderr
# substring, then snapshots Higress consumers to confirm no consumer
# leaks for names the controller never accepted.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/test-helpers.sh"
source "${SCRIPT_DIR}/lib/higress-client.sh"

test_setup "25-name-validation"

TEST_VALID_NAME="test-namechk-$$"
ACCEPTED_INVALID_NAMES=()

_cleanup() {
    log_info "Cleaning up: ${TEST_VALID_NAME}"
    for bad_name in "${ACCEPTED_INVALID_NAMES[@]}"; do
        log_info "Cleaning up mistakenly accepted invalid worker: ${bad_name}"
        exec_in_agent hiclaw delete worker "${bad_name}" 2>/dev/null || true
        docker rm -f "hiclaw-worker-${bad_name}" 2>/dev/null || true
    done
    exec_in_agent hiclaw delete worker "${TEST_VALID_NAME}" 2>/dev/null || true
    sleep 3
    docker rm -f "hiclaw-worker-${TEST_VALID_NAME}" 2>/dev/null || true
}
trap _cleanup EXIT

_get_higress_consumers_or_fail() {
    local label="$1"
    local consumers

    if ! higress_login "${TEST_ADMIN_USER}" "${TEST_ADMIN_PASSWORD}" > /dev/null 2>&1; then
        log_fail "Unable to log in to Higress before ${label}" >&2
        return 1
    fi

    if ! consumers=$(higress_get_consumers 2>/dev/null); then
        log_fail "Unable to query Higress consumers during ${label}" >&2
        return 1
    fi

    if ! echo "${consumers}" | jq -e '.data | type == "array"' >/dev/null 2>&1; then
        log_fail "Higress consumers response during ${label} is not valid JSON with a data array" >&2
        return 1
    fi

    HIGRESS_CONSUMERS_JSON="${consumers}"
}

# ============================================================
# Section 1: Snapshot Higress consumers before any create calls
# ============================================================
log_section "Snapshot Initial State"

HIGRESS_CONSUMERS_JSON=""
if _get_higress_consumers_or_fail "initial snapshot"; then
    CONSUMERS_BEFORE="${HIGRESS_CONSUMERS_JSON}"
    log_info "Higress consumer count before: $(echo "${CONSUMERS_BEFORE}" | jq -r '.data | length // 0' 2>/dev/null || echo "?")"
fi

# ============================================================
# Section 2: Bad names — assert CLI rejects with expected error
# ============================================================
log_section "Reject Invalid Names"

# (label, name, expected error substring)
# Empty name is reported by the `--name is required` check BEFORE
# validateWorkerName runs, so its expected substring differs.
INVALID_CASES=(
    "uppercase|Alice|invalid worker name"
    "underscore|alice_dev|invalid worker name"
    "leading-hyphen|-alice|invalid worker name"
    "special-char|alice!|invalid worker name"
    "empty||--name is required"
)

for case_entry in "${INVALID_CASES[@]}"; do
    label="${case_entry%%|*}"
    rest="${case_entry#*|}"
    bad_name="${rest%%|*}"
    expected_substr="${rest#*|}"

    # Use --no-wait so a regression (validation accidentally accepting)
    # does not block this test for 3 minutes on the readiness poll.
    OUTPUT=$(exec_in_agent hiclaw create worker --name "${bad_name}" --no-wait 2>&1)
    EXIT_CODE=$?

    if [ "${EXIT_CODE}" -ne 0 ]; then
        log_pass "CLI rejected ${label} name (exit=${EXIT_CODE}): '${bad_name}'"
    else
        log_fail "CLI accepted invalid ${label} name '${bad_name}' (exit=0)"
        if [ -n "${bad_name}" ]; then
            ACCEPTED_INVALID_NAMES+=("${bad_name}")
            exec_in_agent hiclaw delete worker "${bad_name}" 2>/dev/null || true
            docker rm -f "hiclaw-worker-${bad_name}" 2>/dev/null || true
        fi
    fi

    if echo "${OUTPUT}" | grep -q -- "${expected_substr}"; then
        log_pass "Error message for ${label} contains '${expected_substr}'"
    else
        log_fail "Error message for ${label} missing '${expected_substr}' (got: ${OUTPUT})"
    fi
done

# ============================================================
# Section 3: Confirm no Higress consumer leaked for invalid names
# ============================================================
log_section "Verify No Higress Leak"

HIGRESS_CONSUMERS_JSON=""
LEAKED=""
if _get_higress_consumers_or_fail "invalid-name leak check"; then
    CONSUMERS_AFTER="${HIGRESS_CONSUMERS_JSON}"
    for case_entry in "${INVALID_CASES[@]}"; do
        rest="${case_entry#*|}"
        bad_name="${rest%%|*}"
        [ -z "${bad_name}" ] && continue
        if echo "${CONSUMERS_AFTER}" | jq -r '.data[]?.name // empty' 2>/dev/null \
            | grep -Fxq "worker-${bad_name}"; then
            LEAKED="${LEAKED} ${bad_name}"
        fi
    done

    if [ -z "${LEAKED}" ]; then
        log_pass "No Higress consumer created for any rejected name"
    else
        log_fail "Higress consumers leaked for invalid names:${LEAKED}"
    fi
fi

# ============================================================
# Section 4: Positive case — valid name accepted
# ============================================================
log_section "Accept Valid Name"

OUTPUT=$(exec_in_agent hiclaw create worker --name "${TEST_VALID_NAME}" --no-wait 2>&1)
EXIT_CODE=$?

if [ "${EXIT_CODE}" -eq 0 ]; then
    log_pass "CLI accepted valid name: ${TEST_VALID_NAME} (exit=0)"
else
    log_fail "CLI rejected valid name '${TEST_VALID_NAME}' (exit=${EXIT_CODE}, output: ${OUTPUT})"
fi

if echo "${OUTPUT}" | grep -qi "accepted\|created\|ready"; then
    log_pass "Create output reports acceptance"
else
    log_info "Create output: ${OUTPUT}"
fi

# ============================================================
# Summary
# ============================================================
test_teardown "25-name-validation"
test_summary
