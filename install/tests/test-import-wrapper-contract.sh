#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BASH_IMPORT="${ROOT_DIR}/install/agentteams-import.sh"
POWERSHELL_IMPORT="${ROOT_DIR}/install/agentteams-import.ps1"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/agentteams-import-contract.XXXXXX")"
trap 'rm -rf "${TEST_ROOT}"' EXIT

FAKE_BIN="${TEST_ROOT}/bin"
CAPTURE="${TEST_ROOT}/docker-args.txt"
TEST_HOME="${TEST_ROOT}/home"
mkdir -p "${FAKE_BIN}" "${TEST_HOME}/cache" "${TEST_HOME}/config" "${TEST_HOME}/data"
export CAPTURE

cat >"${FAKE_BIN}/docker" <<'MOCK'
#!/usr/bin/env bash
set -e
case "${1:-}" in
    info) exit 0 ;;
    ps) printf '%s\n' agentteams-manager ;;
    cp) exit 0 ;;
    exec)
        if [ "${3:-}" = "mkdir" ]; then
            exit 0
        fi
        printf '%s\n' "$@" >"${CAPTURE}"
        ;;
    *) echo "unexpected docker invocation: $*" >&2; exit 1 ;;
esac
MOCK
chmod +x "${FAKE_BIN}/docker"

PASS=0
FAIL=0
RUN_OUTPUT=""
RUN_RC=0

pass() { printf 'PASS: %s\n' "$1"; PASS=$((PASS + 1)); }
fail() { printf 'FAIL: %s\n' "$1" >&2; FAIL=$((FAIL + 1)); }

run_bash_import() {
    set +e
    RUN_OUTPUT="$(PATH="${FAKE_BIN}:${PATH}" bash "${BASH_IMPORT}" "$@" 2>&1)"
    RUN_RC=$?
    set -e
}

assert_rejected() {
    local label="$1" expected="$2"
    if [ "${RUN_RC}" -ne 0 ] && [[ "${RUN_OUTPUT}" == *"${expected}"* ]]; then
        pass "${label}"
    else
        fail "${label}: rc=${RUN_RC}, output=${RUN_OUTPUT}"
    fi
}

run_bash_import worker --name alice --skills github-operations
if [ "${RUN_RC}" -eq 0 ] && grep -Fxq -- '--skills' "${CAPTURE}"; then
    pass "Bash forwards supported worker flags"
else
    fail "Bash did not forward supported worker flags"
fi

run_bash_import worker --name alice --mcp-servers github
assert_rejected "Bash rejects removed --mcp-servers" "Unknown option: --mcp-servers"
run_bash_import worker --name alice --dry-run
assert_rejected "Bash rejects unimplemented --dry-run" "Unknown option: --dry-run"

run_bash_import --help
for flag in --mcp-servers --dry-run --prune; do
    if [[ "${RUN_OUTPUT}" == *"${flag}"* ]]; then
        fail "Bash help still advertises ${flag}"
    else
        pass "Bash help omits ${flag}"
    fi
done

if grep -Eq -- '--prune|--dry-run|--watch' "${ROOT_DIR}/install/agentteams-apply.sh"; then
    fail "Bash apply wrapper still advertises unimplemented flags"
else
    pass "Bash apply wrapper omits unimplemented flags"
fi

PWSH_BIN="${PWSH_BIN:-$(command -v pwsh || true)}"
if [ -n "${PWSH_BIN}" ]; then
    run_pwsh() {
        set +e
        RUN_OUTPUT="$(
            HOME="${TEST_HOME}" \
            XDG_CACHE_HOME="${TEST_HOME}/cache" \
            XDG_CONFIG_HOME="${TEST_HOME}/config" \
            XDG_DATA_HOME="${TEST_HOME}/data" \
            POWERSHELL_TELEMETRY_OPTOUT=1 \
            PATH="${FAKE_BIN}:${PATH}" \
            "${PWSH_BIN}" -NoProfile -File "${POWERSHELL_IMPORT}" "$@" 2>&1
        )"
        RUN_RC=$?
        set -e
    }
    for flag in McpServers DryRun Prune; do
        run_pwsh worker -Name alice "-${flag}" value
        assert_rejected "PowerShell rejects -${flag}" "parameter name '${flag}'"
    done
else
    printf 'SKIP: pwsh is unavailable; PowerShell runtime assertions skipped\n'
fi

printf '\nResults: %d passed, %d failed\n' "${PASS}" "${FAIL}"
test "${FAIL}" -eq 0
