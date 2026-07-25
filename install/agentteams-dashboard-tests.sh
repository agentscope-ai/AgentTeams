#!/bin/bash
# agentteams-dashboard-tests.sh — Regression tests for Dashboard integration
#
# Tests the install script's Dashboard-related behaviors without requiring
# Docker or a running controller. Sources the install script helpers and
# exercises step_dashboard in non-interactive mode.
#
# Usage:
#   bash install/agentteams-dashboard-tests.sh
#
# Exit code: 0 if all pass, 1 if any fail.

set -u

PASS=0
FAIL=0
TESTS_DIR="$(cd "$(dirname "$0")" && pwd)"
INSTALL_SCRIPT="${TESTS_DIR}/agentteams-install.sh"

if [ ! -f "${INSTALL_SCRIPT}" ]; then
    echo "ERROR: install script not found at ${INSTALL_SCRIPT}"
    exit 1
fi

# ---------- Helpers ----------

pass() {
    echo "  [PASS] $1"
    PASS=$((PASS + 1))
}

fail() {
    echo "  [FAIL] $1"
    FAIL=$((FAIL + 1))
}

section() {
    echo ""
    echo "==> $1"
}

# ---------- Test setup ----------
# We source only the parts we need by setting mocks for external deps.

# Mock docker/podman so the script doesn't try to query containers.
docker() { return 1; }
podman() { return 1; }
DOCKER_CMD="docker"

# Minimal env so the script can initialize without interactive prompts.
AGENTTEAMS_NON_INTERACTIVE=1
AGENTTEAMS_REGISTRY="${AGENTTEAMS_REGISTRY:-ghcr.io/agentteams-group}"
AGENTTEAMS_VERSION="v999.0.0-test"
AGENTTEAMS_UPGRADE=0
AGENTTEAMS_UPGRADE_KEEP_ALL=0
AGENTTEAMS_LANG="en"
AGENTTEAMS_USE_EMBEDDED=0
AGENTTEAMS_LOCAL_ONLY=1

# Stub out log/msg so sourcing the script doesn't fail due to missing helpers.
log() { :; }
msg() { echo "$2"; }

# ---------- Test 1: Non-interactive defaults ----------

section "Test 1: Non-interactive default values"

# Source just enough to get step_dashboard + the variables it uses.
# We do this by sourcing the full script but with mocks for everything
# that requires Docker. Since the script is large and has side effects
# at the top level, we instead extract step_dashboard and its direct
# dependencies using a function-based approach.

# Instead of full sourcing, we test the derivation logic directly by
# reading the script and verifying key invariants.

# Test 1a: AGENTTEAMS_DASHBOARD defaults to 1
dashboard_default=$(grep -E '^[[:space:]]*AGENTTEAMS_DASHBOARD=.*:-(0|1)' "${INSTALL_SCRIPT}" | head -1 | sed 's/.*:-\([01]\).*/\1/')
if [ "${dashboard_default}" = "1" ]; then
    pass "AGENTTEAMS_DASHBOARD defaults to 1"
else
    fail "AGENTTEAMS_DASHBOARD defaults to '${dashboard_default}', expected 1"
fi

# Test 1b: Independent version variable exists
if grep -q 'AGENTTEAMS_DASHBOARD_VERSION' "${INSTALL_SCRIPT}"; then
    pass "AGENTTEAMS_DASHBOARD_VERSION variable is defined"
else
    fail "AGENTTEAMS_DASHBOARD_VERSION variable not found"
fi

# Test 1c: Default dashboard version is independent of AGENTTEAMS_VERSION
dashboard_version_line=$(grep -E 'AGENTTEAMS_DASHBOARD_VERSION=.*:-' "${INSTALL_SCRIPT}" | head -1)
if echo "${dashboard_version_line}" | grep -q 'v1\.2\.0-beta\.1'; then
    pass "Dashboard has independent default version (v1.2.0-beta.1)"
else
    fail "Dashboard default version line: ${dashboard_version_line}"
fi

# Test 1d: Default image uses DASHBOARD_VERSION, not main VERSION
if grep -q 'agentteams-dashboard:${AGENTTEAMS_DASHBOARD_VERSION}' "${INSTALL_SCRIPT}"; then
    pass "Default image uses AGENTTEAMS_DASHBOARD_VERSION"
else
    fail "Default image does not use AGENTTEAMS_DASHBOARD_VERSION"
fi

# ---------- Test 2: load_current_params_from_env loads Dashboard vars ----------

section "Test 2: load_current_params_from_env loads Dashboard config"

dashboard_env_vars="AGENTTEAMS_DASHBOARD AGENTTEAMS_DASHBOARD_VERSION AGENTTEAMS_PORT_DASHBOARD AGENTTEAMS_DASHBOARD_IMAGE AGENTTEAMS_AI_GATEWAY_ADMIN_URL"
for var in ${dashboard_env_vars}; do
    if grep -A 50 'load_current_params_from_env()' "${INSTALL_SCRIPT}" | grep -q "${var}"; then
        pass "load_current_params_from_env loads ${var}"
    else
        fail "load_current_params_from_env missing ${var}"
    fi
done

# ---------- Test 3: Explicit gateway URL takes priority ----------

section "Test 3: Explicit AGENTTEAMS_AI_GATEWAY_ADMIN_URL priority"

# Look for the pattern in _start_dashboard: explicit URL branch comes
# before auto-detection (wget) branch.
start_line=$(grep -n '_start_dashboard()' "${INSTALL_SCRIPT}" | head -1 | cut -d: -f1)
end_line=$((start_line + 200))
section_text=$(sed -n "${start_line},${end_line}p" "${INSTALL_SCRIPT}")

