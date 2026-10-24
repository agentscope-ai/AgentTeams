#!/bin/bash
# check-coding-cli-runs-metadata.sh
#
# Tests run-coding-cli.sh's runs.jsonl outcome-metadata append — the write side
# of the coding-runs read enrichment: every exit path after argument validation
# (ok / timeout / blocked-marker / blocked-cli) appends exactly one JSON line
# carrying the outcome, and --worker / AGENTTEAMS_WORKER_NAME land as
# worker / runner. Uses a STUB qwen on PATH — no real runner required, so this
# runs in CI.
#
# Cases:
#   1. ok          — --worker + runner env: one line with v=1, run=<the log
#                    name>, cli, worker, runner, status=ok, exit_code=0 (shown,
#                    not omitted as "unknown"), timeout_secs, RFC3339 UTC
#                    started_at/ended_at, log_bytes>0, prompt_file,
#                    prompt_sha256 (== the prompt's sha256), prompt_summary
#                    (single-lined first 200 chars).
#   2. timeout     — the stub outlives --timeout: status=timeout, exit_code=124.
#   3. blocked     — a conflicting .processing marker: status=blocked,
#                    reason=marker-conflict, exit_code=125, the CLI never ran —
#                    AND a diagnostic transcript exists that explains the
#                    block (the read API enumerates transcripts, so an
#                    index-only attempt would be invisible).
#   4. cli-missing — the CLI binary is not on PATH: status=blocked,
#                    reason=cli-missing, exit_code=125, same transcript
#                    requirement.
#   5. worker-omit — no --worker: the worker key is absent (empty values are
#                    omitted, aligned with the API's omitempty contract); the
#                    200-character prompt-summary truncation is pinned here.
#   6. append-only — two runs in one workspace -> two lines, one per run.
#   7. bad-approval — an invalid approval tier: status=failed,
#                    reason=invalid-approval-mode, and a transcript that names
#                    the refusal (every post-validation exit explains itself).
#   8. non-UTC TZ  — with a non-UTC container clock the record's started_at is
#                    still a real UTC instant (`date -u`), never the local
#                    stamp dressed as UTC.
#   9. ro-log-dir  — an unwritable/read-only coding-cli-logs/ fails the run
#                    PROMPTLY with a visible diagnostic, without invoking the
#                    CLI (a create failure is not a name collision — retrying
#                    it would spin outside the CLI timeout).
#  10. collision   — a same-second transcript is never clobbered: the claim
#                    steps to a free name and both transcripts survive.
#  11. bad-stamp   — a clock that cannot produce run-YYYYMMDD-HHMMSS.log
#                    refuses the run (an unenumerable name would be invisible).
#  12. invariant   — sweep: every recorded run names an existing transcript.
#
# jq + python3 are required (the assertions read the JSONL; the wrapper builds
# each line with python3) — SKIP when either is absent, per the check-suite
# convention.
set -u

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_SCRIPT="${REPO_ROOT}/manager/agent/skills/coding-cli-management/scripts/run-coding-cli.sh"
if [ ! -f "${RUN_SCRIPT}" ]; then
    echo "FAIL setup (run-coding-cli.sh not found: ${RUN_SCRIPT})"
    exit 1
fi

command -v jq >/dev/null 2>&1 || { echo "SKIP: jq required" >&2; exit 0; }
command -v python3 >/dev/null 2>&1 || { echo "SKIP: python3 required (runs.jsonl lines are python-built)" >&2; exit 0; }

bash -n "${RUN_SCRIPT}"

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

# Stub qwen: record its argv; behavior selected by QWEN_STUB_MODE (default
# exit 0 / sleep, to trip the wrapper's --timeout / fail with exit 7).
STUB_BIN="${TMP}/bin"
STUB_ARGS="${TMP}/qwen-args.txt"
mkdir -p "${STUB_BIN}"
cat > "${STUB_BIN}/qwen" <<'STUB'
#!/bin/bash
printf '%s\n' "$@" > "${QWEN_STUB_ARGS}"
case "${QWEN_STUB_MODE:-default}" in
    sleep) sleep 5; exit 0 ;;
    fail)  exit 7 ;;
    *)     exit 0 ;;
