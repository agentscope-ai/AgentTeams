#!/usr/bin/env bash
# Unit tests for the OpenClaw version guard (manager/scripts/lib/
# openclaw-version-guard.sh), the #1460 follow-up in #1479.
#
# The guard must refuse a *known* version mismatch at manager startup while
# a version it cannot determine must never crash the boot. These cases cover
# both halves with stubbed inputs only:
#
#   - $OPENCLAW_PACKAGE_JSON points at a temp package.json the test controls;
#   - an `openclaw` shell function stubs the CLI fallback (no binary needed);
#   - the "expected unknown" case sources a copy of the library from an
#     image-like directory tree that has no hack/versions.env;
#   - no network, no docker.

set -uo pipefail

PASS=0
FAIL=0
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GUARD="${SCRIPT_DIR}/../scripts/lib/openclaw-version-guard.sh"

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; echo "       $2"; FAIL=$((FAIL + 1)); }

echo "=== OpenClaw version guard (unit, stubbed fs/node) ==="

if [ ! -f "${GUARD}" ]; then
  fail "guard library exists" "${GUARD} not found"
  echo "SUMMARY: ${PASS} passed, ${FAIL} failed"
  exit 1
fi
. "${GUARD}"

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT
PKG="${TMP}/package.json"

# CLI fallback stub: prints a version only when STUB_OPENCLAW_VERSION is set,
# otherwise fails like a missing binary (127). A shell function shadows any
# real openclaw on PATH, so the test is hermetic.
STUB_OPENCLAW_VERSION=""
openclaw() {
  if [ -n "${STUB_OPENCLAW_VERSION:-}" ]; then
    printf 'openclaw v%s\n' "${STUB_OPENCLAW_VERSION}"
    return 0
  fi
  return 127
}

write_pkg() {
  printf '{\n  "name": "openclaw",\n  "version": "%s"\n}\n' "$1" > "${PKG}"
}
export OPENCLAW_PACKAGE_JSON="${PKG}"
unset AGENTTEAMS_OPENCLAW_TAG || true

run_guard() { # $1=mode ; captures stdout in GUARD_OUT, rc in GUARD_RC
  GUARD_OUT="$(openclaw_version_guard "$1")"
  GUARD_RC=$?
}

# 1) installed == pinned -> silent success even in fatal mode.
write_pkg "2026.10.9"
AGENTTEAMS_OPENCLAW_TAG="2026.10.9"
run_guard fatal
if [ "${GUARD_RC}" -eq 0 ] && [ -z "${GUARD_OUT}" ]; then
  pass "matching versions pass silently in fatal mode"
else
  fail "matching versions pass silently in fatal mode" "rc=${GUARD_RC} out=${GUARD_OUT}"
fi

# 2) known mismatch, fatal mode -> refuse with FATAL naming both versions.
write_pkg "2026.10.6"
AGENTTEAMS_OPENCLAW_TAG="2026.10.9"
run_guard fatal
if [ "${GUARD_RC}" -ne 0 ] \
   && printf '%s' "${GUARD_OUT}" | grep -q 'FATAL: OpenClaw 2026.10.6 does not match the pinned 2026.10.9'; then
  pass "known mismatch in fatal mode refuses with FATAL"
else
  fail "known mismatch in fatal mode refuses with FATAL" "rc=${GUARD_RC} out=${GUARD_OUT}"
fi

# 3) same mismatch, warn mode -> WARNING, rc 0.
run_guard warn
if [ "${GUARD_RC}" -eq 0 ] \
   && printf '%s' "${GUARD_OUT}" | grep -q 'WARNING: OpenClaw 2026.10.6 does not match the pinned 2026.10.9'; then
  pass "known mismatch in warn mode warns and returns 0"
else
  fail "known mismatch in warn mode warns and returns 0" "rc=${GUARD_RC} out=${GUARD_OUT}"
fi

# 4) installed version undeterminable (no package.json, no CLI) -> WARNING,
#    rc 0: a parse failure must never kill the boot (#1460 lesson).
OPENCLAW_PACKAGE_JSON="${TMP}/does-not-exist/package.json"
STUB_OPENCLAW_VERSION=""
AGENTTEAMS_OPENCLAW_TAG="2026.10.9"
run_guard fatal
if [ "${GUARD_RC}" -eq 0 ] \
   && printf '%s' "${GUARD_OUT}" | grep -q 'cannot determine the installed OpenClaw version'; then
  pass "undeterminable installed version warns without failing"
else
  fail "undeterminable installed version warns without failing" "rc=${GUARD_RC} out=${GUARD_OUT}"
fi

# 5) CLI fallback + tag-style pin: package.json missing, `openclaw
#    --version` supplies the version, the pin carries a `v` prefix.
STUB_OPENCLAW_VERSION="2026.10.9"
AGENTTEAMS_OPENCLAW_TAG="v2026.10.9"
run_guard fatal
if [ "${GUARD_RC}" -eq 0 ] && [ -z "${GUARD_OUT}" ]; then
  pass "CLI fallback version matches a v-prefixed pin"
else
  fail "CLI fallback version matches a v-prefixed pin" "rc=${GUARD_RC} out=${GUARD_OUT}"
fi

# 6) expected version unavailable (image condition: no repo, no env) ->
#    NOTE, rc 0. Sourcing a copy from an image-shaped tree reproduces the
#    manager-container layout where hack/versions.env does not exist.
mkdir -p "${TMP}/img/opt/agentteams/scripts/lib"
cp "${GUARD}" "${TMP}/img/opt/agentteams/scripts/lib/"
OPENCLAW_PACKAGE_JSON="${PKG}"
write_pkg "2026.10.9"
STUB_OPENCLAW_VERSION=""
unset AGENTTEAMS_OPENCLAW_TAG || true
GUARD_OUT="$( . "${TMP}/img/opt/agentteams/scripts/lib/openclaw-version-guard.sh"; openclaw_version_guard fatal )"
GUARD_RC=$?
if [ "${GUARD_RC}" -eq 0 ] \
   && printf '%s' "${GUARD_OUT}" | grep -q 'NOTE: no pinned OpenClaw version available'; then
  pass "unavailable pin notes without failing (image condition)"
else
  fail "unavailable pin notes without failing (image condition)" "rc=${GUARD_RC} out=${GUARD_OUT}"
fi

echo "SUMMARY: ${PASS} passed, ${FAIL} failed"
[ "${FAIL}" -eq 0 ] || exit 1
exit 0
