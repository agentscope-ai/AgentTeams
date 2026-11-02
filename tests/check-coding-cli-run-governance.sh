#!/bin/bash
# check-coding-cli-run-governance.sh
#
# Tests run-coding-cli.sh's governance passthrough, its "ungoverned"
# visibility warning, and its success/failure/timeout outcome detection,
# using a STUB qwen on PATH — no real runner required, so this runs in CI.
# Closes the gap where the delegation executor (the script that actually runs
# the CLI) had no automated test (only detect + verify did).
#
# Cases:
#   1. flag passthrough (jq-independent): --allowed-tools reaches the runner
#      argv; with a budget/sandbox config present (needs jq) those flags do too.
#   2. ungoverned  — no config, no tool allowlist: the run log carries the
#      UNGOVERNED warning (code-level visibility), exit still 0.
#   3. tool-only   — only --allowed-tools (no budget/sandbox): still governed,
#      so NO warning (a tool allowlist is a real boundary).
#   4. timeout     — the runner outlives --timeout: exit 124 + TIMEOUT message
#      (the timeout-detection half of success/failure/timeout verification).
#   5. failure     — the runner exits non-zero: the code is propagated and NO
#      TIMEOUT is reported (the failure-detection half).
#   6. json-file   — the capture (json-out) branch is the default for EVERY
#      version (measured 2026-10-30: no published qwen writes the native
#      --json-file file in headless one-shot mode): with no explicit
#      CODING_CLI_JSON_BOUNDARY the argv carries -o json and NOT
#      --json-file, and the wrapper's own stdout capture lands the file —
#      asserted for the version-less stub (6), 0.26.3 (6b) and 0.25.2 (6c).
#      With an explicit override (6d: CODING_CLI_JSON_BOUNDARY=0.25.3 and
#      stub 0.25.2) the LEGACY pass-through is exercised — the argv carries
#      --json-file with the wrapper's unique per-run capture path (NOT the
#      target), and a stub that writes no file fails the run loudly
#      (json-file-assertion). The stub models the version it claims, so a
#      hardcoded legacy semantic can no longer mask a CLI output drift.
#   7. cli-missing — a wiped install fails fast (exit 125) with the
#      re-install hint.
#   8. distinct-home — a config in the delegating agent's ORIGINAL home
#      (HOME moved to QWENPAW_WORKING_DIR, no CODING_CLI_CONFIG): all four
#      controls reach the runner argv and --yolo is absent (#1409
#      regression: the persisted-home move must not hide the control
#      config).
#   9. new-home fallback — a config only in the effective (persisted) home
#      is still read (backward compat for configs created after the move).
#  10. both homes — different configs in both homes: the original home
#      wins.
#  11. empty-json  — the capture branch exits 0 with ZERO bytes of stdout
#      (the 0.25.3-style drift): the run must FAIL (json-file-assertion), be
#      recorded as not-ok, and leave no audit file at the target.
#  12. bad-json    — the capture branch exits 0 with malformed stdout: same
#      contract — the run fails and the target stays clean.
#  13. write-fail — the --json-file target sits under a regular file, so the
#      validated capture cannot be moved into place: the run fails
#      (json-capture-failed) and no ok is recorded.
#  14. stale-target — legacy (explicit override): a VALID audit pre-written
#      at the target by a previous run, and a CLI (0.24.0-style) that exits
#      0 without writing anything: the run must FAIL (json-file-assertion),
#      the record must not be ok, and the old file must be left untouched
#      (neither consumed nor deleted) — the maintainer's false-success repro.
#  15. legacy-ok   — legacy (explicit override): the stub writes the
#      per-run capture the CLI was told to write; the wrapper validates it
#      and moves it onto the target (exit 0, target content = the stub's,
#      assert=pass in the event log).
#  16. legacy-fail — legacy (explicit override): the legacy CLI exits 7
#      without writing: the code is propagated, no capture tmp is left
#      behind, and a pre-existing target is left untouched.
# The legacy cases pin CODING_CLI_JSON_BOUNDARY=0.25.3 explicitly — since
# the default is capture for every version, the pass-through is otherwise
# unreachable.
#
# jq is optional: the config-driven budget/sandbox assertions are skipped (with
# a note) when jq is absent, matching the detect suite's "skip when a
# dependency is missing" convention.
set -u

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_SCRIPT="${REPO_ROOT}/manager/agent/skills/coding-cli-management/scripts/run-coding-cli.sh"
if [ ! -f "${RUN_SCRIPT}" ]; then
    echo "FAIL setup (run-coding-cli.sh not found: ${RUN_SCRIPT})"
    exit 1
fi

HAS_JQ=0
command -v jq >/dev/null 2>&1 && HAS_JQ=1

