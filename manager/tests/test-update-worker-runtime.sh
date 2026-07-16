#!/bin/bash
# Regression tests for runtime-switch arguments in update-worker-config.sh.

set -uo pipefail

PASS=0
FAIL=0
TMPDIR_ROOT=$(mktemp -d)
trap 'rm -rf "${TMPDIR_ROOT}"' EXIT

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
UPDATE_SCRIPT="${PROJECT_ROOT}/manager/agent/skills/worker-management/scripts/update-worker-config.sh"
TEST_SCRIPT="${TMPDIR_ROOT}/update-worker-config.sh"
MOCK_BIN="${TMPDIR_ROOT}/bin"
HICLAW_LOG="${TMPDIR_ROOT}/hiclaw.log"

pass() {
    echo "  PASS: $1"
    PASS=$((PASS + 1))
}

fail() {
    echo "  FAIL: $1"
    echo "       expected: $2"
    echo "       got:      $3"
    FAIL=$((FAIL + 1))
}

assert_eq() {
    local description="$1" expected="$2" actual="$3"
    if [ "${expected}" = "${actual}" ]; then
        pass "${description}"
    else
        fail "${description}" "${expected}" "${actual}"
    fi
}

assert_contains() {
    local description="$1" needle="$2" actual="$3"
    if printf '%s\n' "${actual}" | grep -qF -- "${needle}"; then
        pass "${description}"
    else
        fail "${description}" "contains ${needle}" "${actual}"
    fi
}

# The production source only exports shared environment helpers. Runtime mode
# does not need them, so replace that source line in the isolated test copy.
sed 's|^source /opt/hiclaw/scripts/lib/hiclaw-env.sh$|: # environment supplied by test|' \
    "${UPDATE_SCRIPT}" > "${TEST_SCRIPT}"
chmod +x "${TEST_SCRIPT}"

mkdir -p "${MOCK_BIN}"
cat > "${MOCK_BIN}/hiclaw" <<'EOF'
#!/bin/sh
case "$1 $2" in
    "update worker")
        printf '%s\n' "$@" > "${TEST_HICLAW_LOG:?}"
        echo "worker updated"
        ;;
    "get workers")
        printf '%s\n' '{"workers":[{"name":"alice","phase":"Running","runtime":"hermes"}]}'
        ;;
    *)
        echo "unexpected hiclaw command: $*" >&2
        exit 2
        ;;
esac
EOF
chmod +x "${MOCK_BIN}/hiclaw"

echo "=== Runtime switch rejects the unsupported MCP combination ==="
if REJECT_OUTPUT=$(PATH="${MOCK_BIN}:${PATH}" \
    TEST_HICLAW_LOG="${HICLAW_LOG}" \
    bash "${TEST_SCRIPT}" --name alice --runtime hermes --mcp-servers github 2>&1); then
    fail "runtime and MCP options are rejected before the CLI call" \
        "non-zero exit" "exit 0"
else
    assert_contains "rejection explains the incompatible options" \
        "--mcp-servers cannot be combined with --runtime" "${REJECT_OUTPUT}"
fi

if [ -e "${HICLAW_LOG}" ]; then
    fail "rejected options do not invoke hiclaw" "no command log" "$(cat "${HICLAW_LOG}")"
else
    pass "rejected options do not invoke hiclaw"
fi

echo "=== Runtime switch forwards supported update flags unchanged ==="
RUNTIME_OUTPUT=$(PATH="${MOCK_BIN}:${PATH}" \
    TEST_HICLAW_LOG="${HICLAW_LOG}" \
    bash "${TEST_SCRIPT}" \
        --name alice \
        --runtime hermes \
        --model qwen3.6-plus \
        --skills code-review)

EXPECTED_ARGS=$(printf '%s\n' \
    update worker \
    --name alice \
    --runtime hermes \
    --model qwen3.6-plus \
    --skills code-review)
assert_eq "supported runtime arguments match hiclaw update worker" \
    "${EXPECTED_ARGS}" "$(cat "${HICLAW_LOG}")"
assert_contains "runtime switch still reports completion" \
    '"status": "runtime_switched"' "${RUNTIME_OUTPUT}"

echo ""
echo "Results: ${PASS} passed, ${FAIL} failed"
if [ "${FAIL}" -gt 0 ]; then
    exit 1
fi
