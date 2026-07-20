#!/bin/bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HELPER="${HELPER:-${SCRIPT_DIR}/../lib/test-helpers.sh}"
ORCHESTRATOR="${ORCHESTRATOR:-${SCRIPT_DIR}/../run-all-tests.sh}"

assert_token() {
    local expected="$1"
    local test_token="$2"
    local agentteams_token="$3"
    local legacy_token="$4"
    local actual

    actual=$(TEST_GITHUB_TOKEN="${test_token}" \
        AGENTTEAMS_GITHUB_TOKEN="${agentteams_token}" \
        HICLAW_GITHUB_TOKEN="${legacy_token}" \
        HELPER="${HELPER}" \
        bash -c 'docker() { return 1; }; source "${HELPER}"; printf "%s" "${TEST_GITHUB_TOKEN}"')

    if [ "${actual}" != "${expected}" ]; then
        echo "FAIL: expected token ${expected}, got ${actual:-<empty>}" >&2
        exit 1
    fi
}

assert_token canonical '' canonical legacy
assert_token explicit explicit canonical legacy
assert_token legacy '' '' legacy

if ! grep -q 'AGENTTEAMS_GITHUB_TOKEN)' "${ORCHESTRATOR}"; then
    echo "FAIL: orchestrator does not load AGENTTEAMS_GITHUB_TOKEN from the env file" >&2
    exit 1
fi

echo "PASS: GitHub test token honors explicit, AgentTeams, and legacy inputs"
