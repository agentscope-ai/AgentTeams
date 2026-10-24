#!/usr/bin/env bash
# Unit tests for storage-migrate.sh: argument parsing, dependency gate and the
# state-machine phase gating. No S3 endpoint is involved - a stub `mc` answers
# the version probe and every real call is expected to be rejected earlier.
#
# Usage: bash tests/storage-migrate-state-machine.sh
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TOOL="$HERE/../storage-migrate.sh"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/sm-unit.XXXXXX")"
FAILS=0
pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*"; FAILS=$((FAILS + 1)); }

# ---- stub mc: only `--version` must satisfy the dependency gate ----------
mkdir -p "$WORK/bin"
cat > "$WORK/bin/mc" <<'STUB'
#!/usr/bin/env bash
case "${1:-}" in
  --version) echo "mc version RELEASE.2025-08-13T08-35-41Z (commit-id=7394ce0d)" ; exit 0 ;;
  *) echo "stub mc: unexpected invocation: $*" >&2 ; exit 9 ;;
esac
STUB
chmod +x "$WORK/bin/mc"
export PATH="$WORK/bin:$PATH"

BASE_ARGS=(--src-endpoint http://src.example:9000 --src-ak a --src-sk b
           --dst-endpoint http://dst.example:9090 --dst-ak c --dst-sk d
           --state-dir "$WORK/state")

run_tool() { # <expected-exit> <description> <args...>
  local want="$1" desc="$2"; shift 2
  local out rc
  out=$("$@" 2>&1); rc=$?
  if [ "$rc" -eq "$want" ]; then
    pass "$desc (exit $rc)"
  else
    fail "$desc (expected exit $want, got $rc): $(printf '%s' "$out" | tail -2 | tr '\n' ' ')"
  fi
  LAST_OUT="$out"
}

expect_msg() { # <description> <needle>
  if printf '%s' "${LAST_OUT:-}" | grep -qF -- "$2"; then pass "$1 (message present)"; else
    fail "$1 (message missing: $2)"
  fi
}

echo "=== storage-migrate unit tests ==="

# 1) --help exits 0 and prints the phase list
run_tool 0 "--help exits 0" bash "$TOOL" --help
expect_msg "--help" "--phase all"

# 2) unknown argument -> usage error (4)
run_tool 4 "unknown argument -> exit 4" bash "$TOOL" --no-such-flag

# 3) missing credentials -> usage error (4)
run_tool 4 "missing source credentials -> exit 4" bash "$TOOL" --dst-endpoint http://d:1 --dst-ak a --dst-sk b

# 4) unknown phase -> usage error (4)
run_tool 4 "unknown phase -> exit 4" bash "$TOOL" "${BASE_ARGS[@]}" --phase nope

# 5) cutover on a fresh state (INIT) is refused
run_tool 1 "cutover requires VERIFIED" bash "$TOOL" "${BASE_ARGS[@]}" --phase cutover
expect_msg "cutover gate" "state.stage=INIT"

# 6) data phase requires PREFLIGHT
run_tool 1 "data requires PREFLIGHT" bash "$TOOL" "${BASE_ARGS[@]}" --phase data
expect_msg "data gate" "state.stage=INIT"

# 7) seed a PREFLIGHT state, then `data` passes the gate (it then fails inside
#    phase_data against the stub mc - the point is that the gate itself moved)
mkdir -p "$WORK/state"
jq -n '{stage:"PREFLIGHT", src:{}, stage_detail:{}}' > "$WORK/state/state.json"
run_tool 1 "data gate passes at PREFLIGHT" bash "$TOOL" "${BASE_ARGS[@]}" --phase data
if ! printf '%s' "${LAST_OUT:-}" | grep -q "state.stage="; then
  pass "data gate did not reject a PREFLIGHT state"
else
  fail "data gate rejected a PREFLIGHT state"
fi

# 8) verify requires IAM
jq -n '{stage:"DATA", src:{}, stage_detail:{}}' > "$WORK/state/state.json"
run_tool 1 "verify requires IAM" bash "$TOOL" "${BASE_ARGS[@]}" --phase verify
expect_msg "verify gate" "state.stage=DATA"

# 9) mark-cutover-done requires CUTOVER_PENDING
jq -n '{stage:"VERIFIED", src:{}, stage_detail:{}}' > "$WORK/state/state.json"
run_tool 1 "mark-cutover-done requires CUTOVER_PENDING" bash "$TOOL" "${BASE_ARGS[@]}" --mark-cutover-done
expect_msg "mark-cutover-done gate" "state.stage=VERIFIED"

# 10) cutover gate opens at VERIFIED (fails later against the stub, not on the gate)
run_tool 1 "cutover gate passes at VERIFIED" bash "$TOOL" "${BASE_ARGS[@]}" --phase cutover
if ! printf '%s' "${LAST_OUT:-}" | grep -q "cutover requires state=VERIFIED"; then
  pass "cutover gate did not reject a VERIFIED state"
else
  fail "cutover gate rejected a VERIFIED state"
fi

# 11) --status is the integration contract: stable JSON, no dependencies needed
st=$(bash "$TOOL" --status --state-dir "$WORK/status-empty" 2>&1)
if jq -e '.schema == "storage-migrate.status/v1" and .stage == "NONE"' <<<"$st" >/dev/null 2>&1; then
  pass "--status on an empty state dir reports stage NONE"
else
  fail "--status on an empty state dir reports stage NONE: $(printf '%s' "$st" | head -2 | tr '\n' ' ')"
fi
mkdir -p "$WORK/status-full"
jq -n '{stage:"VERIFIED", updated:"2026-10-23T00:00:00Z"}' > "$WORK/status-full/state.json"
jq -n '[{phase:"preflight",id:"0-1",status:"pass"},{phase:"verify",id:"3",status:"fail"}]' > "$WORK/status-full/report.json"
st2=$(bash "$TOOL" --status --state-dir "$WORK/status-full" 2>&1)
if jq -e '.stage == "VERIFIED" and .preflight["0-1"] == "pass" and .verify["3"] == "fail"' <<<"$st2" >/dev/null 2>&1; then
  pass "--status exposes the stage and per-item verdicts"
else
  fail "--status exposes the stage and per-item verdicts: $(printf '%s' "$st2" | head -2 | tr '\n' ' ')"
fi

echo
if [ "$FAILS" -eq 0 ]; then
  echo "UNIT RESULT: PASS (all assertions green)"
  exit 0
fi
echo "UNIT RESULT: FAIL ($FAILS assertion(s) failed)"
exit 1
