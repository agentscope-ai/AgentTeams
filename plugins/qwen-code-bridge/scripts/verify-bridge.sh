#!/bin/sh
# verify-bridge.sh — the per-release gate for the QwenPaw × QwenCode bridge.
#
# Runs: (1) the plugin test suite on the installed framework, (2) the same
# suite on the #7565 tree when QWEN_CODE_BRIDGE_7565_SRC is set, (3) live
# checks against the running QwenPaw app (plugin loaded, native web shell
# served, daemon sessions reachable through the proxy). Writes an evidence
# JSON and exits non-zero on any failure.
#
# The evidence JSON's top-level "pass" covers BOTH the test runs and the
# live checks: every pytest invocation's real exit code is captured
# separately (output to a temp file, rc read before anything else — no
# pipes, so tail's always-zero exit code can never mask a failed suite),
# and the JSON carries per-suite {"summary", "exit_code", "pass"} objects.
#
# Env:
#   PY                        python (default /app/venv/bin/python)
#   QWENPAW_APP_URL|QWENPAW_PORT   app base (default http://127.0.0.1:$QWENPAW_PORT)
#   AGENT                     X-Agent-Id for the plugin API (default AgentTeamGuide)
#   QWEN_CODE_BRIDGE_7565_SRC optional path to the #7565 source tree
#   TESTS_DIR                 pytest target dir (default: <plugin>/tests);
#                             testability override used by the gate's own
#                             regression tests (tests/test_verify_bridge_gate.py)
#   LIVE_CHECKS               "skip" skips section 3 entirely (testability
#                             override: live checks need a running app);
#                             the evidence JSON then records live.skipped=true
set -eu

HERE=$(cd "$(dirname "$0")/.." && pwd)
PY=${PY:-/app/venv/bin/python}
APP=${QWENPAW_APP_URL:-http://127.0.0.1:${QWENPAW_PORT:-8088}}
AGENT=${AGENT:-AgentTeamGuide}
STATE=${QWEN_CODE_BRIDGE_STATE_DIR:-/app/working/.qwenpaw/qwen-code-bridge}
TESTS_DIR=${TESTS_DIR:-$HERE/tests}
LIVE_CHECKS=${LIVE_CHECKS:-run}
STAMP=$(date +%Y%m%d-%H%M%S)
OUT=${1:-$STATE/evidence/bridge-$STAMP.json}
mkdir -p "$(dirname "$OUT")"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fail=0
say() { printf '%s\n' "$*"; }

say "== 1/3 tests: installed framework"
SPIKE="$HERE/../../spike/test_t0_spike.py"
[ -f "$SPIKE" ] || SPIKE=""

# Real pytest exit code: output goes to a file first, rc is read
# immediately after (no pipe — a piped command's status is the last
# stage's, i.e. tail's always-0, which masked collection errors).
# `rc=0; cmd || rc=$?` keeps set -e from aborting before the evidence
# JSON is written.
rc_installed=0
"$PY" -m pytest "$TESTS_DIR" $SPIKE -q > "$TMP/installed.log" 2>&1 || rc_installed=$?
OLD=$(tail -1 "$TMP/installed.log")
say "   $OLD (exit $rc_installed)"
[ "$rc_installed" -eq 0 ] || fail=1

rc_new=""
NEW="skipped (QWEN_CODE_BRIDGE_7565_SRC unset)"
if [ -n "${QWEN_CODE_BRIDGE_7565_SRC:-}" ]; then
  say "== 2/3 tests: #7565 tree"
  rc_new=0
  PYTHONPATH="$QWEN_CODE_BRIDGE_7565_SRC" "$PY" -m pytest "$TESTS_DIR" $SPIKE -q \
    > "$TMP/new.log" 2>&1 || rc_new=$?
  NEW=$(tail -1 "$TMP/new.log")
  say "   $NEW (exit $rc_new)"
  [ "$rc_new" -eq 0 ] || fail=1
else
  say "== 2/3 tests: #7565 tree skipped"
fi

LIVE_SKIPPED=no
LOADED="no"
WEB="0"
BASE="0"
SESS="0"
if [ "$LIVE_CHECKS" = "skip" ]; then
  LIVE_SKIPPED=yes
  say "== 3/3 live checks skipped (LIVE_CHECKS=skip)"
else
  say "== 3/3 live checks ($APP)"
  LOADED=$(curl -s --max-time 10 "$APP/api/plugins" -H "X-Agent-Id: $AGENT" \
    | "$PY" -c "import json,sys
try: items=json.load(sys.stdin)
except Exception: print('no'); raise SystemExit
items=items if isinstance(items,list) else (items.get('plugins') or [])
print('yes' if any(p.get('id')=='qwen-code-bridge' and p.get('loaded') for p in items) else 'no')")
  [ "$LOADED" = "yes" ] || fail=1
  say "   plugin loaded: $LOADED"

  WEB=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$APP/api/qwen-code-bridge/web/")
  [ "$WEB" = "200" ] || fail=1
  say "   native web shell: HTTP $WEB"

  BASE=$(curl -s --max-time 10 "$APP/api/qwen-code-bridge/web/" | grep -Ec '__QWEN_DAEMON_BASE__|var B="/api/qwen-code-bridge/web";' || true)
  [ "${BASE:-0}" -ge 1 ] || fail=1
  say "   daemon routing injection: $BASE"

  SESS=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
    "$APP/api/qwen-code-bridge/web/workspaces//tmp/sessions")
  [ "$SESS" = "200" ] || fail=1
  say "   sessions proxy: HTTP $SESS"
fi

"$PY" - "$OUT" "$rc_installed" "$OLD" "$rc_new" "$NEW" "$LIVE_SKIPPED" \
  "$LOADED" "$WEB" "$BASE" "$SESS" <<'PY'
import json, sys, time
(out, rc_old, old, rc_new, new, live_skipped, loaded, web, base, sess) = sys.argv[1:11]
old_obj = {"summary": old, "exit_code": int(rc_old), "pass": int(rc_old) == 0}
if rc_new == "":
    # Not executed (7565 tree unset): keep the skip as a self-describing
    # object, distinct from an executed suite's {summary, exit_code, pass}.
    new_obj = {"skipped": True, "reason": new}
else:
    new_obj = {"summary": new, "exit_code": int(rc_new), "pass": int(rc_new) == 0}
if live_skipped == "yes":
    live = {"skipped": True}
    live_ok = True
else:
    live = {
        "plugin_loaded": loaded == "yes",
        "web_shell_http": int(web or 0),
        "daemon_base_injection": int(base or 0),
        "sessions_proxy_http": int(sess or 0),
    }
    live_ok = (
        loaded == "yes"
        and web == "200"
        and int(base or 0) >= 1
        and sess == "200"
    )
tests_ok = int(rc_old) == 0 and (rc_new == "" or int(rc_new) == 0)
json.dump(
    {
        "generated_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "tests_installed": old_obj,
        "tests_7565": new_obj,
        "live": live,
        "pass": bool(tests_ok and live_ok),
    },
    open(out, "w", encoding="utf-8"),
    ensure_ascii=False,
    indent=2,
)
print("evidence:", out)
PY

say "== RESULT: $([ "$fail" -eq 0 ] && echo PASS || echo FAIL)"
exit "$fail"