# The legacy pass-through is reachable ONLY through an explicit
# CODING_CLI_JSON_BOUNDARY override (the wrapper's default is capture for
# every version): pin the ambient value to "no override" so a case's branch
# cannot depend on the surrounding environment; the legacy cases set the
# override explicitly on their run_qwen call.
CODING_CLI_JSON_BOUNDARY=""

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

# Stub qwen: record its argv (one token per line) to $QWEN_STUB_ARGS. Behavior
# is selected by QWEN_STUB_MODE — default (exit 0) / sleep (sleep 5, to trip
# the timeout) / fail (exit 7, writes no audit — the CLI died before its
# output) / no-file (exit 0, writes NO file — the maintainer's 0.24.0 repro:
# a clean exit without the audit the contract demands) / empty-json
# (json-out: zero-byte stdout, exit 0 — the drift that used to record ok with
# a zero-byte audit) / bad-json (json-out: malformed stdout, exit 0). The
# version the wrapper's branch detection reads is selected by
# QWEN_STUB_VERSION — unset means the stub prints no version at all (with no
# CODING_CLI_JSON_BOUNDARY override the wrapper's default is the capture
# branch; with an override set, detection failure falls to legacy). No real
# runner required, so this runs in CI.
STUB_BIN="${TMP}/bin"
STUB_ARGS="${TMP}/qwen-args.txt"
mkdir -p "${STUB_BIN}"
cat > "${STUB_BIN}/qwen" <<'STUB'
#!/bin/bash
# --version: echo QWEN_STUB_VERSION when set (the wrapper's branch detection
# parses it). Unset -> no output (with no boundary override the wrapper
# still takes the capture branch — its default for every version).
# Exit before the argv record so the detection probe never overwrites the
# real run's argv capture.
if [ "${1:-}" = "--version" ]; then
    if [ -n "${QWEN_STUB_VERSION:-}" ]; then
        printf 'qwen %s\n' "${QWEN_STUB_VERSION}"
    fi
    exit 0
fi
printf '%s\n' "$@" > "${QWEN_STUB_ARGS}"
# Emulate the native structured-output surface per the semantics the claimed
# version carries:
#   --json-file <path> (pre-drift): write a minimal structured result to the
#   path the CLI was TOLD to write — the argv value, i.e. the wrapper's
#   unique per-run capture (exactly what a real CLI does: it writes the
#   path it was given, never a different one) — so the wrapper's legacy
#   validate-and-move is asserted end to end. fail/no-file modes write
#   nothing (a dying / no-op CLI leaves no audit).
#   -o json (qwen-code >= 0.25.3 headless): print the stdout event array and
#   write NO file — 0.25.3's --json-file is a dual-output no-op there, so the
#   drift the wrapper works around is modeled here, not papered over.
json_mode=0
prev=""
case "${QWEN_STUB_MODE:-default}" in
    fail|no-file) write_json_file=0 ;;
    *)            write_json_file=1 ;;
esac
for a in "$@"; do
    if [ "${prev}" = "--json-file" ] && [ "${write_json_file}" = "1" ]; then
        printf '{"type":"result","subtype":"success","result":"stub"}\n' > "${a}"
    fi
    if [ "${prev}" = "-o" ] && [ "${a}" = "json" ]; then
        json_mode=1
    fi
    prev="${a}"
done
# json-out payload per mode: default = the event array; empty-json = ZERO
# bytes on stdout (the drift the wrapper must now catch: exit 0, no audit);
# bad-json = malformed output. Both still exit 0 — the failure is the
# missing/invalid audit, not the CLI's exit code.
json_payload=array
case "${QWEN_STUB_MODE:-default}" in
    empty-json) json_payload=empty ;;
    bad-json)   json_payload=bad ;;
esac
if [ "${json_mode}" = "1" ]; then
    case "${json_payload}" in
        empty) : ;;
        bad)   printf '{not json' ;;
        *)     printf '[{"type":"system","subtype":"init"},{"type":"result","subtype":"success","is_error":false,"result":"stub"}]\n' ;;
    esac
fi
case "${QWEN_STUB_MODE:-default}" in
    sleep) sleep 5; exit 0 ;;
    fail)  exit 7 ;;
    *)     exit 0 ;;
esac
STUB
chmod +x "${STUB_BIN}/qwen"

workspace="${TMP}/ws"
prompt="${TMP}/prompt.txt"
mkdir -p "${workspace}"
echo "do the thing" > "${prompt}"

fail=0
have_arg() { grep -qxF -- "$1" "${STUB_ARGS}" 2>/dev/null; }
argv_show() { tr '\n' ' ' < "${STUB_ARGS}" 2>/dev/null; }

