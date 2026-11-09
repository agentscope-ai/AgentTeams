#!/bin/bash
# Regression tests for the one-shot execution boundary of run-coding-cli.sh:
# the per-run --approval-mode / --max-wall-time flags, their config
# counterparts (approval_mode / max_wall_time), the per-run > config >
# default precedence, the non-qwen warn-and-ignore contract, the hard error
# on an invalid tier, and the byte-invariant default path (no flag + no
# config -> the exact pre-boundary qwen argv).
#
# Method (the one established by #1342 for qwen arg assembly): a fake runner
# stub is injected at the front of PATH and records the exact argv the
# wrapper assembles; assertions compare the recorded argv, one arg per line.

set -uo pipefail

PASS=0
FAIL=0
TMPDIR_ROOT=$(mktemp -d)
trap 'rm -rf "${TMPDIR_ROOT}"' EXIT

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
SCRIPT="${PROJECT_ROOT}/manager/agent/skills/coding-cli-management/scripts/run-coding-cli.sh"

if ! command -v jq >/dev/null 2>&1; then
    echo "[SKIP] exec-boundary unit test (jq not found — run-coding-cli.sh reads its config with jq)"
    exit 0
fi

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

CASE_DIR=""
ARGV_LOG=""
RUN_CLI="qwen"

# Fake runner stub: records argv (one arg per line) and exits 0.
install_stub() {
    local name="$1"
    cat > "${CASE_DIR}/bin/${name}" <<'EOF'
#!/bin/bash
printf '%s\n' "$@" > "${ARGV_LOG_TARGET}"
exit 0
EOF
    chmod +x "${CASE_DIR}/bin/${name}"
}

setup_case() {
    CASE_DIR="${TMPDIR_ROOT}/$1"
    mkdir -p "${CASE_DIR}/bin" "${CASE_DIR}/workspace" "${CASE_DIR}/home"
    printf 'p1' > "${CASE_DIR}/prompt.txt"
    ARGV_LOG="${CASE_DIR}/argv.log"
    RUN_CLI="qwen"
}

# run_wrapper [extra args...]
# Runs run-coding-cli.sh with the case's bin dir first on PATH and a fully
# isolated environment (case-local HOME; CODING_CLI_CONFIG at the case's
# config path, which does not exist unless the case writes it).
run_wrapper() {
    PATH="${CASE_DIR}/bin:${PATH}" \
    CODING_CLI_CONFIG="${CASE_DIR}/coding-cli-config.json" \
    HOME="${CASE_DIR}/home" \
    ARGV_LOG_TARGET="${ARGV_LOG}" \
    bash "${SCRIPT}" \
        --cli "${RUN_CLI}" \
        --workspace "${CASE_DIR}/workspace" \
        --prompt-file "${CASE_DIR}/prompt.txt" \
        "$@" \
        > "${CASE_DIR}/stdout.log" 2> "${CASE_DIR}/stderr.log"
}

write_config() {
    printf '%s' "$1" > "${CASE_DIR}/coding-cli-config.json"
}

assert_argv_eq() {
    local desc="$1"; shift
    local expected actual
    expected=$(printf '%s\n' "$@")
    actual=$(cat "${ARGV_LOG}" 2>/dev/null || true)
    if [ "${expected}" = "${actual}" ]; then
        pass "${desc}"
    else
        fail "${desc}" "argv: $(printf '%s ' "$@")" "argv: ${actual}"
    fi
}

assert_argv_not_contains() {
    local desc="$1" needle="$2"
    if [ ! -f "${ARGV_LOG}" ] || ! grep -qxF -- "${needle}" "${ARGV_LOG}"; then
        pass "${desc}"
    else
        fail "${desc}" "argv does not contain '${needle}'" "$(cat "${ARGV_LOG}")"
    fi
}

assert_stderr_contains() {
    local desc="$1" needle="$2"
    if grep -qF -- "${needle}" "${CASE_DIR}/stderr.log" 2>/dev/null; then
        pass "${desc}"
    else
        fail "${desc}" "stderr contains '${needle}'" "$(cat "${CASE_DIR}/stderr.log" 2>/dev/null || true)"
    fi
}

echo "=== T1: default path (no flag, no config) -> byte-invariant --yolo argv ==="
setup_case t1-default
install_stub qwen
run_wrapper
assert_eq "T1 wrapper exits 0" 0 "$?"
assert_argv_eq "T1 argv is exactly: --yolo <prompt>" "--yolo" "p1"

echo "=== T2: --approval-mode plan -> --approval-mode plan, no --yolo ==="
setup_case t2-cli-plan
install_stub qwen
run_wrapper --approval-mode plan
assert_eq "T2 wrapper exits 0" 0 "$?"
assert_argv_eq "T2 argv is exactly: --approval-mode plan <prompt>" "--approval-mode" "plan" "p1"
assert_argv_not_contains "T2 no --yolo in argv" "--yolo"

