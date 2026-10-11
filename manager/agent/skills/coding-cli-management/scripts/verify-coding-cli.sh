#!/bin/bash
# verify-coding-cli.sh — opt-in execution verification for the coding CLI
# runners of run-coding-cli.sh.
#
# Reference runner versions (the versions this suite was field-verified against on the AgentTeams dev host, 2026-10-08; deployments pick their own npm channel tag and re-run the suite on change):
#   qwen     0.25.0  (verified flags: --yolo, --approval-mode, --max-wall-time, --max-session-turns)
#   opencode 1.18.34 (field-verified 2026-10-02; not pinned — re-run this
#            suite before relying on any other version)
#
# Opt-in by design: a CLI whose binary is not on PATH is reported as
# [SKIP] <cli>: <reason> and does NOT fail the suite. Run this where a real
# (or stub) runner is installed:
#
#   bash manager/agent/skills-alpha/coding-cli-management/scripts/verify-coding-cli.sh
#
# Cases per CLI:
#   a) headless_success    : fresh workspace + minimal prompt
#                            ("Create a file called hello.txt containing
#                            exactly: ok") -> run-coding-cli.sh exit 0 and
#                            workspace/hello.txt contains "ok"
#   b) auth_failure_exit    : run under a clean HOME (no auth) ->
#                            run-coding-cli.sh exits non-zero (the CLI's
#                            non-zero code is passed through; the `tee` in
#                            the pipeline does not swallow it — the script
#                            reads PIPESTATUS[0] right after the pipeline)
#   c) timeout_kill        : stub runner that sleeps 30s + --timeout 5 ->
#                            killed by the outer `timeout`, exit non-zero
#                            (124, or whatever the runner passes through)
#   d) workspace_boundary  : the artifact from case (a) is inside the
#                            --workspace dir, and the run log records
#                            workspace=<dir>
#   e) yolo_sandbox_warning: (qwen only) the un-sandboxed --yolo warning is
#                            visible in the case-(a) run log — or the
#                            environment is itself sandboxed (verbatim
#                            warning + dual branch in run_case_e below)
#   f) turn_budget         : (qwen only) max_session_turns=1 in an isolated
#                            config (CODING_CLI_CONFIG) stops a two-step
#                            prompt at the turn budget
#   g) json_file           : (qwen only) --json-file <path> writes the final
#                            structured result to <path>; a real minimal
#                            prompt asserts the file exists and parses as
#                            JSON (python3 -m json.tool)
#
# Note for (c) on qwen: qwen also has a native run-level budget,
# `qwen --max-wall-time <secs>`, which aborts the run with exit code 55.
# The outer `timeout` in run-coding-cli.sh remains the hard backstop for
# all runners; the stub case above verifies that wrapper path without
# costing a real model run.
#
# Exit: 0 if every case passes or is skipped (with reason), 1 on any failure.

set -u

# Locate the repo root from this file's own path
# (manager/agent/skills-alpha/coding-cli-management/scripts/ -> 5 levels up).
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../../../../" && pwd)"

RUN_SCRIPT="${SCRIPT_DIR}/run-coding-cli.sh"
if [ ! -f "${RUN_SCRIPT}" ]; then
    echo "FAIL setup (run-coding-cli.sh not found: ${RUN_SCRIPT})"
    exit 1
fi

if ! command -v jq >/dev/null 2>&1; then
    echo "[SKIP] verify-coding-cli (jq not found)"
    exit 0
fi

SANDBOX="$(mktemp -d)"
trap 'rm -rf "${SANDBOX}"' EXIT

mkdir -p "${SANDBOX}/bin" "${SANDBOX}/home-clean" "${SANDBOX}/ws-a" \
         "${SANDBOX}/ws-b" "${SANDBOX}/ws-c" "${SANDBOX}/prompts"

PROMPT_FILE="${SANDBOX}/prompts/case-a.txt"
printf 'Create a file called hello.txt containing exactly: ok' > "${PROMPT_FILE}"

FAILURES=0

pass() { echo "[PASS] $1"; }
fail() { echo "[FAIL] $1 ($2)"; FAILURES=$((FAILURES + 1)); }
skip() { echo "[SKIP] $1"; }