run_qwen() {
    # $@ = extra run-coding-cli.sh flags; $CFG (may be empty) selects the config;
    # $QWEN_STUB_MODE (may be empty) selects the stub behavior.
    PATH="${STUB_BIN}:${PATH}" \
    QWEN_STUB_ARGS="${STUB_ARGS}" \
    CODING_CLI_CONFIG="${CFG:-${TMP}/nonexistent.json}" \
    bash "${RUN_SCRIPT}" --cli qwen --workspace "${workspace}" --prompt-file "${prompt}" "$@" 2>&1
}

run_qwen_distinct_home() {
    # $1 = the delegating agent's original home, $2 = the persisted working
    # dir (QWENPAW_WORKING_DIR), $3... = extra run-coding-cli.sh flags.
    # Unlike run_qwen this does NOT set CODING_CLI_CONFIG (it is pinned to
    # empty, which the wrapper treats as unset): it exercises the DEFAULT
    # config resolution with a moved HOME — the #1409 scenario.
    PATH="${STUB_BIN}:${PATH}" \
    QWEN_STUB_ARGS="${STUB_ARGS}" \
    CODING_CLI_CONFIG="" \
    HOME="$1" \
    QWENPAW_WORKING_DIR="$2" \
    bash "${RUN_SCRIPT}" --cli qwen --workspace "${workspace}" --prompt-file "${prompt}" "${@:3}" 2>&1
}

# The shared runs.jsonl index the json cases inspect (each failing run
# appends exactly one record, so the LAST line is that run's record).
runs_jsonl="${workspace}/coding-cli-logs/runs.jsonl"

# --- case 1: flag passthrough always; config-driven budget/sandbox iff jq -----
cfg1="${TMP}/cfg1.json"
if [ "${HAS_JQ}" = "1" ]; then
    printf '{"max_session_turns": 5, "sandbox": true}' > "${cfg1}"
fi
CFG="${cfg1}" rm -f "${STUB_ARGS}"
out="$(CFG="${cfg1}" run_qwen --allowed-tools "Read,Grep")"
for want in --yolo --allowed-tools "Read,Grep"; do
    if ! have_arg "${want}"; then
        echo "FAIL case1: runner argv missing '${want}' (got: $(argv_show))"
        fail=1
    fi
done
if [ "${HAS_JQ}" = "1" ]; then
    for want in --max-session-turns 5 --sandbox; do
        if ! have_arg "${want}"; then
            echo "FAIL case1-config: runner argv missing '${want}' (got: $(argv_show))"
            fail=1
        fi
    done
else
    echo "SKIP case1 config-driven budget/sandbox assertions (jq absent)"
fi
if grep -q "UNGOVERNED" <<<"${out}"; then
    echo "FAIL case1: unexpected UNGOVERNED warning (got: ${out})"
    fail=1
fi

# --- case 2: ungoverned (no config, no tool allowlist) -> warning, exit 0 -----
CFG="" rm -f "${STUB_ARGS}"
out="$(CFG="" run_qwen)"
rc=$?
if [ "${rc}" -ne 0 ]; then
    echo "FAIL case2: run should still exit 0 (got ${rc}): ${out}"
    fail=1
fi
if ! grep -q "UNGOVERNED" <<<"${out}"; then
    echo "FAIL case2: expected UNGOVERNED warning, got: ${out}"
    fail=1
fi

# --- case 3: tool allowlist only -> governed, so NO warning -------------------
CFG="" rm -f "${STUB_ARGS}"
out="$(CFG="" run_qwen --allowed-tools "Read")"
if grep -q "UNGOVERNED" <<<"${out}"; then
    echo "FAIL case3: a tool allowlist is a real boundary; no UNGOVERNED warning expected (got: ${out})"
    fail=1
fi
if ! have_arg "--allowed-tools" || ! have_arg "Read"; then
    echo "FAIL case3: --allowed-tools Read should reach the runner argv (got: $(argv_show))"
    fail=1
fi

# --- case 4: timeout — runner outlives --timeout -> exit 124 + TIMEOUT --------
rm -f "${STUB_ARGS}"
out="$(QWEN_STUB_MODE=sleep run_qwen --timeout 1)"
rc=$?
if [ "${rc}" -ne 124 ]; then
    echo "FAIL case4: expected exit 124 on timeout (got ${rc}): ${out}"
    fail=1
fi
if ! grep -q "TIMEOUT" <<<"${out}"; then
    echo "FAIL case4: expected TIMEOUT message, got: ${out}"
    fail=1
fi

# --- case 5: failure — runner exits non-zero -> propagate code, no TIMEOUT ----
rm -f "${STUB_ARGS}"
out="$(QWEN_STUB_MODE=fail run_qwen)"
rc=$?
if [ "${rc}" -ne 7 ]; then
    echo "FAIL case5: expected exit 7 (the runner's code) (got ${rc}): ${out}"
    fail=1
