#!/bin/bash
# Regression tests for the Nacos Worker template installer CLI contract.

set -uo pipefail

PASS=0
FAIL=0
TMPDIR_ROOT=$(mktemp -d)
trap 'rm -rf "${TMPDIR_ROOT}"' EXIT

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
INSTALL_SCRIPT="${PROJECT_ROOT}/manager/agent/skills/hiclaw-find-worker/scripts/install-worker-template.sh"

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

echo "=== Dry-run output contains a runnable hiclaw command ==="
DRY_RUN_OUTPUT=$(bash "${INSTALL_SCRIPT}" \
    --package-uri nacos://registry.example:8848/public/reviewer \
    --worker-name alice \
    --model qwen3.6-plus \
    --skills github-operations \
    --runtime hermes \
    --dry-run)

EXPECTED_ARGS='["apply","worker","--name","alice","--package","nacos://registry.example:8848/public/reviewer","--model","qwen3.6-plus","--skills","github-operations","--runtime","hermes"]'
assert_eq "dry-run reports only supported apply worker flags" \
    "${EXPECTED_ARGS}" \
    "$(printf '%s\n' "${DRY_RUN_OUTPUT}" | jq -c '.hiclaw_args')"

echo "=== Removed MCP override fails before invoking hiclaw ==="
if MCP_OUTPUT=$(bash "${INSTALL_SCRIPT}" \
    --package-uri nacos://registry.example:8848/public/reviewer \
    --worker-name alice \
    --mcp-servers github 2>&1); then
    fail "unsupported MCP override is rejected" "non-zero exit" "exit 0"
elif printf '%s\n' "${MCP_OUTPUT}" | grep -qF 'Unknown option: --mcp-servers'; then
    pass "unsupported MCP override is rejected"
else
    fail "unsupported MCP override explains the invalid option" \
        "Unknown option: --mcp-servers" "${MCP_OUTPUT}"
fi

echo "=== Normal install forwards supported flags unchanged ==="
MOCK_BIN="${TMPDIR_ROOT}/bin"
HICLAW_LOG="${TMPDIR_ROOT}/hiclaw.log"
mkdir -p "${MOCK_BIN}"
cat > "${MOCK_BIN}/hiclaw" <<'EOF'
#!/bin/sh
printf '%s\n' "$@" > "${TEST_HICLAW_LOG:?}"
EOF
chmod +x "${MOCK_BIN}/hiclaw"

PATH="${MOCK_BIN}:${PATH}" \
TEST_HICLAW_LOG="${HICLAW_LOG}" \
AGENTTEAMS_NACOS_REGISTRY_URI="nacos://registry.example:8848/team" \
bash "${INSTALL_SCRIPT}" \
    --template reviewer \
    --version v1 \
    --worker-name bob \
    --model qwen3.6-plus \
    --skills git-delegation \
    --runtime openhuman

EXPECTED_LOG=$(printf '%s\n' \
    apply worker \
    --name bob \
    --package nacos://registry.example:8848/team/reviewer/v1 \
    --model qwen3.6-plus \
    --skills git-delegation \
    --runtime openhuman)
assert_eq "normal install forwards the supported CLI contract" \
    "${EXPECTED_LOG}" "$(cat "${HICLAW_LOG}")"

echo ""
echo "Results: ${PASS} passed, ${FAIL} failed"
if [ "${FAIL}" -gt 0 ]; then
    exit 1
fi