echo "=== T3: config approval_mode=plan (no CLI flag) -> --approval-mode plan ==="
setup_case t3-config-plan
install_stub qwen
write_config '{"approval_mode":"plan"}'
run_wrapper
assert_eq "T3 wrapper exits 0" 0 "$?"
assert_argv_eq "T3 argv is exactly: --approval-mode plan <prompt>" "--approval-mode" "plan" "p1"

echo "=== T4: CLI --approval-mode auto beats config approval_mode=plan ==="
setup_case t4-cli-beats-config
install_stub qwen
write_config '{"approval_mode":"plan"}'
run_wrapper --approval-mode auto
assert_eq "T4 wrapper exits 0" 0 "$?"
assert_argv_eq "T4 argv is exactly: --approval-mode auto <prompt>" "--approval-mode" "auto" "p1"

echo "=== T5: --max-wall-time 10m -> --max-wall-time 10m appended, --yolo kept ==="
setup_case t5-walltime
install_stub qwen
run_wrapper --max-wall-time 10m
assert_eq "T5 wrapper exits 0" 0 "$?"
assert_argv_eq "T5 argv is exactly: --yolo --max-wall-time 10m <prompt>" "--yolo" "--max-wall-time" "10m" "p1"

echo "=== T6: config max_wall_time=5m (no CLI flag) -> --max-wall-time 5m ==="
setup_case t6-config-walltime
install_stub qwen
write_config '{"max_wall_time":"5m"}'
run_wrapper
assert_eq "T6 wrapper exits 0" 0 "$?"
assert_argv_eq "T6 argv is exactly: --yolo --max-wall-time 5m <prompt>" "--yolo" "--max-wall-time" "5m" "p1"

echo "=== T7: --cli opencode + --approval-mode plan -> flag ignored with the governance note ==="
setup_case t7-opencode-ignores
install_stub opencode
RUN_CLI="opencode"
run_wrapper --approval-mode plan
assert_eq "T7 wrapper exits 0" 0 "$?"
assert_argv_eq "T7 opencode argv unchanged (no --approval-mode)" \
    "run" "--auto" "--dir" "${CASE_DIR}/workspace" "p1"
assert_stderr_contains "T7 governance note emitted for cli=opencode" "governance flags"
assert_stderr_contains "T7 note covers --approval-mode" "--approval-mode"

echo "=== T8: invalid --approval-mode bogus -> hard error, no silent fallback ==="
setup_case t8-invalid
install_stub qwen
run_wrapper --approval-mode bogus
assert_eq "T8 wrapper exits non-zero" 1 "$?"
assert_stderr_contains "T8 error names the invalid value" "bogus"
assert_stderr_contains "T8 error lists the valid values" "plan, default, auto-edit, auto, yolo"
if [ -f "${ARGV_LOG}" ]; then
    fail "T8 qwen was not invoked" "no argv log" "argv log present: $(cat "${ARGV_LOG}")"
else
    pass "T8 qwen was not invoked"
fi

echo "=== T9: config approval_mode=plan, no budgets -> no UNGOVERNED warning ==="
setup_case t9-plan-governed
install_stub qwen
write_config '{"approval_mode":"plan"}'
run_wrapper
assert_eq "T9 wrapper exits 0" 0 "$?"
assert_argv_eq "T9 argv is exactly: --approval-mode plan <prompt>" "--approval-mode" "plan" "p1"
if grep -qF "UNGOVERNED" "${CASE_DIR}/stdout.log" 2>/dev/null; then
    fail "T9 no UNGOVERNED warning for the plan tier" "no UNGOVERNED line" "$(grep -F UNGOVERNED "${CASE_DIR}/stdout.log")"
else
    pass "T9 no UNGOVERNED warning for the plan tier"
fi

echo "=== T10: default (yolo) with no budgets -> UNGOVERNED warning still emitted ==="
setup_case t10-yolo-ungoverned
install_stub qwen
run_wrapper
assert_eq "T10 wrapper exits 0" 0 "$?"
assert_argv_eq "T10 argv is exactly: --yolo <prompt>" "--yolo" "p1"
if grep -qF "UNGOVERNED" "${CASE_DIR}/stdout.log" 2>/dev/null; then
    pass "T10 UNGOVERNED warning still emitted for ungoverned yolo"
else
    fail "T10 UNGOVERNED warning still emitted for ungoverned yolo" "UNGOVERNED line in stdout" "$(cat "${CASE_DIR}/stdout.log")"
fi

echo
echo "Results: ${PASS} passed, ${FAIL} failed"
[ "${FAIL}" -eq 0 ]