fi
if grep -q "TIMEOUT" <<<"${out}"; then
    echo "FAIL case5: no TIMEOUT expected for a plain failure (got: ${out})"
    fail=1
fi
if ! grep -q "exit code 7" <<<"${out}"; then
    echo "FAIL case5: expected 'Finished with exit code 7' (got: ${out})"
    fail=1
fi

# --- case 6: --json-file contract — capture is the default -------------------
# default (no QWEN_STUB_VERSION, no CODING_CLI_JSON_BOUNDARY): the JSON-OUT
# (capture) branch for EVERY version — argv carries -o json and NOT
# --json-file, and the wrapper's own stdout capture is what lands the file
# (the stub writes none).
json_out="${TMP}/json-out/result.json"
mkdir -p "$(dirname "${json_out}")"
rm -f "${STUB_ARGS}" "${json_out}"
out="$(CFG="" run_qwen --json-file "${json_out}")"
if ! have_arg "-o" || ! have_arg "json"; then
    echo "FAIL case6-json-out: -o json should reach the runner argv (got: $(argv_show))"
    fail=1
fi
if have_arg "--json-file"; then
    echo "FAIL case6-json-out: --json-file must NOT reach the runner argv without a boundary override (got: $(argv_show))"
    fail=1
fi
if [ ! -f "${json_out}" ]; then
    echo "FAIL case6-json-out: the wrapper's stdout capture did not land ${json_out}"
    fail=1
fi
# The contract is a PARSEABLE audit file, not just a present one.
if command -v python3 >/dev/null 2>&1; then
    if ! python3 -m json.tool "${json_out}" >/dev/null 2>&1; then
        echo "FAIL case6-json-out: the captured audit at ${json_out} is not parseable JSON"
        fail=1
    fi
else
    echo "SKIP case6-json-out parse assertion (python3 absent)"
fi

# --- case 6b: the same contract as seen by 0.26.3 ---------------------------
# Field-verified 2026-10-28: qwen-code 0.26.3 still writes no --json-file in
# headless one-shot mode (exit 0, plain stdout, no file), and -o json still
# yields a parseable event array. The boundary branch must therefore keep
# applying unchanged for the current CLI line, not just 0.25.3.
json_out2="${TMP}/json-out-0263/result.json"
mkdir -p "$(dirname "${json_out2}")"
rm -f "${STUB_ARGS}" "${json_out2}"
out="$(QWEN_STUB_VERSION=0.26.3 CFG="" run_qwen --json-file "${json_out2}")"
rc=$?
if [ "${rc}" -ne 0 ]; then
    echo "FAIL case6b-json-out-0263: a normal json-out run should exit 0 (got ${rc}): ${out}"
    fail=1
fi
if ! have_arg "-o" || ! have_arg "json"; then
    echo "FAIL case6b-json-out-0263: -o json should reach the runner argv for 0.26.3 (got: $(argv_show))"
    fail=1
fi
if have_arg "--json-file"; then
    echo "FAIL case6b-json-out-0263: --json-file must NOT reach the runner argv for 0.26.3 (got: $(argv_show))"
    fail=1
fi
if [ ! -f "${json_out2}" ]; then
    echo "FAIL case6b-json-out-0263: the wrapper's stdout capture did not land ${json_out2}"
    fail=1
fi
if command -v python3 >/dev/null 2>&1; then
    if ! python3 -m json.tool "${json_out2}" >/dev/null 2>&1; then
        echo "FAIL case6b-json-out-0263: the captured audit at ${json_out2} is not parseable JSON"
        fail=1
    fi
else
    echo "SKIP case6b-json-out-0263 parse assertion (python3 absent)"
fi

# --- case 6c: the same contract as seen by 0.25.2 ---------------------------
# The default must NOT route 0.25.2 to the legacy pass-through: the old
# 0.25.3 boundary misrouted it into a branch that cannot produce a file
# (measured 2026-10-30: 0.25.2 headless one-shot exits 0 and writes no
# --json-file, with or without --yolo/-o json). With no explicit override,
# 0.25.2 takes the capture branch exactly like 0.26.3.
json_out3="${TMP}/json-out-0252/result.json"
mkdir -p "$(dirname "${json_out3}")"
rm -f "${STUB_ARGS}" "${json_out3}"
out="$(QWEN_STUB_VERSION=0.25.2 CFG="" run_qwen --json-file "${json_out3}")"
rc=$?
if [ "${rc}" -ne 0 ]; then
    echo "FAIL case6c-json-out-0252: a normal json-out run should exit 0 (got ${rc}): ${out}"
    fail=1
fi
if ! have_arg "-o" || ! have_arg "json"; then
    echo "FAIL case6c-json-out-0252: -o json should reach the runner argv for 0.25.2 (got: $(argv_show))"
    fail=1