esac
STUB
chmod +x "${STUB_BIN}/qwen"

fail=0
prompt="${TMP}/prompt.txt"
printf 'line one\nline two\n' > "${prompt}"
prompt_sha="$(sha256sum "${prompt}" | awk '{print $1}')"

jsonl_of() { echo "$1/coding-cli-logs/runs.jsonl"; }
last_line() { tail -n 1 "$(jsonl_of "$1")" 2>/dev/null; }

assert_eq() { # $1 label, $2 got, $3 want
    if [ "$2" != "$3" ]; then
        echo "FAIL $1: got '$2', want '$3'"
        fail=1
    fi
}

assert_jq() { # $1 label, $2 json text, $3 jq predicate (true = pass)
    if ! jq -e "$3" <<<"$2" >/dev/null 2>&1; then
        echo "FAIL $1: jq -e '$3' (got: ${2:-<empty>})"
        fail=1
    fi
}

field() { jq -r "$2" <<<"$1" 2>/dev/null; }

# assert_transcript <label> <workspace> <jsonl line> [diagnostic needle]
# The recorded run must name an existing transcript, the transcript must carry
# the run header, and (given a needle) the diagnostic that explains the
# outcome. The read API enumerates transcripts, so an index line without one
# is exactly the review's gap: mirrored but invisible.
assert_transcript() {
    local label="$1" ws="$2" line="$3" needle="${4:-}"
    local run_name tpath
    run_name="$(field "${line}" '.run')"
    tpath="${ws}/coding-cli-logs/${run_name}"
    if [ -z "${run_name}" ] || [ ! -f "${tpath}" ]; then
        echo "FAIL ${label}: the index line must name an existing transcript (run='${run_name}')"
        fail=1
        return
    fi
    grep -qF "[run-coding-cli] cli=" "${tpath}" \
        || { echo "FAIL ${label}: transcript is missing the run header (${tpath})"; fail=1; }
    if [ -n "${needle}" ] && ! grep -qF "${needle}" "${tpath}"; then
        echo "FAIL ${label}: transcript does not explain the outcome ('${needle}' absent from ${tpath})"
        fail=1
    fi
    assert_jq "${label} log_bytes > 0" "${line}" '.log_bytes > 0'
}

run_cli() { # $1 = workspace, $2 = prompt file, $@ = extra wrapper flags
    local ws="$1" pf="$2"; shift 2
    PATH="${STUB_BIN}:${PATH}" \
    QWEN_STUB_ARGS="${STUB_ARGS}" \
    CODING_CLI_CONFIG="${TMP}/nonexistent.json" \
    AGENTTEAMS_WORKER_NAME="runner-1" \
    bash "${RUN_SCRIPT}" --cli qwen --workspace "${ws}" --prompt-file "${pf}" "$@" 2>&1
}

# --- case 1: ok path — every field lands ------------------------------------
ws1="${TMP}/ws1"; mkdir -p "${ws1}"
rm -f "${STUB_ARGS}"
out="$(run_cli "${ws1}" "${prompt}" --worker sysdev-coder)"
assert_eq "case1 exit" "$?" "0"
line="$(last_line "${ws1}")"
if [ -z "${line}" ]; then
    echo "FAIL case1: no runs.jsonl line was appended (out: ${out})"
    fail=1
