#!/usr/bin/env bash
# Regression for the pin assertion inside the Control UI device-identity
# probe (manager/tests/test-control-ui-device-identity.sh), #1479.
#
# The probe must FAIL when the image's OpenClaw version differs from the
# pinned tag, PASS when it matches, and only NOTE when it cannot be read.
# This test sources the probe file with a stubbed `docker` (so no gateway is
# ever started) and drives assert_pinned_version() through all three
# outcomes.
#
# Counters are split on purpose: probe-internal results (pass/fail called
# inside assert_pinned_version) count in PROBE_PASS/PROBE_FAIL, which
# run_assert() resets around each case; suite-level results count in
# PASS/FAIL via suite_pass/suite_fail. One shared pair let a later case's
# reset erase an earlier test-level failure, so the script could print FAIL
# yet still exit 0. Case 4 proves a sabotaged first fixture keeps the final
# exit nonzero.
#
# It never runs real Docker.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROBE="${SCRIPT_DIR}/test-control-ui-device-identity.sh"

echo "=== Control UI probe pin assertion (stubbed docker) ==="

if [ ! -f "${PROBE}" ]; then
  echo "  FAIL: probe exists"
  echo "       ${PROBE} not found"
  echo "SUMMARY: 0 passed, 1 failed"
  exit 1
fi

# The probe defines pass/fail/skip/note and the PASS/FAIL/SKIP counters, and
# assert_pinned_version() reports through pass/fail. After the source below
# this test redefines pass/fail to count per-probe results and adds
# suite_pass/suite_fail for its own assertions, so the two result layers
# never share a counter. The `docker` stub answers only the
# `docker run --rm ...` version-read call; everything else (cleanup's rm
# calls) just returns 0.
STUB_GATEWAY_VERSION=""
docker() {
  if [ "$1" = "run" ] && [ "$2" = "--rm" ]; then
    printf '%s' "${STUB_GATEWAY_VERSION}"
  fi
  return 0
}

source "${PROBE}"

# Counter separation (see header): probe-internal pass/fail count per-probe
# results; the suite-level assertions use their own pair. The display format
# stays the probe's.
PROBE_PASS=0
PROBE_FAIL=0
pass() { echo "  PASS: $1"; PROBE_PASS=$((PROBE_PASS + 1)); }
fail() { echo "  FAIL: $1"; echo "       $2"; PROBE_FAIL=$((PROBE_FAIL + 1)); }
suite_pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
suite_fail() { echo "  FAIL: $1"; echo "       $2"; FAIL=$((FAIL + 1)); }

OUT_FILE="$(mktemp)"
# The probe armed `trap cleanup EXIT` when sourced; re-arm it so both the
# probe's temp dir and our output file are removed.
trap 'rm -f "${OUT_FILE}"; cleanup' EXIT

EXPECTED="$(openclaw_expected_version)"
NEXP="$(openclaw_version_normalize "${EXPECTED}")"
if [ -z "${NEXP}" ]; then
  suite_fail "pinned version resolvable" "neither AGENTTEAMS_OPENCLAW_TAG nor hack/versions.env is available"
  echo "SUMMARY: ${PASS} passed, ${FAIL} failed"
  exit 1
fi

# Runs assert_pinned_version in THIS shell (a command substitution would lose
# the counter increments), captures its output, and snapshots the per-probe
# counters so each case is asserted independently. run_assert only ever
# touches the per-probe pair — the suite PASS/FAIL counters survive from
# case to case, so a test-level failure recorded in an earlier case is still
# in the final SUMMARY (and the exit code).
run_assert() {
  PROBE_PASS=0
  PROBE_FAIL=0
  assert_pinned_version > "${OUT_FILE}"
  OUT="$(cat "${OUT_FILE}")"
  A_PASS=${PROBE_PASS}
  A_FAIL=${PROBE_FAIL}
}

# 1) image version == pin -> PASS. Sabotage mode forces a mismatching
# fixture instead, so case 1 records a suite-level failure that must
# survive to the final exit code (case 4 drives this mode in a subprocess).
if [ "${CONTROL_UI_ASSERT_SABOTAGE_CASE1:-0}" = "1" ]; then
  STUB_GATEWAY_VERSION="1999.9.9"
else
  STUB_GATEWAY_VERSION="${NEXP}"
fi
run_assert
if [ "${A_PASS}" -eq 1 ] && [ "${A_FAIL}" -eq 0 ] \
   && printf '%s' "${OUT}" | grep -q "PASS: gateway version matches the pinned OpenClaw tag (${NEXP})"; then
  suite_pass "matching image version records a PASS"
else
  suite_fail "matching image version records a PASS" "out=${OUT}"
fi

# 2) image version != pin -> FAIL with both versions named.
STUB_GATEWAY_VERSION="1999.1.1"
run_assert
if [ "${A_PASS}" -eq 0 ] && [ "${A_FAIL}" -eq 1 ] \
   && printf '%s' "${OUT}" | grep -q 'FAIL: gateway version matches the pinned OpenClaw tag' \
   && printf '%s' "${OUT}" | grep -q "expected ${NEXP}, image has 1999.1.1"; then
  suite_pass "stale image version records a FAIL with both versions"
else
  suite_fail "stale image version records a FAIL with both versions" "out=${OUT}"
fi

# 3) version unreadable -> NOTE only, neither pass nor fail.
STUB_GATEWAY_VERSION=""
run_assert
if [ "${A_PASS}" -eq 0 ] && [ "${A_FAIL}" -eq 0 ] \
   && printf '%s' "${OUT}" | grep -q 'NOTE: gateway version unknown'; then
  suite_pass "unreadable version notes without failing"
else
  suite_fail "unreadable version notes without failing" "out=${OUT}"
fi

# 4) self-test: a fresh run with case 1 sabotaged must end nonzero with the
# failure visible — the regression for the shared-counter erasure. The
# subprocess inherits the sabotage variable, so its own case 4 is skipped:
# no recursion.
if [ -z "${CONTROL_UI_ASSERT_SABOTAGE_CASE1:-}" ]; then
  SABOTAGE_OUT="$(env CONTROL_UI_ASSERT_SABOTAGE_CASE1=1 bash "${SCRIPT_DIR}/test-control-ui-version-assert.sh" 2>&1)"
  SABOTAGE_RC=$?
  if [ "${SABOTAGE_RC}" -ne 0 ] \
     && printf '%s' "${SABOTAGE_OUT}" | grep -q "FAIL: matching image version records a PASS"; then
    suite_pass "a sabotaged first fixture ends the run nonzero (rc=${SABOTAGE_RC})"
  else
    suite_fail "a sabotaged first fixture ends the run nonzero (rc=${SABOTAGE_RC})" "out=${SABOTAGE_OUT}"
  fi
else
  skip "sabotage self-test (this run is itself sabotaged)"
fi

echo "SUMMARY: ${PASS} passed, ${FAIL} failed"
[ "${FAIL}" -eq 0 ] || exit 1
exit 0