fi
if have_arg "--json-file"; then
    echo "FAIL case6c-json-out-0252: --json-file must NOT reach the runner argv for 0.25.2 (got: $(argv_show))"
    fail=1
fi
if [ ! -f "${json_out3}" ]; then
    echo "FAIL case6c-json-out-0252: the wrapper's stdout capture did not land ${json_out3}"
    fail=1
fi
if command -v python3 >/dev/null 2>&1; then
    if ! python3 -m json.tool "${json_out3}" >/dev/null 2>&1; then
        echo "FAIL case6c-json-out-0252: the captured audit at ${json_out3} is not parseable JSON"
        fail=1
    fi
else
    echo "SKIP case6c-json-out-0252 parse assertion (python3 absent)"
fi

# --- case 6d: an explicit override pins the legacy pass-through ------------
# With CODING_CLI_JSON_BOUNDARY=0.25.3 and stub 0.25.2 (below it) the LEGACY
# branch is exercised: the argv carries --json-file with the wrapper's
# unique per-run capture path (NOT the target). A stub that writes no file
# must fail the run loudly (json-file-assertion) — the guard for an
# operator who pins an older CLI (no supported version writes the native
# file, so the pin cannot produce the contract).
json_pin="${TMP}/json-pinned/result.json"
mkdir -p "$(dirname "${json_pin}")"
rm -f "${STUB_ARGS}" "${json_pin}"
out="$(CODING_CLI_JSON_BOUNDARY=0.25.3 QWEN_STUB_VERSION=0.25.2 QWEN_STUB_MODE=no-file CFG="" run_qwen --json-file "${json_pin}")"
rc=$?
if [ "${rc}" -eq 0 ]; then
    echo "FAIL case6d-legacy-pinned: a pinned CLI that writes no file must fail the run (got exit 0): ${out}"
    fail=1
fi
if ! have_arg "--json-file"; then
    echo "FAIL case6d-legacy-pinned: the legacy pass-through should reach the runner argv (got: $(argv_show))"
    fail=1
fi
if have_arg "-o"; then
    echo "FAIL case6d-legacy-pinned: -o must NOT reach the runner argv on the legacy branch (got: $(argv_show))"
    fail=1
fi
cap_path="$(awk 'flag {print; exit} /^--json-file$/ {flag=1}' "${STUB_ARGS}" 2>/dev/null)"
case "${cap_path:-}" in
    "${json_pin}".run-*.tmp) : ;;
    *)
        echo "FAIL case6d-legacy-pinned: the CLI should receive the unique per-run capture path, not the target (argv --json-file value: ${cap_path:-<none>})"
        fail=1
        ;;
esac
last_rec="$(tail -n 1 "${runs_jsonl}" 2>/dev/null)"
if grep -q '"status": "ok"' <<<"${last_rec}"; then
    echo "FAIL case6d-legacy-pinned: the run record must not be ok (got: ${last_rec})"
    fail=1
fi
if ! grep -q '"reason": "json-file-assertion"' <<<"${last_rec}"; then
    echo "FAIL case6d-legacy-pinned: the record should carry json-file-assertion (got: ${last_rec})"
    fail=1
fi
if [ -e "${json_pin}" ]; then
    echo "FAIL case6d-legacy-pinned: no audit file may land at the target when the pinned CLI writes nothing (found ${json_pin})"
    fail=1
fi
if ls "${json_pin}".run-*.tmp >/dev/null 2>&1; then
    echo "FAIL case6d-legacy-pinned: a per-run capture tmp was left behind: $(ls "${json_pin}".run-*.tmp)"
    fail=1
fi

# --- case 7: CLI missing (recreation wiped the install) -> actionable error --
# The recreation-wipe scenario: the CLI is configured (config says enabled) but
# its npm -g install was wiped when the worker container was recreated, while
# the auth config in the agent home survived. The pre-flight guard must fail
# fast with an actionable re-install message (exit 125), not a confusing
# mid-run "command not found". Run with a clean PATH that lacks the qwen stub
# to simulate the wiped install.
out="$(PATH="/usr/bin:/bin" \
    QWEN_STUB_ARGS="${STUB_ARGS}" \
    CODING_CLI_CONFIG="${TMP}/nonexistent.json" \
    bash "${RUN_SCRIPT}" --cli qwen --workspace "${workspace}" --prompt-file "${prompt}" 2>&1)"
rc=$?
if [ "${rc}" -ne 125 ]; then
    echo "FAIL case7: expected exit 125 when the CLI binary is missing (got ${rc}): ${out}"
    fail=1
fi
if ! grep -q "not on PATH" <<<"${out}"; then
    echo "FAIL case7: expected an actionable 'not on PATH' message (got: ${out})"
    fail=1
