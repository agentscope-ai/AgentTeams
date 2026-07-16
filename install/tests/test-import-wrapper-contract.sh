#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BASH_IMPORT="${ROOT_DIR}/install/hiclaw-import.sh"
POWERSHELL_IMPORT="${ROOT_DIR}/install/hiclaw-import.ps1"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/agentteams-import-contract.XXXXXX")"
trap 'rm -rf "${TEST_ROOT}"' EXIT

FAKE_BIN="${TEST_ROOT}/bin"
FAKE_DOCKER_CAPTURE="${TEST_ROOT}/docker-args.txt"
TEST_HOME="${TEST_ROOT}/home"
mkdir -p "${FAKE_BIN}" "${TEST_HOME}/cache" "${TEST_HOME}/config" "${TEST_HOME}/data"
export FAKE_DOCKER_CAPTURE

cat > "${FAKE_BIN}/docker" <<'EOF'
#!/usr/bin/env bash
set -e

case "${1:-}" in
    info)
        exit 0
        ;;
    ps)
        printf '%s\n' agentteams-manager
        exit 0
        ;;
    cp)
        exit 0
        ;;
    exec)
        if [ "${3:-}" = "mkdir" ]; then
            exit 0
        fi
        printf '%s\n' "$@" > "${FAKE_DOCKER_CAPTURE}"
        exit 0
        ;;
esac

printf 'unexpected docker invocation: %s\n' "$*" >&2
exit 1
EOF
chmod +x "${FAKE_BIN}/docker"

PASS=0
FAIL=0
RUN_OUTPUT=""
RUN_RC=0

pass() {
    printf 'PASS: %s\n' "$1"
    PASS=$((PASS + 1))
}

fail() {
    printf 'FAIL: %s\n' "$1" >&2
    FAIL=$((FAIL + 1))
}

run_bash_import() {
    set +e
    RUN_OUTPUT="$(PATH="${FAKE_BIN}:${PATH}" bash "${BASH_IMPORT}" "$@" 2>&1)"
    RUN_RC=$?
    set -e
}

run_powershell_import() {
    local pwsh_bin="$1"
    shift
    set +e
    RUN_OUTPUT="$(
        HOME="${TEST_HOME}" \
        XDG_CACHE_HOME="${TEST_HOME}/cache" \
        XDG_CONFIG_HOME="${TEST_HOME}/config" \
        XDG_DATA_HOME="${TEST_HOME}/data" \
        POWERSHELL_TELEMETRY_OPTOUT=1 \
        PATH="${FAKE_BIN}:${PATH}" \
        "${pwsh_bin}" -NoProfile -File "${POWERSHELL_IMPORT}" "$@" 2>&1
    )"
    RUN_RC=$?
    set -e
}

assert_rejected() {
    local label="$1"
    local expected="$2"
    if [ "${RUN_RC}" -ne 0 ] && [[ "${RUN_OUTPUT}" == *"${expected}"* ]]; then
        pass "${label}"
    else
        fail "${label}: rc=${RUN_RC}, output=${RUN_OUTPUT}"
    fi
}

assert_not_advertised() {
    local label="$1"
    local flag="$2"
    if [[ "${RUN_OUTPUT}" != *"${flag}"* ]]; then
        pass "${label}"
    else
        fail "${label}: help still contains ${flag}"
    fi
}

run_bash_import worker --name alice --skills github-operations
if [ "${RUN_RC}" -eq 0 ] && grep -Fxq -- '--skills' "${FAKE_DOCKER_CAPTURE}"; then
    pass "Bash forwards supported worker flags"
else
    fail "Bash did not forward supported worker flags"
fi

run_bash_import worker --name alice --mcp-servers github
assert_rejected "Bash rejects removed --mcp-servers" "Unknown option: --mcp-servers"

run_bash_import worker --name alice --dry-run
assert_rejected "Bash rejects unimplemented --dry-run" "Unknown option: --dry-run"

run_bash_import --help
assert_not_advertised "Bash help omits removed MCP flag" "--mcp-servers"
assert_not_advertised "Bash help omits unimplemented dry-run flag" "--dry-run"
assert_not_advertised "Bash help omits unimplemented prune flag" "--prune"

if grep -Eq -- '--prune|--dry-run|--watch' "${ROOT_DIR}/install/hiclaw-apply.sh"; then
    fail "Bash apply wrapper still advertises unimplemented flags"
else
    pass "Bash apply wrapper omits unimplemented flags"
fi

PWSH_BIN="${PWSH_BIN:-$(command -v pwsh || true)}"
if [ -n "${PWSH_BIN}" ]; then
    run_powershell_import "${PWSH_BIN}" worker -Name alice -Skills github-operations
    if [ "${RUN_RC}" -eq 0 ] && grep -Fxq -- '--skills' "${FAKE_DOCKER_CAPTURE}"; then
        pass "PowerShell forwards supported worker parameters"
    else
        fail "PowerShell did not forward supported worker parameters"
    fi

    run_powershell_import "${PWSH_BIN}" worker -Name alice -McpServers github
    assert_rejected "PowerShell rejects removed -McpServers" "parameter name 'McpServers'"

    run_powershell_import "${PWSH_BIN}" worker -Name alice -DryRun
    assert_rejected "PowerShell rejects unimplemented -DryRun" "parameter name 'DryRun'"

    run_powershell_import "${PWSH_BIN}" worker -Name alice -Prune
    assert_rejected "PowerShell rejects unimplemented -Prune" "parameter name 'Prune'"

    run_powershell_import "${PWSH_BIN}"
    assert_not_advertised "PowerShell help omits removed MCP parameter" "McpServers"
    assert_not_advertised "PowerShell help omits unimplemented dry-run parameter" "DryRun"
    assert_not_advertised "PowerShell help omits unimplemented prune parameter" "Prune"
else
    printf 'SKIP: pwsh is unavailable; PowerShell runtime assertions skipped\n'
fi

printf '\nResults: %d passed, %d failed\n' "${PASS}" "${FAIL}"
if [ "${FAIL}" -ne 0 ]; then
    exit 1
fi
