#!/bin/sh
# verify-bridge.sh — the per-release gate for the QwenPaw × QwenCode bridge.
#
# Runs: (1) the plugin test suite on the installed framework, (2) the same
# suite on the #7565 tree when QWEN_CODE_BRIDGE_7565_SRC is set, (3) live
# checks against the running QwenPaw app (plugin loaded, native web shell
# served, daemon sessions reachable through the proxy). Writes an evidence
# JSON and exits non-zero on any failure.
#
# Env:
#   PY                        python (default /app/venv/bin/python)
#   QWENPAW_APP_URL|QWENPAW_PORT   app base (default http://127.0.0.1:$QWENPAW_PORT)
#   AGENT                     X-Agent-Id for the plugin API (default AgentTeamGuide)
#   QWEN_CODE_BRIDGE_7565_SRC optional path to the #7565 source tree
set -eu

HERE=$(cd "$(dirname "$0")/.." && pwd)
PY=${PY:-/app/venv/bin/python}
APP=${QWENPAW_APP_URL:-http://127.0.0.1:${QWENPAW_PORT:-8088}}
AGENT=${AGENT:-AgentTeamGuide}
STATE=${QWEN_CODE_BRIDGE_STATE_DIR:-/app/working/.qwenpaw/qwen-code-bridge}
STAMP=$(date +%Y%m%d-%H%M%S)
OUT=${1:-$STATE/evidence/bridge-$STAMP.json}
mkdir -p "$(dirname "$OUT")"

fail=0
say() { printf '%s\n' "$*"; }

say "== 1/3 tests: installed framework"
SPIKE="$HERE/../../spike/test_t0_spike.py"
[ -f "$SPIKE" ] || SPIKE=""
OLD=$("$PY" -m pytest "$HERE/tests" $SPIKE -q 2>&1 | tail -1)
say "   $OLD"
echo "$OLD" | grep -q "failed" && fail=1

NEW="skipped (QWEN_CODE_BRIDGE_7565_SRC unset)"
if [ -n "${QWEN_CODE_BRIDGE_7565_SRC:-}" ]; then
  say "== 2/3 tests: #7565 tree"
  NEW=$(PYTHONPATH="$QWEN_CODE_BRIDGE_7565_SRC" "$PY" -m pytest "$HERE/tests" $SPIKE -q 2>&1 | tail -1)
  say "   $NEW"
  echo "$NEW" | grep -q "failed" && fail=1
else
  say "== 2/3 tests: #7565 tree skipped"
fi

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

BASE=$(curl -s --max-time 10 "$APP/api/qwen-code-bridge/web/" | grep -c '__QWEN_DAEMON_BASE__' || true)
[ "${BASE:-0}" -ge 1 ] || fail=1
say "   daemon-base injection: $BASE"

SESS=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
  "$APP/api/qwen-code-bridge/web/workspaces//tmp/sessions")
[ "$SESS" = "200" ] || fail=1
say "   sessions proxy: HTTP $SESS"

"$PY" - "$OUT" "$OLD" "$NEW" "$LOADED" "$WEB" "$BASE" "$SESS" <<'PY'
import json, sys, time
out, old, new, loaded, web, base, sess = sys.argv[1:8]
json.dump(
    {
        "generated_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "tests_installed": old,
        "tests_7565": new,
        "live": {
            "plugin_loaded": loaded == "yes",
            "web_shell_http": int(web or 0),
            "daemon_base_injection": int(base or 0),
            "sessions_proxy_http": int(sess or 0),
        },
        "pass": loaded == "yes" and web == "200" and int(base or 0) >= 1 and sess == "200",
    },
    open(out, "w", encoding="utf-8"),
    ensure_ascii=False,
    indent=2,
)
print("evidence:", out)
PY

say "== RESULT: $([ "$fail" -eq 0 ] && echo PASS || echo FAIL)"
exit "$fail"