fi
if ! grep -qi "install endpoint\|recreat" <<<"${out}"; then
    echo "FAIL case7: expected an actionable re-install hint (got: ${out})"
    fail=1
fi

# --- cases 8-10: default config path with a moved HOME (#1409) --------------
# These do NOT set CODING_CLI_CONFIG: they exercise the default resolution
# with HOME (the delegating agent's original home) distinct from
# QWENPAW_WORKING_DIR (the persisted runtime home the wrapper moves HOME to).
# All three assert config-driven flags, so they need jq and follow the
# suite's skip-when-absent convention.
if [ "${HAS_JQ}" = "1" ]; then
    # --- case 8: distinct-home default config — original home is read ------
    orig_home="${TMP}/homes/orig8"
    work_home="${TMP}/homes/work8"
    mkdir -p "${orig_home}" "${work_home}"
    printf '{"approval_mode":"plan","max_session_turns":3,"max_wall_time":"30s","sandbox":true}' > "${orig_home}/coding-cli-config.json"
    rm -f "${STUB_ARGS}"
    out="$(run_qwen_distinct_home "${orig_home}" "${work_home}")"
    rc=$?
    if [ "${rc}" -ne 0 ]; then
        echo "FAIL case8: distinct-home run should exit 0 (got ${rc}): ${out}"
        fail=1
    fi
    for want in --approval-mode plan --max-session-turns 3 --max-wall-time 30s --sandbox; do
        if ! have_arg "${want}"; then
            echo "FAIL case8: runner argv missing '${want}' (got: $(argv_show))"
            fail=1
        fi
    done
    if have_arg "--yolo"; then
        echo "FAIL case8: approval_mode plan from the original home must not add --yolo (got: $(argv_show))"
        fail=1
    fi

    # --- case 9: new-home fallback — config only in the effective home -----
    orig_home="${TMP}/homes/orig9"
    work_home="${TMP}/homes/work9"
    mkdir -p "${orig_home}" "${work_home}"
    printf '{"approval_mode":"plan","max_session_turns":3,"max_wall_time":"30s","sandbox":true}' > "${work_home}/coding-cli-config.json"
    rm -f "${STUB_ARGS}"
    out="$(run_qwen_distinct_home "${orig_home}" "${work_home}")"
    rc=$?
    if [ "${rc}" -ne 0 ]; then
        echo "FAIL case9: new-home fallback run should exit 0 (got ${rc}): ${out}"
        fail=1
    fi
    for want in --approval-mode plan --max-session-turns 3 --max-wall-time 30s --sandbox; do
        if ! have_arg "${want}"; then
            echo "FAIL case9: effective-home config must still be read (got: $(argv_show))"
            fail=1
        fi
    done

    # --- case 10: both homes set — the original home's config wins ---------
    orig_home="${TMP}/homes/orig10"
    work_home="${TMP}/homes/work10"
    mkdir -p "${orig_home}" "${work_home}"
    printf '{"approval_mode":"plan","max_session_turns":3,"max_wall_time":"30s","sandbox":true}' > "${orig_home}/coding-cli-config.json"
    printf '{"approval_mode":"yolo","max_session_turns":9,"max_wall_time":"1h","sandbox":false}' > "${work_home}/coding-cli-config.json"
    rm -f "${STUB_ARGS}"
    out="$(run_qwen_distinct_home "${orig_home}" "${work_home}")"
    rc=$?
    if [ "${rc}" -ne 0 ]; then
        echo "FAIL case10: both-homes run should exit 0 (got ${rc}): ${out}"
        fail=1
    fi
    if ! have_arg "--approval-mode" || ! have_arg "plan"; then
        echo "FAIL case10: the original home's config must win (got: $(argv_show))"
        fail=1
    fi
    if have_arg "--yolo" || have_arg "9" || have_arg "1h"; then
        echo "FAIL case10: the effective home's config must not apply (got: $(argv_show))"
        fail=1
    fi
else
    echo "SKIP cases 8-10 distinct-home default config (jq absent)"
fi
# --- cases 11-13: the captured-JSON contract (json-out branch) -------------
# The maintainer's repro: a CLI that exits 0 with an unusable capture must
# NOT be recorded as ok with a zero-byte/invalid audit at the target. The
# capture branch is the default for every version (these cases run the
# 0.26.3 stub with no explicit boundary override). Each failing run appends
# exactly one record, so the LAST line of the shared runs.jsonl (defined
# before case 1) is this run's record.