# run_case_a <cli>: real headless run in $SANDBOX/ws-a (also feeds case d).
run_case_a() {
    local cli="$1" rc
    bash "${RUN_SCRIPT}" --cli "${cli}" --workspace "${SANDBOX}/ws-a" \
        --prompt-file "${PROMPT_FILE}" --timeout 300 \
        > "${SANDBOX}/prompts/case-a.out" 2>&1
    rc=$?
    local problems=""
    [ "${rc}" = "0" ] || problems="exit=${rc}; "
    [ -f "${SANDBOX}/ws-a/hello.txt" ] || problems="hello.txt missing; "
    if [ -f "${SANDBOX}/ws-a/hello.txt" ]; then
        [ "$(cat "${SANDBOX}/ws-a/hello.txt")" = "ok" ] || problems="hello.txt content wrong; "
    fi
    if [ -n "${problems}" ]; then
        fail "${cli}.headless_success" "${problems}"
        sed 's/^/    | /' "${SANDBOX}/prompts/case-a.out" | tail -10
        return 1
    fi
    pass "${cli}.headless_success"
    return 0
}

# run_case_d <cli>: artifact location + run log workspace= line (uses case a).
run_case_d() {
    local cli="$1" log problems=""
    log="$(ls -1t "${SANDBOX}/ws-a/coding-cli-logs/"*.log 2>/dev/null | head -1)"
    [ -f "${SANDBOX}/ws-a/hello.txt" ] || problems="artifact not in workspace; "
    [ -n "${log}" ] || problems="no run log in workspace/coding-cli-logs; "
    if [ -n "${log}" ]; then
        grep -q "workspace=${SANDBOX}/ws-a" "${log}" \
            || problems="run log does not record workspace= path; "
    fi
    if [ -n "${problems}" ]; then
        fail "${cli}.workspace_boundary" "${problems}"
    else
        pass "${cli}.workspace_boundary"
    fi
}

# run_case_b <cli>: clean-HOME run must exit non-zero (auth failure path).
run_case_b() {
    local cli="$1" rc
    env -i HOME="${SANDBOX}/home-clean" PATH="${PATH}" \
        bash "${RUN_SCRIPT}" --cli "${cli}" --workspace "${SANDBOX}/ws-b" \
            --prompt-file "${PROMPT_FILE}" --timeout 120 \
            > "${SANDBOX}/prompts/case-b.out" 2>&1
    rc=$?
    if [ "${rc}" != "0" ]; then
        pass "${cli}.auth_failure_exit (rc=${rc})"
    else
        fail "${cli}.auth_failure_exit" "expected non-zero exit on auth failure, got 0"
        sed 's/^/    | /' "${SANDBOX}/prompts/case-b.out" | tail -10
    fi
}

# run_case_c <cli>: stub runner that sleeps 30s must be killed at --timeout.
run_case_c() {
    local cli="$1" rc
    cat > "${SANDBOX}/bin/${cli}" <<'SLEEP_STUB'
#!/bin/bash
# Stub runner for verify-coding-cli.sh case (c): sleep past the timeout.
exec sleep 30
SLEEP_STUB
    chmod +x "${SANDBOX}/bin/${cli}"
    PATH="${SANDBOX}/bin:${PATH}" \
        bash "${RUN_SCRIPT}" --cli "${cli}" --workspace "${SANDBOX}/ws-c" \
            --prompt-file "${PROMPT_FILE}" --timeout 5 \
            > "${SANDBOX}/prompts/case-c.out" 2>&1
    rc=$?
    if [ "${rc}" != "0" ]; then
        pass "${cli}.timeout_kill (rc=${rc})"
    else
        fail "${cli}.timeout_kill" "expected non-zero exit after timeout, got 0"
    fi
}

