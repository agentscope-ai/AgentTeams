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
# assert_pinned_version() reports through them - the test reuses those as-is.
# The `docker` stub answers only the `docker run --rm ...` version-read call;
# everything else (cleanup's rm calls) just returns 0.
STUB_GATEWAY_VERSION=""
docker() {
  if [ "$1" = "run" ] && [ "$2" = "--rm" ]; then
    printf '%s' "${STUB_GATEWAY_VERSION}"
  fi
  return 0
}

source "${PROBE}"

OUT_FILE="$(mktemp)"
# The probe armed `trap cleanup EXIT` when sourced; re-arm it so both the
# probe's temp dir and our output file are removed.
trap 'rm -f "${OUT_FILE}"; cleanup' EXIT

EXPECTED="$(openclaw_expected_version)"
NEXP="$(openclaw_version_normalize "${EXPECTED}")"
if [ -z "${NEXP}" ]; then
  fail "pinned version resolvable" "neither AGENTTEAMS_OPENCLAW_TAG nor hack/versions.env is available"
  echo "SUMMARY: ${PASS} passed, ${FAIL} failed"
  exit 1
fi

# Runs assert_pinned_version in THIS shell (a command substitution would lose
# the PASS/FAIL increments), captures its output, then resets the probe
# counters so each case is asserted independently and the final SUMMARY
# counts test-level results only.
run_assert() {
  PASS=0
  FAIL=0
  assert_pinned_version > "${OUT_FILE}"
  OUT="$(cat "${OUT_FILE}")"
  A_PASS=${PASS}
  A_FAIL=${FAIL}
  PASS=0
  FAIL=0
}

# 1) image version == pin -> PASS.
STUB_GATEWAY_VERSION="${NEXP}"
run_assert
if [ "${A_PASS}" -eq 1 ] && [ "${A_FAIL}" -eq 0 ] \
   && printf '%s' "${OUT}" | grep -q "PASS: gateway version matches the pinned OpenClaw tag (${NEXP})"; then
  pass "matching image version records a PASS"
else
  fail "matching image version records a PASS" "out=${OUT}"
fi

# 2) image version != pin -> FAIL with both versions named.
STUB_GATEWAY_VERSION="1999.1.1"
run_assert
if [ "${A_PASS}" -eq 0 ] && [ "${A_FAIL}" -eq 1 ] \
   && printf '%s' "${OUT}" | grep -q 'FAIL: gateway version matches the pinned OpenClaw tag' \
   && printf '%s' "${OUT}" | grep -q "expected ${NEXP}, image has 1999.1.1"; then
  pass "stale image version records a FAIL with both versions"
else
  fail "stale image version records a FAIL with both versions" "out=${OUT}"
fi

# 3) version unreadable -> NOTE only, neither pass nor fail.
STUB_GATEWAY_VERSION=""
run_assert
if [ "${A_PASS}" -eq 0 ] && [ "${A_FAIL}" -eq 0 ] \
   && printf '%s' "${OUT}" | grep -q 'NOTE: gateway version unknown'; then
  pass "unreadable version notes without failing"
else
  fail "unreadable version notes without failing" "out=${OUT}"
fi

echo "SUMMARY: ${PASS} passed, ${FAIL} failed"
[ "${FAIL}" -eq 0 ] || exit 1
exit 0