# --- case 11: empty capture (exit 0, zero-byte stdout) -> fail, no target --
json_empty="${TMP}/json-empty/result.json"
mkdir -p "$(dirname "${json_empty}")"
rm -f "${STUB_ARGS}" "${json_empty}"
out="$(QWEN_STUB_VERSION=0.26.3 QWEN_STUB_MODE=empty-json CFG="" run_qwen --json-file "${json_empty}")"
rc=$?
if [ "${rc}" -eq 0 ]; then
    echo "FAIL case 11: an empty json-out capture must fail the run (got exit 0): ${out}"
    fail=1
fi
last_rec="$(tail -n 1 "${runs_jsonl}" 2>/dev/null)"
if grep -q '"status": "ok"' <<<"${last_rec}"; then
    echo "FAIL case 11: the run record must not be ok (got: ${last_rec})"
    fail=1
fi
if ! grep -q '"reason": "json-file-assertion"' <<<"${last_rec}"; then
    echo "FAIL case 11: the record should carry json-file-assertion (got: ${last_rec})"
    fail=1
fi
if [ -e "${json_empty}" ]; then
    echo "FAIL case 11: no audit file may land at the target on an empty capture (found ${json_empty})"
    fail=1
fi

# --- case 12: malformed capture (exit 0, bad JSON) -> fail, no target ------
json_bad="${TMP}/json-bad/result.json"
mkdir -p "$(dirname "${json_bad}")"
rm -f "${STUB_ARGS}" "${json_bad}"
out="$(QWEN_STUB_VERSION=0.26.3 QWEN_STUB_MODE=bad-json CFG="" run_qwen --json-file "${json_bad}")"
rc=$?
if [ "${rc}" -eq 0 ]; then
    echo "FAIL case 12: a malformed json-out capture must fail the run (got exit 0): ${out}"
    fail=1
fi
last_rec="$(tail -n 1 "${runs_jsonl}" 2>/dev/null)"
if grep -q '"status": "ok"' <<<"${last_rec}"; then
    echo "FAIL case 12: the run record must not be ok (got: ${last_rec})"
    fail=1
fi
if ! grep -q '"reason": "json-file-assertion"' <<<"${last_rec}"; then
    echo "FAIL case 12: the record should carry json-file-assertion (got: ${last_rec})"
    fail=1
fi
if [ -e "${json_bad}" ]; then
    echo "FAIL case 12: no audit file may land at the target on a malformed capture (found ${json_bad})"
    fail=1
fi

# --- case 13: capture cannot be delivered -> json-capture-failed, no ok ---
# The target sits under a REGULAR FILE (notadir): the pre-flight -e check
# sees no target and lets the run start, the capture validates fine in the
# log dir, but the move into place fails. A write failure must never be
# recorded as ok.
touch "${TMP}/notadir"
json_nodir="${TMP}/notadir/x.json"
rm -f "${STUB_ARGS}"
out="$(QWEN_STUB_VERSION=0.26.3 CFG="" run_qwen --json-file "${json_nodir}")"
rc=$?
if [ "${rc}" -eq 0 ]; then
    echo "FAIL case 13: an undeliverable capture must fail the run (got exit 0): ${out}"
    fail=1
fi
last_rec="$(tail -n 1 "${runs_jsonl}" 2>/dev/null)"
if grep -q '"status": "ok"' <<<"${last_rec}"; then
    echo "FAIL case 13: the run record must not be ok (got: ${last_rec})"
    fail=1
fi
if ! grep -q '"reason": "json-capture-failed"' <<<"${last_rec}"; then
    echo "FAIL case 13: the record should carry json-capture-failed (got: ${last_rec})"
    fail=1
fi

# --- cases 14-16: the legacy per-run capture contract (explicit override) --
# The maintainer's repro on the legacy branch: a CLI that exits 0 without
# writing its --json-file output used to be recorded as ok with a VALID audit
# left by a previous run at the target. The wrapper now names a unique
# per-run capture for the CLI, validates that capture, and only then moves it
# onto the target — so the assertion is against THIS run's output, and a
# stale target can neither pass nor be touched. Since the capture branch is
# now the default for every version, each case below pins
# CODING_CLI_JSON_BOUNDARY=0.25.3 explicitly (0.24.0 < 0.25.3 -> legacy).

# --- case 14: stale target + a CLI that writes nothing -> fail, old file ---
# left untouched (neither consumed as this run's artifact nor deleted).
json_stale="${TMP}/json-stale/result.json"
mkdir -p "$(dirname "${json_stale}")"
echo '{"stale":true}' > "${json_stale}"
rm -f "${STUB_ARGS}"
out="$(CODING_CLI_JSON_BOUNDARY=0.25.3 QWEN_STUB_VERSION=0.24.0 QWEN_STUB_MODE=no-file CFG="" run_qwen --json-file "${json_stale}")"
rc=$?
if [ "${rc}" -eq 0 ]; then
    echo "FAIL case 14: a stale target must not make a no-op run exit 0 (got exit 0): ${out}"
    fail=1