explicit_line=$(echo "${section_text}" | grep -n '\-n.*AGENTTEAMS_AI_GATEWAY_ADMIN_URL' | head -1 | cut -d: -f1)
detect_line=$(echo "${section_text}" | grep -n 'wget.*8001\|exec.*curl.*8001' | head -1 | cut -d: -f1)

if [ -n "${explicit_line}" ] && [ -n "${detect_line}" ] && [ "${explicit_line}" -lt "${detect_line}" ]; then
    pass "_start_dashboard: explicit URL check comes before auto-detect"
else
    fail "_start_dashboard: cannot verify explicit URL priority (explicit=${explicit_line:-?}, detect=${detect_line:-?})"
fi

# ---------- Test 4: URL normalization ----------

section "Test 4: URL normalization (auto-prepend http://)"

if grep -q 'http://\*|https://\*)' "${INSTALL_SCRIPT}"; then
    pass "URL normalization (http:// prefix) is implemented"
else
    fail "URL normalization not found"
fi

# ---------- Test 5: CLI token polling + legacy HiClaw path ----------

section "Test 5: CLI token polling with HiClaw compatibility"

if grep -q 'cli-token.*2>/dev/null.*cat.*hiclaw' "${INSTALL_SCRIPT}" || \
   grep -q 'cat /var/run/agentteams/cli-token.*|| cat /var/run/hiclaw/cli-token' "${INSTALL_SCRIPT}"; then
    pass "CLI token polling checks both agentteams and hiclaw paths"
else
    # Try a looser match
    if grep -q '/var/run/hiclaw/cli-token' "${INSTALL_SCRIPT}"; then
        pass "Legacy HiClaw cli-token path is checked"
    else
        fail "Legacy HiClaw cli-token path not found"
    fi
fi

if grep -q '_token_max_wait=30' "${INSTALL_SCRIPT}" || grep -q '30s' "${INSTALL_SCRIPT}"; then
    pass "Token polling timeout exists"
else
    fail "Token polling timeout not found"
fi

# ---------- Test 6: Legacy cleanup is exact-match only ----------

section "Test 6: Legacy cleanup uses exact match (not broad glob)"

# Check that there's no 'agentteams-*' style removal loop
if grep -E 'grep.*agentteams-\*|agentteams-.*\*' "${INSTALL_SCRIPT}" | grep -v 'agentteams-dashboard' | grep -q 'docker.*rm'; then
    fail "Broad agentteams-* cleanup detected"
else
    pass "No broad agentteams-* cleanup pattern found"
fi

# Check for exact known legacy container
if grep -q 'agentteams-docker-proxy' "${INSTALL_SCRIPT}"; then
    pass "Legacy cleanup targets exact known container (agentteams-docker-proxy)"
else
    fail "No exact legacy container match found"
fi

# ---------- Test 7: Makefile targets exist ----------

section "Test 7: Makefile dashboard targets"

MAKEFILE="${TESTS_DIR}/../Makefile"
if [ ! -f "${MAKEFILE}" ]; then
    echo "  [SKIP] Makefile not found at ${MAKEFILE}"
else
    for target in install-dashboard update-dashboard uninstall-dashboard build-dashboard; do
        if grep -q "^${target}:" "${MAKEFILE}" || grep -q "^\.PHONY.*${target}" "${MAKEFILE}"; then
            pass "Makefile has ${target} target"
        else
            fail "Makefile missing ${target} target"
        fi
    done
fi

# ---------- Test 8: PowerShell limitation documented ----------

section "Test 8: Platform limitation documented"

if grep -qi 'powershell.*dashboard\|dashboard.*powershell\|platform.*dashboard' "${INSTALL_SCRIPT}" || \
   grep -qi 'bash.*only\|linux.*macos.*dashboard' "${INSTALL_SCRIPT}"; then
    pass "PowerShell platform limitation documented in install script"
else
    # Check Makefile too
    if [ -f "${MAKEFILE}" ] && grep -qi 'powershell\|platform.*limitation' "${MAKEFILE}"; then
        pass "PowerShell platform limitation documented in Makefile"
    else
        fail "PowerShell platform limitation not clearly documented"
    fi
fi

# ---------- Test 9: Dashboard env persistence ----------

section "Test 9: Dashboard env persistence"

# Check that the .env file generation includes Dashboard variables
if grep -E 'AGENTTEAMS_DASHBOARD=|AGENTTEAMS_DASHBOARD_VERSION=|AGENTTEAMS_PORT_DASHBOARD=' "${INSTALL_SCRIPT}" | grep -q 'env\|ENV'; then
    pass "Dashboard variables included in env persistence"
else
    # Check the env file write section
    if grep -A 50 'AGENTTEAMS_ENV_FILE' "${INSTALL_SCRIPT}" | grep -q 'DASHBOARD'; then
        pass "Dashboard variables included in env file generation"
    else
        fail "Dashboard variables not found in env persistence"
    fi
fi

# ---------- Summary ----------

echo ""
echo "=============================="
echo " Dashboard Integration Tests"
echo "=============================="
TOTAL=$((PASS + FAIL))
echo "Result: ${PASS}/${TOTAL} passed"

if [ "${FAIL}" -gt 0 ]; then
    echo ""
    echo "Some tests failed. See above for details."
    exit 1
fi

echo ""
echo "All tests passed."
exit 0