else
    assert_eq "case1 line count" "$(wc -l < "$(jsonl_of "${ws1}")" | tr -d ' ')" "1"
    logname="$(basename "$(ls "${ws1}"/coding-cli-logs/run-*.log | head -n 1)")"
    assert_eq "case1 v" "$(field "${line}" '.v')" "1"
    assert_eq "case1 run == the log file" "$(field "${line}" '.run')" "${logname}"
    assert_eq "case1 cli" "$(field "${line}" '.cli')" "qwen"
    assert_eq "case1 worker" "$(field "${line}" '.worker')" "sysdev-coder"
    assert_eq "case1 runner" "$(field "${line}" '.runner')" "runner-1"
    assert_eq "case1 status" "$(field "${line}" '.status')" "ok"
    assert_eq "case1 exit_code 0 shown" "$(field "${line}" '.exit_code')" "0"
    assert_eq "case1 timeout_secs" "$(field "${line}" '.timeout_secs')" "3600"
    assert_jq "case1 log_bytes > 0" "${line}" '.log_bytes > 0'
    assert_eq "case1 prompt_file" "$(field "${line}" '.prompt_file')" "${prompt}"
    assert_eq "case1 prompt_sha256" "$(field "${line}" '.prompt_sha256')" "${prompt_sha}"
    assert_eq "case1 prompt_summary single-lined" "$(field "${line}" '.prompt_summary')" "line one line two"
    assert_jq "case1 started_at is RFC3339 UTC" "${line}" '.started_at | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")'
    assert_jq "case1 ended_at is RFC3339 UTC" "${line}" '.ended_at | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")'
    assert_jq "case1 reason omitted on ok" "${line}" 'has("reason") | not'
fi

# --- case 2: timeout — the stub outlives --timeout ---------------------------
ws2="${TMP}/ws2"; mkdir -p "${ws2}"
out="$(QWEN_STUB_MODE=sleep run_cli "${ws2}" "${prompt}" --timeout 1)"
assert_eq "case2 exit" "$?" "124"
line="$(last_line "${ws2}")"
assert_eq "case2 status" "$(field "${line}" '.status')" "timeout"
assert_eq "case2 exit_code" "$(field "${line}" '.exit_code')" "124"
assert_eq "case2 timeout_secs" "$(field "${line}" '.timeout_secs')" "1"
assert_jq "case2 ended_at present" "${line}" '.ended_at != ""'

# --- case 3: blocked — a conflicting .processing marker ----------------------
task3="${TMP}/task3"; ws3="${task3}/workspace"; mkdir -p "${ws3}"
jq -n --arg p alice --arg e "$(date -u -d '+15 minutes' +%Y-%m-%dT%H:%M:%SZ)" \
    '{processor:$p, expires_at:$e}' > "${task3}/.processing"
rm -f "${STUB_ARGS}"
out="$(run_cli "${ws3}" "${prompt}" --task-dir "${task3}" --processor bob)"
assert_eq "case3 exit" "$?" "125"
assert_eq "case3 CLI never ran" "$([ -f "${STUB_ARGS}" ] && echo ran || echo skipped)" "skipped"
line="$(last_line "${ws3}")"
assert_eq "case3 status" "$(field "${line}" '.status')" "blocked"
assert_eq "case3 reason" "$(field "${line}" '.reason')" "marker-conflict"
assert_eq "case3 exit_code" "$(field "${line}" '.exit_code')" "125"
assert_jq "case3 run name shape" "${line}" '.run | test("^run-[0-9]{8}-[0-9]{6}[.]log$")'
# The attempt must leave a READABLE transcript, not just an index line: the
# read API enumerates transcripts, so an index-only attempt never reaches the
# list or detail (review finding).
assert_transcript "case3" "${ws3}" "${line}" "is held by"

# --- case 4: cli-missing — the binary is not on PATH -------------------------
ws4="${TMP}/ws4"; mkdir -p "${ws4}"
out="$(PATH="/usr/bin:/bin" QWEN_STUB_ARGS="${STUB_ARGS}" \
    CODING_CLI_CONFIG="${TMP}/nonexistent.json" AGENTTEAMS_WORKER_NAME="runner-1" \
    bash "${RUN_SCRIPT}" --cli qwen --workspace "${ws4}" --prompt-file "${prompt}" 2>&1)"
assert_eq "case4 exit" "$?" "125"
line="$(last_line "${ws4}")"
assert_eq "case4 status" "$(field "${line}" '.status')" "blocked"
assert_eq "case4 reason" "$(field "${line}" '.reason')" "cli-missing"
assert_eq "case4 exit_code" "$(field "${line}" '.exit_code')" "125"
assert_transcript "case4" "${ws4}" "${line}" "not on PATH"

