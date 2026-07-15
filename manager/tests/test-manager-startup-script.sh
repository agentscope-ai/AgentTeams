#!/bin/bash
# Regression tests for manager/scripts/init/start-manager-agent.sh.
#
# Usage: bash manager/tests/test-manager-startup-script.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
START_SCRIPT="${PROJECT_ROOT}/manager/scripts/init/start-manager-agent.sh"

PASS=0
FAIL=0

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; echo "       $2"; FAIL=$((FAIL + 1)); }

echo ""
echo "=== TC1: startup script has valid bash syntax ==="
if bash -n "${START_SCRIPT}"; then
    pass "bash -n start-manager-agent.sh"
else
    fail "bash -n start-manager-agent.sh" "syntax check failed"
fi

echo ""
echo "=== TC2: Worker restart recovery block has no top-level local declarations ==="
bad_lines=$(
    awk '
      /Recreate Worker containers as needed after Manager restart/ { in_block = 1; next }
      in_block && /Builtin files \(AGENTS.md, skills\)/ { in_block = 0 }
      in_block && /^[[:space:]]*local[[:space:]]/ { print NR ":" $0 }
    ' "${START_SCRIPT}"
)
if [ -z "${bad_lines}" ]; then
    pass "no local declarations in top-level restart recovery block"
else
    fail "no local declarations in top-level restart recovery block" "${bad_lines}"
fi

echo ""
echo "=== Summary ==="
echo "PASS: ${PASS}"
echo "FAIL: ${FAIL}"

if [ "${FAIL}" -ne 0 ]; then
    exit 1
fi