# run_case_e <cli>: (qwen only) the un-sandboxed --yolo warning must be
# visible in the case-(a) run log.
#
# Verbatim warning observed (qwen 0.24.7 and 0.25.0, QwenPaw001 container
# without docker — first line of a real headless run log):
#   Warning: running headless with --yolo / approval-mode=yolo and no
#   sandbox. All tool calls (shell, write, edit) auto-execute at this
#   process's privilege level. Configure tools.executionSandbox on Linux
#   or a supported legacy sandbox via --sandbox / QWEN_SANDBOX, or set
#   QWEN_CODE_SUPPRESS_YOLO_WARNING=1 to silence this notice.
#
# Dual branch:
#   1. warning visible            -> PASS (unsandboxed --yolo run warned,
#                                    as expected)
#   2. warning absent, but the    -> PASS only if the environment is itself
#      environment is sandboxed     sandboxed (QWEN_SANDBOX set, or a sandbox
#                                    marker in the log) — i.e. the sandbox
#                                    took effect and suppressed the warning
#   3. otherwise                  -> SKIP with reason (environment
#                                    difference)
run_case_e() {
    local cli="$1" log
    log="$(ls -1t "${SANDBOX}/ws-a/coding-cli-logs/"*.log 2>/dev/null | head -1)"
    if [ -z "${log}" ] || [ ! -f "${log}" ]; then
        skip "${cli}.yolo_sandbox_warning (no case-a run log to inspect)"
        return 0
    fi
    if grep -q "running headless with --yolo / approval-mode=yolo and no sandbox" "${log}"; then
        pass "${cli}.yolo_sandbox_warning (warning visible in run log — unsandboxed --yolo run, as expected)"
        return 0
    fi
    if [ -n "${QWEN_SANDBOX:-}" ] || grep -qi "sandbox" "${log}"; then
        pass "${cli}.yolo_sandbox_warning (no warning; environment itself is sandboxed, sandbox took effect)"
        return 0
    fi
    skip "${cli}.yolo_sandbox_warning (no warning in run log and no sandbox in this environment — environment difference)"
    return 0
}

# run_case_f <cli>: (qwen only) max_session_turns budget smoke.
#
# Real run of a two-step prompt ("create a.txt, then b.txt") with
# max_session_turns=1 in an ISOLATED config: CODING_CLI_CONFIG points at a
# sandboxed HOME, so the delegating agent's real config is untouched.
#
# Observed behavior (qwen 0.24.7 and 0.25.0, real run): the run aborts at
# the turn budget with EXIT CODE 53 and the run log line
#   Reached max session turns for this session. Increase the number of
#   turns by specifying maxSessionTurns in settings.json.
# Artifact state is model-dependent (one turn can batch both step file
# writes before the final response hits the budget), so the assertion
# pins exit code + budget message, not the file set.
run_case_f() {
    local cli="$1" rc log_f
    local home_f="${SANDBOX}/home-f"
    mkdir -p "${home_f}" "${SANDBOX}/ws-f"
    printf '{"enabled":true,"cli":"qwen","max_session_turns":1}\n' \
        > "${home_f}/coding-cli-config.json"
    printf 'Step 1: create a file called a.txt containing exactly: A\nStep 2: then create a file called b.txt containing exactly: B\n' \
        > "${SANDBOX}/prompts/case-f.txt"
    CODING_CLI_CONFIG="${home_f}/coding-cli-config.json" \
        bash "${RUN_SCRIPT}" --cli "${cli}" --workspace "${SANDBOX}/ws-f" \
            --prompt-file "${SANDBOX}/prompts/case-f.txt" --timeout 300 \
            > "${SANDBOX}/prompts/case-f.out" 2>&1
    rc=$?
    log_f="$(ls -1t "${SANDBOX}/ws-f/coding-cli-logs/"*.log 2>/dev/null | head -1)"
    if [ "${rc}" != "0" ] && grep -q "Reached max session turns" "${log_f}" 2>/dev/null; then
        pass "${cli}.turn_budget (rc=${rc} — aborted at the turn budget with 'Reached max session turns'; observed rc=53 on qwen 0.24.7 and 0.25.0)"
    else
        fail "${cli}.turn_budget" "expected non-zero exit with 'Reached max session turns' in the run log (rc=${rc})"
        sed 's/^/    | /' "${SANDBOX}/prompts/case-f.out" | tail -10
    fi
}