# --- case 5: no --worker -> the worker key is omitted; summary truncates -----
ws5="${TMP}/ws5"; mkdir -p "${ws5}"
prompt5="${TMP}/prompt5.txt"
printf 'x%.0s' {1..300} > "${prompt5}"
out="$(run_cli "${ws5}" "${prompt5}")"
assert_eq "case5 exit" "$?" "0"
line="$(last_line "${ws5}")"
assert_jq "case5 worker omitted" "${line}" 'has("worker") | not'
assert_eq "case5 runner still recorded" "$(field "${line}" '.runner')" "runner-1"
summary5="$(field "${line}" '.prompt_summary')"
assert_eq "case5 summary truncated to the first 200 chars" "${#summary5}" "200"

# --- case 6: append-only — one line (and one transcript) per run -------------
# Three back-to-back runs land in the same wall-clock second often enough that
# the name claim must step forward on every collision: each run gets its own
# transcript, never a shared/merged one.
ws6="${TMP}/ws6"; mkdir -p "${ws6}"
run_cli "${ws6}" "${prompt}" --worker w1 >/dev/null 2>&1
run_cli "${ws6}" "${prompt}" --worker w2 >/dev/null 2>&1
run_cli "${ws6}" "${prompt}" --worker w3 >/dev/null 2>&1
assert_eq "case6 line count" "$(wc -l < "$(jsonl_of "${ws6}")" | tr -d ' ')" "3"
assert_eq "case6 transcript count" "$(ls "${ws6}/coding-cli-logs"/run-*.log | wc -l | tr -d ' ')" "3"
assert_eq "case6 distinct run names" "$(jq -r '.run' "$(jsonl_of "${ws6}")" | sort -u | wc -l | tr -d ' ')" "3"
assert_eq "case6 line1 status" "$(field "$(sed -n 1p "$(jsonl_of "${ws6}")")" '.status')" "ok"
assert_eq "case6 line1 worker" "$(field "$(sed -n 1p "$(jsonl_of "${ws6}")")" '.worker')" "w1"
assert_eq "case6 line3 worker" "$(field "$(sed -n 3p "$(jsonl_of "${ws6}")")" '.worker')" "w3"

# --- case 7: invalid approval tier -> failed + a self-explaining transcript --
ws7="${TMP}/ws7"; mkdir -p "${ws7}"
out="$(run_cli "${ws7}" "${prompt}" --approval-mode bogus)"
assert_eq "case7 exit" "$?" "1"
line="$(last_line "${ws7}")"
assert_eq "case7 status" "$(field "${line}" '.status')" "failed"
assert_eq "case7 reason" "$(field "${line}" '.reason')" "invalid-approval-mode"
assert_eq "case7 exit_code" "$(field "${line}" '.exit_code')" "1"
assert_transcript "case7" "${ws7}" "${line}" "invalid approval mode"

# --- case 8: non-UTC container — the record's instant is still UTC -----------
# The wrapper names transcripts with the CONTAINER's local clock, so a
# filename is not a UTC instant. The record's started_at must come from
# `date -u` — the reader is never handed a local stamp dressed as UTC.
ws8="${TMP}/ws8"; mkdir -p "${ws8}"
utc_hour_before="$(date -u +%H)"
out="$(TZ=Asia/Shanghai run_cli "${ws8}" "${prompt}")"
assert_eq "case8 exit" "$?" "0"
utc_hour_after="$(date -u +%H)"
line="$(last_line "${ws8}")"
assert_jq "case8 started_at is UTC" "${line}" '.started_at | test("Z$")'
if [ "${utc_hour_before}" != "$(TZ=Asia/Shanghai date +%H)" ]; then
    started_hour="$(field "${line}" '.started_at' | cut -c12-13)"
    case "${started_hour}" in
        "${utc_hour_before}"|"${utc_hour_after}") ;;
        *)
            echo "FAIL case8: started_at hour '${started_hour}' is the container's local clock, not UTC (${utc_hour_before}/${utc_hour_after})"
            fail=1
            ;;
    esac
else
    echo "SKIP case8 local-vs-UTC hour comparison (TZ data not honored here)"
fi

