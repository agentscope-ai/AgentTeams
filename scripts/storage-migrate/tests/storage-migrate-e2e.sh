#!/usr/bin/env bash
# Full-chain migration test against real engines (Docker required).
#
# Covers: dry run, the full chain, an IAM failure and its resume, verify,
# cutover, the human gate, the negative gates, and an independent comparison
# that does not trust the tool's own report.
#
# Requirements on PATH: bash, jq, curl, mc (RELEASE.2025-08-13 or newer), docker.
# Usage: bash tests/storage-migrate-e2e.sh
set -uo pipefail

if ! command -v mc >/dev/null; then echo "mc not found on PATH" >&2; exit 1; fi
if ! mc --version >/dev/null 2>&1; then
  echo "mc is on PATH but does not run (a 404 page saved as a binary?): $(command -v mc)" >&2
  exit 1
fi
for c in jq curl docker; do
  command -v "$c" >/dev/null || { echo "$c not found on PATH" >&2; exit 1; }
done

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TOOL="$HERE/../storage-migrate.sh"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/sm-e2e.XXXXXX")"

MINIO_IMG="${MINIO_IMG:-higress-registry.cn-hangzhou.cr.aliyuncs.com/higress/minio:20260216}"
RUSTFS_IMG="${RUSTFS_IMG:-rustfs/rustfs:1.0.3}"
SRC_NAME=sm-e2e-src; DST_NAME=sm-e2e-dst
SRC_PORT="${SRC_PORT:-19020}"; DST_PORT="${DST_PORT:-19210}"
SRC=http://127.0.0.1:$SRC_PORT; DST=http://127.0.0.1:$DST_PORT
SAK=srcadmin; SSK=srcpass12345; DAK=dstadmin; DSK=dstpass12345
BKT=agentteams-storage
FAILS=0
pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*"; FAILS=$((FAILS + 1)); }
stage_of() { jq -r '.stage // "?"' "$1/state.json" 2>/dev/null; }