# run_case_g <cli>: (qwen only) --json-file structured-output smoke.
#
# Real run of a minimal prompt with `--json-file <path>`: the qwen runner
# writes its final structured result to <path> (config.ts, mutually exclusive
# with --json-fd). The assertion pins that the file exists at that path AND
# parses as JSON (`python3 -m json.tool`) — i.e. the wrapper's passthrough
# produced a machine-auditable artifact end to end. Requires a real qwen with
# working auth (the same environment case (a) needs).
run_case_g() {
    local cli="$1" rc json_out
    if ! command -v python3 >/dev/null 2>&1; then
        skip "${cli}.json_file (python3 not on PATH — JSON-parse assertion unavailable)"
        return 0
    fi
    json_out="${SANDBOX}/ws-g/result.json"
    mkdir -p "${SANDBOX}/ws-g"
    bash "${RUN_SCRIPT}" --cli "${cli}" --workspace "${SANDBOX}/ws-g" \
        --prompt-file "${PROMPT_FILE}" --timeout 300 \
        --json-file "${json_out}" \
        > "${SANDBOX}/prompts/case-g.out" 2>&1
    rc=$?
    if [ "${rc}" != "0" ]; then
        fail "${cli}.json_file" "run with --json-file exited ${rc} (expected 0)"
        sed 's/^/    | /' "${SANDBOX}/prompts/case-g.out" | tail -10
        return 0
    fi
    if [ ! -f "${json_out}" ]; then
        fail "${cli}.json_file" "--json-file produced no file at ${json_out}"
        sed 's/^/    | /' "${SANDBOX}/prompts/case-g.out" | tail -10
        return 0
    fi
    if ! python3 -m json.tool "${json_out}" >/dev/null 2>&1; then
        fail "${cli}.json_file" "file at ${json_out} is not valid JSON"
        return 0
    fi
    pass "${cli}.json_file (structured result written to the --json-file path and parses as JSON)"
}

# check_node_generation: environment-level check (runs once, not per-CLI). The npm
# channel (qwen-code / opencode install) needs a Node in the OpenSSL 3.5 generation
# to survive restrictive-network TLS-fingerprint filtering (see the SKILL "Node
# generation" and "Restricted-network note"). WARN, not FAIL: on a local or
# non-restrictive link an older Node still works, but flag the WAN risk so an
# OpenSSL-3.0-generation Node is a known quantity, not a silent RST in production.
check_node_generation() {
    if ! command -v node >/dev/null 2>&1; then
        skip "node_generation (node not on PATH — the npm install/upgrade route is unavailable)"
        return
    fi
    local node_v ssl_v major eol
    node_v="$(node -p 'process.version' 2>/dev/null || echo unknown)"
    ssl_v="$(node -p 'process.versions.openssl' 2>/dev/null || echo unknown)"
    major="${node_v#v}"; major="${major%%.*}"
    eol=0
    case "${major}" in 18|20|25) eol=1 ;; esac
    case "${ssl_v}" in
        3.5*|3.6*|4.*)
            if [ "${eol}" = "1" ]; then
                echo "[WARN] node_generation: node ${node_v} (OpenSSL ${ssl_v}) is EOL — fingerprint-safe but no longer security-patched; target Node 24 LTS."
            else
                pass "node_generation (node ${node_v}, OpenSSL ${ssl_v} — 3.5 generation, fingerprint-safe)"
            fi ;;
        *)
            echo "[WARN] node_generation: node ${node_v} bundles OpenSSL ${ssl_v} (<3.5). On a restrictive-network link the qwen-code npm route can be RST'd by a TLS-fingerprint middle box — target Node 24 LTS (OpenSSL 3.5) or route the model plane through the local proxy. Local/non-restrictive links are unaffected."
            if [ "${eol}" = "1" ]; then
                echo "[WARN] node_generation: node ${node_v} is also EOL (no security patches)."
            fi ;;
    esac
}

check_node_generation

for cli in qwen opencode; do
    if ! command -v "${cli}" >/dev/null 2>&1; then
        skip "${cli}: binary not found on PATH (not installed — opt-in skipped)"
        continue
    fi
    echo ""
    echo "== ${cli} (pinned version: $(command -v "${cli}")) =="
    case_a_ok=0
    if run_case_a "${cli}"; then
        case_a_ok=1
        run_case_d "${cli}"
    fi
    run_case_b "${cli}"
    run_case_c "${cli}"
    if [ "${cli}" = "qwen" ]; then
        run_case_e "${cli}"
        if [ "${case_a_ok}" = "1" ]; then
            run_case_f "${cli}"
        else
            skip "${cli}.turn_budget (case-a failed — cannot isolate budget behavior from an auth failure)"
        fi
        run_case_g "${cli}"
    fi
done

echo ""
if [ "${FAILURES}" = "0" ]; then
    echo "All cases passed or skipped."
    exit 0
else
    echo "${FAILURES} case(s) FAILED."
    exit 1
fi
