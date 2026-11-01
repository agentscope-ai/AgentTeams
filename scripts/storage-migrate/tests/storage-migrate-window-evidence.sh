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
  *"ls --recursive --json"*bucket2*) cat "${MC_FIXTURE2:-$MC_FIXTURE}" ;;
  *"ls --recursive --json"*)         cat "$MC_FIXTURE" ;;
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



# multi-bucket: a prefix move confined to the SECOND bucket must be caught
printf '%s\n' '{"key":"prefix-a/one.bin","size":8}' > "$WORK/b1.json"
printf '%s\n' '{"key":"prefix-a/two.bin","size":8}' > "$WORK/b2_quiet.json"
printf '%s\n' '{"key":"prefix-b/two.bin","size":8}' > "$WORK/b2_moved.json"
get_buckets(){ echo bucket1 bucket2; }
export MC_FIXTURE="$WORK/b1.json" MC_FIXTURE2="$WORK/b2_quiet.json"; m1=$(window_sample)
[ "$(cut -f1 <<<"$m1")" = "2" ] && [ "$(cut -f2 <<<"$m1")" = "16" ] && ok "multi-bucket totals are 2/16" || no "multi-bucket totals wrong: $m1"
export MC_FIXTURE2="$WORK/b2_quiet.json"; m2=$(window_sample)
[ "$m1" = "$m2" ] && ok "multi-bucket quiet window is stable" || no "multi-bucket quiet window differs"
export MC_FIXTURE2="$WORK/b2_moved.json"; m3=$(window_sample)
[ "$m3" != "$m1" ] && ok "prefix move in the second bucket is detected" || no "second-bucket prefix move missed (multi-bucket false freeze)"
get_buckets(){ echo bucket1; }

# empty bucket: success with zero objects must be a valid, deterministic sample
: > "$WORK/empty.json"
export MC_FIXTURE="$WORK/empty.json"; s5=$(window_sample); r5=$?
[ "$r5" = 0 ] && [ "$(cut -f1 <<<"$s5")" = "0" ] && [ "$(cut -f2 <<<"$s5")" = "0" ] && ok "empty bucket reports 0/0 instead of failing" || no "empty bucket rejected or wrong totals ($s5)"
s6=$(window_sample)
[ "$s5" = "$s6" ] && ok "empty bucket digest is deterministic" || no "empty bucket digest not deterministic"

# empty/garbage must fail, not silently pass
printf '%s\n' 'not json' > "$WORK/bad.json"
export MC_FIXTURE="$WORK/bad.json"; window_sample >/dev/null; [ $? -ne 0 ] && ok "invalid listing is rejected" || no "invalid listing accepted"

echo "WINDOW RESULT: $([ "$fail" -eq 0 ] && echo PASS || echo FAIL) ($pass passed, $fail failed)"
[ "$fail" -eq 0 ]
