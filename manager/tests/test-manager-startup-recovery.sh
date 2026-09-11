#!/bin/bash
# Regression tests for Manager startup Worker recovery ownership.

set -uo pipefail

PASS=0
FAIL=0
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
STARTUP_SCRIPT="${PROJECT_ROOT}/manager/scripts/init/start-manager-agent.sh"

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; echo "       expected: $2"; echo "       got:      $3"; FAIL=$((FAIL + 1)); }

assert_contains() {
    local desc="$1" needle="$2" haystack="$3"
    if printf '%s\n' "${haystack}" | grep -qF -- "${needle}"; then
        pass "${desc}"
    else
        fail "${desc}" "contains '${needle}'" "not found"
    fi
}

assert_not_contains() {
    local desc="$1" needle="$2" haystack="$3"
    if ! printf '%s\n' "${haystack}" | grep -qF -- "${needle}"; then
        pass "${desc}"
    else
        fail "${desc}" "does not contain '${needle}'" "found '${needle}'"
    fi
}

startup_source="$(<"${STARTUP_SCRIPT}")"

echo "=== Manager startup Worker recovery ownership ==="
assert_contains "startup still detects the configured container runtime" \
    'export AGENTTEAMS_CONTAINER_RUNTIME=' "${startup_source}"
assert_contains "startup still loads the Worker management API helpers" \
    'source /opt/agentteams/scripts/lib/container-api.sh' "${startup_source}"
assert_not_contains "startup no longer creates Worker CRs during recovery" \
    'worker_backend_create' "${startup_source}"
assert_not_contains "startup no longer enumerates Workers for recovery" \
    'Recreate Worker containers as needed after Manager restart.' "${startup_source}"

if bash -n "${STARTUP_SCRIPT}"; then
    pass "startup script has valid Bash syntax"
else
    fail "startup script has valid Bash syntax" "bash -n succeeds" "bash -n failed"
fi

if [ "${FAIL}" -eq 0 ]; then
    echo "All ${PASS} tests passed."
    exit 0
fi

echo "${FAIL} of $((PASS + FAIL)) tests failed."
exit 1