cleanup() { docker rm -f "$SRC_NAME" "$DST_NAME" >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "=== fresh engines ==="
# Remove leftovers first: a stale container still holding the port would make
# the tests run against last run's data and produce misleading results.
cleanup
docker run -d --name "$SRC_NAME" -p "$SRC_PORT":9000 \
  -e MINIO_ROOT_USER="$SAK" -e MINIO_ROOT_PASSWORD="$SSK" "$MINIO_IMG" server /data >/dev/null \
  || { echo "failed to start $SRC_NAME (port $SRC_PORT already in use?)" >&2; exit 1; }
docker run -d --name "$DST_NAME" -p "$DST_PORT":9000 \
  -e RUSTFS_ACCESS_KEY="$DAK" -e RUSTFS_SECRET_KEY="$DSK" -e RUSTFS_DURABILITY_MODE=strict \
  "$RUSTFS_IMG" /data >/dev/null \
  || { echo "failed to start $DST_NAME (port $DST_PORT already in use?)" >&2; exit 1; }
for _ in $(seq 1 60); do
  s=$(curl -s -o /dev/null -w '%{http_code}' "$SRC/minio/health/live" || true)
  d=$(curl -s -o /dev/null -w '%{http_code}' "$DST/minio/health/live" || true)
  [ "$s" = "200" ] && [ "$d" = "200" ] && break
  sleep 2
done
[ "$s" = "200" ] && [ "$d" = "200" ] || { echo "engines did not become ready (src=$s dst=$d)" >&2; exit 1; }
echo "engines ready"

echo "=== seed the source ==="
# RustFS can accept health probes before its storage quorum is ready. Do not
# make the seed depend on a stale alias from a previous run: retry the alias
# creation explicitly and fail the test if it never becomes usable.
set_alias() { # <alias> <endpoint> <ak> <sk>
  local i
  for i in $(seq 1 30); do
    mc alias set "$1" "$2" "$3" "$4" >/dev/null 2>&1 && return 0
    [ "$i" -lt 30 ] && sleep 1
  done
  echo "FAIL: could not set mc alias $1 after 30 attempts" >&2
  return 1
}
set_alias smsrc "$SRC" "$SAK" "$SSK" || exit 1
set_alias smdst "$DST" "$DAK" "$DSK" || exit 1
mc mb --ignore-existing "smsrc/$BKT" >/dev/null
printf 'hello world\n' > "$WORK/a.txt"
head -c 65536 /dev/urandom > "$WORK/b.bin"
python3 - "$WORK/c.txt" <<'PY'
import sys
open(sys.argv[1], "w").write("line\n" * 2000)
PY
mc cp "$WORK/a.txt" "smsrc/$BKT/agents/w1/SOUL.md" >/dev/null
mc cp "$WORK/b.bin" "smsrc/$BKT/agents/w1/workspace/blob.bin" >/dev/null
mc cp "$WORK/c.txt" "smsrc/$BKT/shared/doc.md" >/dev/null
mc cp "$WORK/a.txt" "smsrc/$BKT/teams/t1/shared/notes.md" >/dev/null
mc cp "$WORK/a.txt" "smsrc/$BKT/agentteams-config/packages/x.txt" >/dev/null
N_SRC=$(mc ls --recursive "smsrc/$BKT" | wc -l | tr -d ' ')

cat > "$WORK/policy.json" <<'P'
{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::agentteams-storage/shared/*"]}]}
P
mc admin user add smsrc worker-a passA12345 >/dev/null 2>&1
mc admin user add smsrc worker-b passB12345 >/dev/null 2>&1
mc admin user add smsrc worker-c passC12345 >/dev/null 2>&1
mc admin policy create smsrc worker-readonly "$WORK/policy.json" >/dev/null 2>&1
mc admin policy create smsrc worker-full "$WORK/policy.json" >/dev/null 2>&1
mc admin policy attach smsrc worker-readonly --user worker-a >/dev/null 2>&1
mc admin policy attach smsrc worker-full --user worker-b >/dev/null 2>&1
cat > "$WORK/secrets.json" <<'S'
{"worker-a":"passA12345","worker-b":"passB12345","worker-c":"passC12345"}
S
echo "seeded: $N_SRC objects, 3 users, 2 policies"

BASE=(--src-endpoint "$SRC" --src-ak "$SAK" --src-sk "$SSK"
      --dst-endpoint "$DST" --dst-ak "$DAK" --dst-sk "$DSK" --profile generic)

echo
echo "=== A) dry run: plan only, no mutation ==="
out=$(bash "$TOOL" "${BASE[@]}" --mode mirror --state-dir "$WORK/stateA" --dry-run 2>&1); rc=$?
[ "$rc" -eq 0 ] && pass "A1 dry-run exit 0" || fail "A1 dry-run exit 0 (rc=$rc): $(printf '%s' "$out" | tail -3 | tr '\n' ' ')"
[ -s "$WORK/stateA/dryrun-plan.json" ] && pass "A2 plan written" || fail "A2 plan written"
grep -q "preflight green" <<<"$out" && pass "A3 preflight green" || fail "A3 preflight green"
N_DST_A=$(mc ls --recursive "smdst/$BKT" 2>/dev/null | wc -l | tr -d ' ')
[ "${N_DST_A:-0}" -eq 0 ] && pass "A4 no target mutation" || fail "A4 no target mutation (dst=$N_DST_A)"
[ "$(stage_of "$WORK/stateA")" = "PREFLIGHT" ] && pass "A5 stage=PREFLIGHT" || fail "A5 stage=PREFLIGHT (got $(stage_of "$WORK/stateA"))"

echo
echo "=== B) full run without secrets: IAM red, data copied, state=DATA ==="
out=$(bash "$TOOL" "${BASE[@]}" --mode mirror --state-dir "$WORK/stateB" 2>&1); rc=$?
[ "$rc" -eq 3 ] && pass "B1 IAM red without secrets (exit 3)" || fail "B1 IAM red without secrets (rc=$rc): $(printf '%s' "$out" | tail -3 | tr '\n' ' ')"
[ "$(stage_of "$WORK/stateB")" = "DATA" ] && pass "B2 state=DATA" || fail "B2 state=DATA (got $(stage_of "$WORK/stateB"))"
N_DST_B=$(mc ls --recursive "smdst/$BKT" 2>/dev/null | wc -l | tr -d ' ')
[ "${N_DST_B:-0}" -eq "$N_SRC" ] && pass "B3 data copied ($N_DST_B=$N_SRC objects)" || fail "B3 data copied (dst=$N_DST_B src=$N_SRC)"

echo
echo "=== C) resume the IAM phase with the secrets file ==="
out=$(bash "$TOOL" "${BASE[@]}" --mode mirror --state-dir "$WORK/stateB" --phase iam --iam-secrets-file "$WORK/secrets.json" 2>&1); rc=$?
[ "$rc" -eq 0 ] && pass "C1 resume iam exit 0" || fail "C1 resume iam exit 0 (rc=$rc): $(printf '%s' "$out" | tail -3 | tr '\n' ' ')"
[ "$(stage_of "$WORK/stateB")" = "IAM" ] && pass "C2 state=IAM" || fail "C2 state=IAM (got $(stage_of "$WORK/stateB"))"
U_DST=$(mc admin user list smdst --json 2>/dev/null | jq -r '.accessKey // empty' | sort -u | wc -l | tr -d ' ')
[ "${U_DST:-0}" -ge 3 ] && pass "C3 target has >=3 users (got $U_DST)" || fail "C3 target has >=3 users (got $U_DST)"

echo
echo "=== D) verify (V1-V9) ==="
out=$(bash "$TOOL" "${BASE[@]}" --mode mirror --state-dir "$WORK/stateB" --phase verify 2>&1); rc=$?
[ "$rc" -eq 0 ] && pass "D1 verify exit 0" || fail "D1 verify exit 0 (rc=$rc): $(printf '%s' "$out" | grep -E 'V[0-9].*fail|verify red' | head -3 | tr '\n' ' ')"
[ "$(stage_of "$WORK/stateB")" = "VERIFIED" ] && pass "D2 state=VERIFIED" || fail "D2 state=VERIFIED (got $(stage_of "$WORK/stateB"))"
grep -q "verify green" <<<"$out" && pass "D3 verify green message" || fail "D3 verify green message"
# integration contract: the status document reflects the real stage and verdicts
st=$(bash "$TOOL" --status --state-dir "$WORK/stateB" 2>&1)
if jq -e '.schema == "storage-migrate.status/v1" and .stage == "VERIFIED" and ([.verify[]] | all(. == "pass"))' <<<"$st" >/dev/null 2>&1; then
  pass "D4 --status reports VERIFIED with all verify items pass"
else
  fail "D4 --status reports VERIFIED with all verify items pass: $(printf '%s' "$st" | head -2 | tr '\n' ' ')"
fi

echo
echo "=== E) cutover: final delta, write probes, checklist ==="
out=$(bash "$TOOL" "${BASE[@]}" --mode mirror --state-dir "$WORK/stateB" --phase cutover 2>&1); rc=$?
[ "$rc" -eq 0 ] && pass "E1 cutover exit 0" || fail "E1 cutover exit 0 (rc=$rc): $(printf '%s' "$out" | tail -3 | tr '\n' ' ')"
[ "$(stage_of "$WORK/stateB")" = "CUTOVER_PENDING" ] && pass "E2 state=CUTOVER_PENDING" || fail "E2 state=CUTOVER_PENDING (got $(stage_of "$WORK/stateB"))"
[ -s "$WORK/stateB/cutover-checklist.sh" ] && pass "E3 checklist generated" || fail "E3 checklist generated"
st2=$(bash "$TOOL" --status --state-dir "$WORK/stateB" 2>&1)
jq -e '.stage == "CUTOVER_PENDING" and (.artifacts.checklist | length > 0)' <<<"$st2" >/dev/null 2>&1 \
  && pass "E5 --status reports CUTOVER_PENDING" || fail "E5 --status reports CUTOVER_PENDING: $(printf '%s' "$st2" | head -2 | tr '\n' ' ')"
grep -q "RUSTFS_ON_DEMAND_MIGRATION_ENABLED" "$WORK/stateB/cutover-checklist.sh" 2>/dev/null \
  && pass "E4 checklist mentions the ODM toggle" || fail "E4 checklist mentions the ODM toggle"

echo
echo "=== F) human gate: mark-cutover-done ==="
out=$(bash "$TOOL" "${BASE[@]}" --mode mirror --state-dir "$WORK/stateB" --mark-cutover-done 2>&1); rc=$?
[ "$rc" -eq 0 ] && pass "F1 mark-cutover-done exit 0" || fail "F1 mark-cutover-done exit 0 (rc=$rc)"
[ "$(stage_of "$WORK/stateB")" = "CUTOVER_DONE" ] && pass "F2 state=CUTOVER_DONE" || fail "F2 state=CUTOVER_DONE (got $(stage_of "$WORK/stateB"))"
# F3 (#1413 SIGPIPE regression): a consumer that closes the pipe early
# (head -2) must not break the run. After its last log line the tool only
# writes files, so rc must be 0 - or, at minimum, the state must have
# advanced to CUTOVER_DONE.
mkdir -p "$WORK/stateF3"
jq -n '{stage:"CUTOVER_PENDING", src:{endpoint:"http://src.example:9000", buckets:["b"]}, dst:{endpoint:"http://dst.example:9090"}, stage_detail:{}}' > "$WORK/stateF3/state.json"
bash "$TOOL" "${BASE[@]}" --mode mirror --state-dir "$WORK/stateF3" --mark-cutover-done 2>&1 | head -2
rc3=${PIPESTATUS[0]}
if [ "$rc3" -eq 0 ]; then
  pass "F3 SIGPIPE regression: early-closing consumer, rc=0"
elif [ "$(stage_of "$WORK/stateF3")" = "CUTOVER_DONE" ]; then
  pass "F3 SIGPIPE regression: rc=$rc3 but state=CUTOVER_DONE"
else
  fail "F3 SIGPIPE regression: early-closing consumer (rc=$rc3, state=$(stage_of "$WORK/stateF3"))"
fi

echo
echo "=== G) negative gate ==="
out=$(bash "$TOOL" "${BASE[@]}" --mode mirror --state-dir "$WORK/stateN" --phase cutover 2>&1); rc=$?
[ "$rc" -eq 1 ] && pass "G1 cutover refused at INIT (exit 1)" || fail "G1 cutover refused at INIT (rc=$rc)"
grep -q "cutover requires state=VERIFIED" <<<"$out" && pass "G2 gate message" || fail "G2 gate message"

echo
echo "=== H) independent comparison (not trusting the tool) ==="
src_list=$(mc ls --recursive --json "smsrc/$BKT" | jq -r '.key + " " + (.size|tostring)' | sort)
dst_list=$(mc ls --recursive --json "smdst/$BKT" | jq -r '.key + " " + (.size|tostring)' | sort)
if [ "$src_list" = "$dst_list" ]; then pass "H1 key+size sets identical"; else
  fail "H1 key+size sets identical: $(diff <(echo "$src_list") <(echo "$dst_list") | head -4 | tr '\n' ' ')"
fi
MISMATCH=""
for k in $(echo "$src_list" | awk '{print $1}'); do
  a=$(mc cat "smsrc/$BKT/$k" | md5sum | cut -d' ' -f1)
  b=$(mc cat "smdst/$BKT/$k" | md5sum | cut -d' ' -f1)
  [ "$a" = "$b" ] || MISMATCH="$MISMATCH $k"
done
[ -z "$MISMATCH" ] && pass "H2 every object md5 matches" || fail "H2 md5 mismatches:$MISMATCH"
U_SRC=$(mc admin user list smsrc --json 2>/dev/null | jq -r '.accessKey // empty' | sort -u)
U_DST2=$(mc admin user list smdst --json 2>/dev/null | jq -r '.accessKey // empty' | sort -u)
MISSING=$(comm -23 <(echo "$U_SRC") <(echo "$U_DST2"))
[ -z "$MISSING" ] && pass "H3 every source user exists on target" || fail "H3 missing users:$MISSING"
P_SRC=$(mc admin policy list smsrc 2>/dev/null | sort)
P_DST=$(mc admin policy list smdst 2>/dev/null | sort)
PMISS=$(comm -23 <(echo "$P_SRC") <(echo "$P_DST"))
[ -z "$PMISS" ] && pass "H4 every source policy exists on target" || fail "H4 missing policies:$PMISS"

echo
if [ "$FAILS" -eq 0 ]; then echo "E2E RESULT: PASS (all assertions green)"; exit 0; fi
echo "E2E RESULT: FAIL ($FAILS assertion(s))"; exit 1