fi
last_rec="$(tail -n 1 "${runs_jsonl}" 2>/dev/null)"
if grep -q '"status": "ok"' <<<"${last_rec}"; then
    echo "FAIL case 14: the run record must not be ok (got: ${last_rec})"
    fail=1
fi
if ! grep -q '"reason": "json-file-assertion"' <<<"${last_rec}"; then
    echo "FAIL case 14: the record should carry json-file-assertion (got: ${last_rec})"
    fail=1
fi
if [ ! -f "${json_stale}" ] || [ "$(cat "${json_stale}")" != '{"stale":true}' ]; then
    echo "FAIL case 14: the pre-existing target must be left untouched (got: $(cat "${json_stale}" 2>/dev/null))"
    fail=1
fi
if ls "${json_stale}".run-*.tmp >/dev/null 2>&1; then
    echo "FAIL case 14: a per-run capture tmp was left behind: $(ls "${json_stale}".run-*.tmp)"
    fail=1
fi

# --- case 15: legacy normal — the stub writes the per-run capture the CLI --
# was told to write; the wrapper validates it and moves it onto the target.
json_ok="${TMP}/json-legacy/result.json"
mkdir -p "$(dirname "${json_ok}")"
rm -f "${STUB_ARGS}" "${json_ok}"
out="$(CODING_CLI_JSON_BOUNDARY=0.25.3 QWEN_STUB_VERSION=0.24.0 CFG="" run_qwen --json-file "${json_ok}")"
rc=$?
if [ "${rc}" -ne 0 ]; then
    echo "FAIL case 15: a normal legacy run should exit 0 (got ${rc}): ${out}"
    fail=1
fi
cap_path="$(awk 'flag {print; exit} /^--json-file$/ {flag=1}' "${STUB_ARGS}" 2>/dev/null)"
case "${cap_path:-}" in
    "${json_ok}".run-*.tmp) : ;;
    *)
        echo "FAIL case 15: the CLI should receive the unique per-run capture path, not the target (argv --json-file value: ${cap_path:-<none>})"
        fail=1
        ;;
esac
if [ -n "${cap_path}" ] && [ -e "${cap_path}" ]; then
    echo "FAIL case 15: the validated capture must have been moved, not left behind (${cap_path})"
    fail=1
fi
want_content='{"type":"result","subtype":"success","result":"stub"}'
if [ ! -f "${json_ok}" ] || [ "$(cat "${json_ok}")" != "${want_content}" ]; then
    echo "FAIL case 15: the target must hold the stub's capture content (got: $(cat "${json_ok}" 2>/dev/null))"
    fail=1
fi
last_json_event="$(grep -h '"event":"coding-cli-json-branch"' "${workspace}"/coding-cli-logs/run-*.log 2>/dev/null | tail -n 1)"
if ! grep -q '"assert":"pass"' <<<"${last_json_event:-}"; then
    echo "FAIL case 15: the event log should carry assert=pass (got: ${last_json_event:-<none>})"
    fail=1
fi

# --- case 16: legacy CLI failure (exit 7, no file) — code propagated, -----
# no capture tmp behind, a pre-existing target left untouched.
json_fail7="${TMP}/json-fail7/result.json"
mkdir -p "$(dirname "${json_fail7}")"
echo '{"stale":true}' > "${json_fail7}"
rm -f "${STUB_ARGS}"
out="$(CODING_CLI_JSON_BOUNDARY=0.25.3 QWEN_STUB_VERSION=0.24.0 QWEN_STUB_MODE=fail CFG="" run_qwen --json-file "${json_fail7}")"
rc=$?
if [ "${rc}" -ne 7 ]; then
    echo "FAIL case 16: the runner's exit code 7 must be propagated (got ${rc}): ${out}"
    fail=1
fi
last_rec="$(tail -n 1 "${runs_jsonl}" 2>/dev/null)"
if grep -q '"status": "ok"' <<<"${last_rec}"; then
    echo "FAIL case 16: the run record must not be ok (got: ${last_rec})"
    fail=1
fi
if [ ! -f "${json_fail7}" ] || [ "$(cat "${json_fail7}")" != '{"stale":true}' ]; then
    echo "FAIL case 16: the pre-existing target must be left untouched (got: $(cat "${json_fail7}" 2>/dev/null))"
    fail=1
fi
if ls "${json_fail7}".run-*.tmp >/dev/null 2>&1; then
    echo "FAIL case 16: a per-run capture tmp was left behind: $(ls "${json_fail7}".run-*.tmp)"
    fail=1
fi

if [ "${fail}" -eq 0 ]; then
    echo "PASS coding-cli run governance (jq=${HAS_JQ})"
    exit 0
fi
echo "FAIL coding-cli run governance (jq=${HAS_JQ})"
exit 1
