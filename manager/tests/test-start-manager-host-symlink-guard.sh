#!/bin/bash
# test-start-manager-host-symlink-guard.sh
# Unit-level test for the host-symlink guard in start-manager-agent.sh
# (Finding #9): when HOST_ORIGINAL_HOME is not an absolute POSIX path
# (e.g. a raw Windows path like "C:\Users\foo" leaked through from a
# Windows host installer), the script must fall through to the
# /root/host-home fallback symlink instead of attempting to create a
# symlink at a bogus "C:\..." path.
#
# This test exercises the guard logic directly (mirroring the case
# statement in start-manager-agent.sh) against a temp directory acting
# as "/host-share", without requiring root, Docker, or the real
# container filesystem layout.
#
# Usage: bash manager/tests/test-start-manager-host-symlink-guard.sh

set -uo pipefail

PASS=0
FAIL=0
TMPDIR_ROOT=$(mktemp -d)
trap 'rm -rf "${TMPDIR_ROOT}"' EXIT

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
TARGET_SCRIPT="${PROJECT_ROOT}/manager/scripts/init/start-manager-agent.sh"

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; echo "       expected: $2"; echo "       got:      $3"; FAIL=$((FAIL + 1)); }

assert_eq() {
    local desc="$1" expected="$2" actual="$3"
    if [ "${expected}" = "${actual}" ]; then
        pass "${desc}"
    else
        fail "${desc}" "${expected}" "${actual}"
    fi
}

# Sanity: the guard must exist in the real script (fails loudly if the
# fix regresses / is refactored away without updating this test).
if ! grep -q '# Not an absolute POSIX path' "${TARGET_SCRIPT}"; then
    echo "FATAL: expected POSIX-path guard comment not found in ${TARGET_SCRIPT}"
    exit 1
fi

# Reimplementation of the guarded symlink decision from
# start-manager-agent.sh (local mode block). Returns, via global
# RESULT_SYMLINK_TARGET, which path the real script would symlink to,
# without actually touching /root or /host-share.
run_guard() {
    local ORIGINAL_HOST_HOME="$1"
    local host_share_dir="$2"
    RESULT_SYMLINK_TARGET=""
    case "${ORIGINAL_HOST_HOME}" in
        /*)
            if [ ! -e "${ORIGINAL_HOST_HOME}" ] && [ "${ORIGINAL_HOST_HOME}" != "/" ] && [ "${ORIGINAL_HOST_HOME}" != "/root" ] && [ "${ORIGINAL_HOST_HOME}" != "/data" ] && [ "${ORIGINAL_HOST_HOME}" != "/host-share" ]; then
                RESULT_SYMLINK_TARGET="${ORIGINAL_HOST_HOME}"
            else
                RESULT_SYMLINK_TARGET="/root/host-home"
            fi
            ;;
        *)
            RESULT_SYMLINK_TARGET="/root/host-home"
            ;;
    esac
}

echo "Test: absolute POSIX path that does not exist -> symlink created at that path"
run_guard "${TMPDIR_ROOT}/does-not-exist-yet" "${TMPDIR_ROOT}"
assert_eq "absolute nonexistent path uses itself as target" "${TMPDIR_ROOT}/does-not-exist-yet" "${RESULT_SYMLINK_TARGET}"

echo "Test: absolute POSIX path that already exists -> fallback symlink"
run_guard "${TMPDIR_ROOT}" "${TMPDIR_ROOT}"
assert_eq "existing absolute path falls back" "/root/host-home" "${RESULT_SYMLINK_TARGET}"

echo "Test: reserved absolute paths -> fallback symlink"
for reserved in "/" "/root" "/data" "/host-share"; do
    run_guard "${reserved}" "${TMPDIR_ROOT}"
    assert_eq "reserved path '${reserved}' falls back" "/root/host-home" "${RESULT_SYMLINK_TARGET}"
done

echo "Test: raw Windows path (no leading /) -> fallback symlink, not a bogus link"
run_guard 'C:\Users\someone' "${TMPDIR_ROOT}"
assert_eq "raw Windows path falls back instead of symlinking to itself" "/root/host-home" "${RESULT_SYMLINK_TARGET}"

echo "Test: Windows path with forward slashes (still not POSIX-absolute) -> fallback"
run_guard 'C:/Users/someone' "${TMPDIR_ROOT}"
assert_eq "drive-letter path falls back instead of symlinking to itself" "/root/host-home" "${RESULT_SYMLINK_TARGET}"

echo ""
echo "Results: ${PASS} passed, ${FAIL} failed"
[ "${FAIL}" -eq 0 ]