# --- case 9: unwritable log dir — fail promptly, never spin ------------------
# A create failure is NOT a name collision. On a read-only (or otherwise
# unwritable) coding-cli-logs/ the wrapper must refuse immediately with a
# visible diagnostic instead of retrying: that retry loop sits before the
# pre-flight guards and outside the CLI timeout, so --timeout cannot bound it.
ws9="${TMP}/ws9"; mkdir -p "${ws9}/coding-cli-logs"
chmod 500 "${ws9}/coding-cli-logs"
if ( set -C; : > "${ws9}/coding-cli-logs/.probe" ) 2>/dev/null; then
    echo "SKIP case9 read-only log dir (this user can still create files there — root?)"
    rm -f "${ws9}/coding-cli-logs/.probe"
    chmod 700 "${ws9}/coding-cli-logs"
elif ! command -v timeout >/dev/null 2>&1; then
    echo "SKIP case9 read-only log dir (no external timeout to bound the probe)"
    chmod 700 "${ws9}/coding-cli-logs"
else
    rm -f "${STUB_ARGS}"
    out="$(PATH="${STUB_BIN}:${PATH}" QWEN_STUB_ARGS="${STUB_ARGS}" \
        CODING_CLI_CONFIG="${TMP}/nonexistent.json" AGENTTEAMS_WORKER_NAME="runner-1" \
        timeout 10 bash "${RUN_SCRIPT}" --cli qwen --workspace "${ws9}" --prompt-file "${prompt}" 2>&1)"
    rc=$?
    if [ "${rc}" -eq 124 ]; then
        echo "FAIL case9: the wrapper spun on an unwritable log dir (external timeout killed it)"
        fail=1
    elif [ "${rc}" -eq 0 ]; then
        echo "FAIL case9: an unwritable log dir must not report success: ${out}"
        fail=1
    fi
    grep -qi "cannot create the run transcript\|run-log directory\|must be writable" <<<"${out}" \
        || { echo "FAIL case9: expected a visible diagnostic about the unwritable log dir (got: ${out})"; fail=1; }
    [ ! -f "${STUB_ARGS}" ] \
        || { echo "FAIL case9: the CLI must not be invoked when the log dir is unwritable"; fail=1; }
    [ ! -f "$(jsonl_of "${ws9}")" ] \
        || { echo "FAIL case9: nothing can be recorded into an unwritable log dir"; fail=1; }
    chmod 700 "${ws9}/coding-cli-logs"
fi

# --- case 10: same-second collision — never clobber a live transcript --------
# The transcript name comes from the container clock, so a blocked attempt can
# collide with the run that holds the task. A stub `date` freezes that clock
# (epoch formatting still goes through the real `date`) to make the collision
# deterministic: the claim must step to the next second and leave the existing
# transcript byte-identical.
ws10="${TMP}/ws10"; mkdir -p "${ws10}/coding-cli-logs"
stub_date_dir="${TMP}/bin-date"; mkdir -p "${stub_date_dir}"
cat > "${stub_date_dir}/date" <<'STUB'
#!/bin/bash
case "$*" in
    *"+%s")                 echo "1786000000"; exit 0 ;;
    *"-d @"*)               exec /usr/bin/date "$@" ;;
    *"+%Y%m%d-%H%M%S")      exec /usr/bin/date -d "@1786000000" +%Y%m%d-%H%M%S ;;
    *"+%Y-%m-%dT%H:%M:%SZ") echo "2026-11-10T10:14:59Z"; exit 0 ;;
esac
/usr/bin/date "$@"
STUB
chmod +x "${stub_date_dir}/date"
stamp_taken="$(PATH="${stub_date_dir}:${PATH}" date -d '@1786000000' +%Y%m%d-%H%M%S)"
stamp_free="$(PATH="${stub_date_dir}:${PATH}" date -d '@1786000001' +%Y%m%d-%H%M%S)"
sentinel="${ws10}/coding-cli-logs/run-${stamp_taken}.log"
printf 'live run transcript — must survive\n' > "${sentinel}"
out="$(PATH="${stub_date_dir}:${STUB_BIN}:${PATH}" QWEN_STUB_ARGS="${STUB_ARGS}" \
    CODING_CLI_CONFIG="${TMP}/nonexistent.json" AGENTTEAMS_WORKER_NAME="runner-1" \
    timeout 10 bash "${RUN_SCRIPT}" --cli qwen --workspace "${ws10}" --prompt-file "${prompt}" 2>&1)"
