#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
INSTALL_SCRIPT="${INSTALL_SCRIPT:-${ROOT_DIR}/install/hiclaw-install.sh}"

# Load only the routing helper; sourcing the full installer would start Docker.
source <(sed -n '/^should_skip_step() {/,/^}/p' "${INSTALL_SCRIPT}")

PASS=0
FAIL=0

pass() {
    printf 'PASS: %s\n' "$1"
    PASS=$((PASS + 1))
}

fail() {
    printf 'FAIL: %s\n' "$1" >&2
    FAIL=$((FAIL + 1))
}

assert_skipped() {
    local step="$1"
    local context="$2"
    if should_skip_step "${step}"; then
        pass "${context}: ${step} is skipped"
    else
        fail "${context}: ${step} should be skipped"
    fi
}

assert_runs() {
    local step="$1"
    local context="$2"
    if should_skip_step "${step}"; then
        fail "${context}: ${step} should run"
    else
        pass "${context}: ${step} runs"
    fi
}

reset_mode() {
    AGENTTEAMS_NON_INTERACTIVE=0
    AGENTTEAMS_QUICKSTART=0
    AGENTTEAMS_UPGRADE=0
    AGENTTEAMS_UPGRADE_KEEP_ALL=0
    AGENTTEAMS_USE_EMBEDDED=0
    AGENTTEAMS_ENV_FILE="${ROOT_DIR}/install/tests/nonexistent.env"
    DOCKER_CMD=docker
}

reset_mode
AGENTTEAMS_QUICKSTART=1
for step in step_e2ee step_idle step_docker_proxy step_hostshare; do
    assert_skipped "${step}" "quick start"
done
assert_runs step_manager_runtime "quick start"

reset_mode
AGENTTEAMS_NON_INTERACTIVE=1
for step in step_manager_runtime step_e2ee step_idle step_docker_proxy step_hostshare; do
    assert_skipped "${step}" "non-interactive"
done

reset_mode
AGENTTEAMS_USE_EMBEDDED=1
assert_skipped step_docker_proxy "embedded"

reset_mode
AGENTTEAMS_UPGRADE=1
AGENTTEAMS_UPGRADE_KEEP_ALL=1
for step in \
    step_llm step_admin step_network step_ports step_domains step_github \
    step_skills step_runtime step_manager_runtime step_e2ee step_docker_proxy \
    step_idle step_hostshare; do
    assert_skipped "${step}" "keep-all upgrade"
done

reset_mode
for step in step_manager_runtime step_e2ee step_idle step_docker_proxy step_hostshare; do
    assert_runs "${step}" "manual"
done

printf '\nResults: %d passed, %d failed\n' "${PASS}" "${FAIL}"
if [ "${FAIL}" -ne 0 ]; then
    exit 1
fi
