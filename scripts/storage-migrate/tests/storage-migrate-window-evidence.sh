#!/usr/bin/env bash
# window_sample/window_evidence: quiet bucket passes, growth fails, and an
# equal-size prefix move fails (the digest must cover the prefix distribution).
set -uo pipefail
HERE="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT
BIN="$WORK/bin"; mkdir -p "$BIN"
cat > "$BIN/mc" <<'STUB'
#!/usr/bin/env bash
case "$*" in
  *"ls --recursive --json"*) cat "$MC_FIXTURE" ;;
  *) exit 0 ;;
esac
STUB
chmod +x "$BIN/mc"
PATH="$BIN:$PATH"
pass=0; fail=0
ok(){ pass=$((pass+1)); echo "PASS: $1"; }
no(){ fail=$((fail+1)); echo "FAIL: $1"; }

# shellcheck disable=SC1091
. "$HERE/lib/common.sh" 2>/dev/null
# shellcheck disable=SC1091
. "$HERE/lib/orchestration.sh" 2>/dev/null
SRC_ALIAS=src; get_buckets(){ echo bucket1; }

printf '%s\n' '{"key":"prefix-a/one.bin","size":8}' > "$WORK/quiet.json"
printf '%s\n' '{"key":"prefix-b/one.bin","size":8}' > "$WORK/moved.json"
printf '%s\n' '{"key":"prefix-a/one.bin","size":8}' '{"key":"prefix-a/two.bin","size":4}' > "$WORK/grown.json"

export MC_FIXTURE="$WORK/quiet.json"; s1=$(window_sample); r1=$?
export MC_FIXTURE="$WORK/quiet.json"; s2=$(window_sample)
[ "$r1" = 0 ] && [ "$s1" = "$s2" ] && ok "quiet bucket yields identical samples" || no "quiet bucket samples differ ($s1 vs $s2)"
[ "$(cut -f1 <<<"$s1")" = "1" ] && [ "$(cut -f2 <<<"$s1")" = "8" ] && ok "totals accumulate (count=1 bytes=8)" || no "totals wrong: $s1"

export MC_FIXTURE="$WORK/grown.json"; s3=$(window_sample)
[ "$s3" != "$s1" ] && ok "growth changes the sample" || no "growth not detected"

export MC_FIXTURE="$WORK/moved.json"; s4=$(window_sample)
[ "$s4" != "$s1" ] && ok "equal-size prefix move changes the digest" || no "prefix move not detected (same count/size accepted as frozen)"

# empty/garbage must fail, not silently pass
printf '%s\n' 'not json' > "$WORK/bad.json"
export MC_FIXTURE="$WORK/bad.json"; window_sample >/dev/null; [ $? -ne 0 ] && ok "invalid listing is rejected" || no "invalid listing accepted"

echo "WINDOW RESULT: $([ "$fail" -eq 0 ] && echo PASS || echo FAIL) ($pass passed, $fail failed)"
[ "$fail" -eq 0 ]