assert_eq "case10 exit" "$?" "0"
line="$(last_line "${ws10}")"
assert_eq "case10 stepped to a free name" "$(field "${line}" '.run')" "run-${stamp_free}.log"
assert_eq "case10 live transcript not clobbered" "$(cat "${sentinel}")" "live run transcript — must survive"
assert_transcript "case10" "${ws10}" "${line}"

# --- case 11: a clock that cannot name a transcript refuses the run ----------
# The read API only enumerates run-<YYYYMMDD>-<HHMMSS>.log. A candidate outside
# that shape (a broken clock/date) must refuse the run rather than claim a name
# no reader can enumerate.
ws11="${TMP}/ws11"; mkdir -p "${ws11}/coding-cli-logs"
stub_bad_dir="${TMP}/bin-bad-date"; mkdir -p "${stub_bad_dir}"
cat > "${stub_bad_dir}/date" <<'STUB'
#!/bin/bash
case "$*" in
    *"+%s")                 echo "1786000000"; exit 0 ;;
    *"-d @"*)               echo "not-a-stamp"; exit 0 ;;
    *"+%Y%m%d-%H%M%S")      exec /usr/bin/date -d "@1786000000" +%Y%m%d-%H%M%S ;;
    *"+%Y-%m-%dT%H:%M:%SZ") echo "2026-11-10T10:14:59Z"; exit 0 ;;
esac
/usr/bin/date "$@"
STUB
chmod +x "${stub_bad_dir}/date"
stamp_collide="$(PATH="${stub_bad_dir}:${PATH}" date +%Y%m%d-%H%M%S)"
printf 'live run transcript — must survive\n' > "${ws11}/coding-cli-logs/run-${stamp_collide}.log"
rm -f "${STUB_ARGS}"
out="$(PATH="${stub_bad_dir}:${STUB_BIN}:${PATH}" QWEN_STUB_ARGS="${STUB_ARGS}" \
    CODING_CLI_CONFIG="${TMP}/nonexistent.json" AGENTTEAMS_WORKER_NAME="runner-1" \
    timeout 10 bash "${RUN_SCRIPT}" --cli qwen --workspace "${ws11}" --prompt-file "${prompt}" 2>&1)"
rc=$?
if [ "${rc}" -eq 0 ] || [ "${rc}" -eq 124 ]; then
    echo "FAIL case11: an unusable transcript name must refuse promptly (got ${rc}): ${out}"
    fail=1
fi
grep -qF "unusable run transcript name" <<<"${out}" \
    || { echo "FAIL case11: expected a diagnostic naming the unusable transcript (got: ${out})"; fail=1; }
[ ! -f "${STUB_ARGS}" ] \
    || { echo "FAIL case11: the CLI must not be invoked when no transcript name can be claimed"; fail=1; }
[ ! -f "$(jsonl_of "${ws11}")" ] \
    || { echo "FAIL case11: no record may be written without an enumerable transcript"; fail=1; }
assert_eq "case11 no stray transcript" "$(ls "${ws11}/coding-cli-logs" | wc -l | tr -d ' ')" "1"

# --- case 12: invariant — every recorded run names an existing transcript -----
# The class-level guard for the review finding: regardless of outcome, a
# runs.jsonl line implies a transcript the read API can enumerate.
for ws in "${ws1}" "${ws2}" "${ws3}" "${ws4}" "${ws5}" "${ws6}" "${ws7}" "${ws8}" "${ws10}"; do
    idx="$(jsonl_of "${ws}")"
    [ -f "${idx}" ] || continue
    while IFS= read -r rec; do
        [ -n "${rec}" ] || continue
        assert_transcript "case9 $(basename "${ws}")" "${ws}" "${rec}"
    done < "${idx}"
done

if [ "${fail}" -eq 0 ]; then
    echo "PASS coding-cli runs metadata (runs.jsonl outcome records)"
    exit 0
fi
echo "FAIL coding-cli runs metadata (runs.jsonl outcome records)"
exit 1
