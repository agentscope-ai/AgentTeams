#!/usr/bin/env bash
# Dry-run test for storage-migrate.sh against a fake S3 stack (no Docker, no
# network). Proves that:
#   * preflight completes and the plan is written,
#   * the dry run performs no mutating data operation (mirror/cp/rm),
#   * a failing preflight item aborts with exit 2.
#
# The fake `mc` and `curl` record every invocation to $CALLS.
#
# Usage: bash tests/storage-migrate-dryrun-fake.sh
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TOOL="$HERE/../storage-migrate.sh"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/sm-dryrun.XXXXXX")"
export CALLS="$WORK/calls.log"
: > "$CALLS"
FAILS=0
pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*"; FAILS=$((FAILS + 1)); }

mkdir -p "$WORK/bin"

# ---- fake mc -------------------------------------------------------------
cat > "$WORK/bin/mc" <<'STUB'
#!/usr/bin/env bash
echo "mc $*" >> "${CALLS:?}"
args="$*"
case "$args" in
  --version) echo "mc version RELEASE.2025-08-13T08-35-41Z (commit-id=7394ce0d)"; exit 0 ;;
  "alias set "*) exit 0 ;;
  "admin info "*) exit 0 ;;
  "admin user list --json"*) printf '{"status":"success","accessKey":"worker-a","userStatus":"enabled","policyName":"worker-a"}\n'; exit 0 ;;
  "admin user list"*) echo "worker-a enabled worker-a"; exit 0 ;;
  "admin policy list --json"*) printf '{"status":"success","policyName":"readwrite"}\n'; exit 0 ;;
  "admin policy list"*) echo "readwrite"; exit 0 ;;
  "admin policy info"*) printf '{"PolicyName":"readwrite","Policy":{"Version":"2012-10-17","Statement":[]}}\n'; exit 0 ;;
  "ls --incomplete"*) exit 0 ;;
  "ls --recursive --json "*) printf '{"key":"agents/w1/a.txt","size":12,"lastModified":"2026-01-01T00:00:00Z"}\n'; exit 0 ;;
  "ls --recursive "*)  echo "12B agents/w1/a.txt"; exit 0 ;;
  "ls "*)              echo "agentteams-storage/"; exit 0 ;;
  "du "*)              echo "12B agentteams-storage"; exit 0 ;;
esac
exit 0
STUB
chmod +x "$WORK/bin/mc"

# ---- fake curl (health probes + version/feature detection) --------------
cat > "$WORK/bin/curl" <<'STUB'
#!/usr/bin/env bash
echo "curl $*" >> "${CALLS:?}"
case "$*" in
  *"--help all"*) echo "     --aws-sigv4 <provider1[:provider2[:region[:service]]]>"; exit 0 ;;
  *"--version"*)  echo "curl 8.14.1 (x86_64-pc-linux-gnu)"; exit 0 ;;
  *"__HTTP_CODE__"*) printf '\n__HTTP_CODE__:404'; exit 0 ;;   # plain S3 (no RustFS admin route)
  *"%{http_code}"*) printf '200'; exit 0 ;;
esac
exit 0
STUB
chmod +x "$WORK/bin/curl"
export PATH="$WORK/bin:$PATH"

BASE=(--src-endpoint http://src.example:9000 --src-ak a --src-sk b
      --dst-endpoint http://dst.example:9090 --dst-ak c --dst-sk d
      --state-dir "$WORK/state")

echo "=== storage-migrate dry-run (fake S3 stack) ==="

# 1) dry run completes and writes the plan
out=$(bash "$TOOL" "${BASE[@]}" --mode mirror --profile generic --dry-run 2>&1); rc=$?
if [ "$rc" -eq 0 ]; then pass "dry run exits 0"; else fail "dry run exits 0 (got $rc): $(printf '%s' "$out" | tail -3 | tr '\n' ' ')"; fi
if [ -s "$WORK/state/dryrun-plan.json" ]; then pass "dry-run plan written"; else fail "dry-run plan written"; fi
if jq -e '.run' "$WORK/state/dryrun-plan.json" >/dev/null 2>&1; then pass "plan is valid JSON"; else fail "plan is valid JSON"; fi
if grep -q "preflight green" <<<"$out"; then pass "preflight reached green"; else fail "preflight reached green: $(printf '%s' "$out" | grep -E 'FAIL|RED' | head -2 | tr '\n' ' ')"; fi

# 2) dry run must not mutate data
if grep -qE '^mc (mirror|cp|mb|rm) ' "$CALLS"; then
  fail "dry run performed no data mutation: $(grep -E '^mc (mirror|cp|mb|rm) ' "$CALLS" | head -3 | tr '\n' ' ')"
else
  pass "dry run performed no data mutation"
fi

# 3) a missing source credential aborts with the usage exit code
out2=$(bash "$TOOL" --dst-endpoint http://dst.example:9090 --dst-ak c --dst-sk d --state-dir "$WORK/state2" 2>&1); rc2=$?
if [ "$rc2" -eq 4 ]; then pass "missing credentials exits 4"; else fail "missing credentials exits 4 (got $rc2)"; fi

# 4) preflight red aborts with exit 2: an unreachable target
cat > "$WORK/bin/curl" <<'STUB'
#!/usr/bin/env bash
case "$*" in
  *"--help all"*) echo "     --aws-sigv4 x"; exit 0 ;;
  *"--version"*)  echo "curl 8.14.1"; exit 0 ;;
  *"%{http_code}"*) printf '000'; exit 0 ;;   # target unreachable
esac
exit 0
STUB
chmod +x "$WORK/bin/curl"
out3=$(bash "$TOOL" --src-endpoint http://src.example:9000 --src-ak a --src-sk b \
        --dst-endpoint http://unreachable.example:9090 --dst-ak c --dst-sk d \
        --state-dir "$WORK/state3" --mode mirror --profile generic --dry-run 2>&1); rc3=$?
if [ "$rc3" -eq 2 ]; then pass "unreachable target aborts preflight with exit 2"; else fail "unreachable target aborts preflight with exit 2 (got $rc3): $(printf '%s' "$out3" | tail -2 | tr '\n' ' ')"; fi

echo
if [ "$FAILS" -eq 0 ]; then echo "DRYRUN RESULT: PASS"; exit 0; fi
echo "DRYRUN RESULT: FAIL ($FAILS assertion(s) failed)"; exit 1
