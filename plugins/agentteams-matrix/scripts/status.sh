#!/usr/bin/env bash
set -euo pipefail

# Read-only status for the TeamHarness bundle.
#
# Usage: status.sh [--runtime auto|qwenpaw|openclaw|hermes]... [--json] [--channel NAME]
#
# Reports, per runtime: installed (yes/no), version, plugin id, marker path,
# file integrity (marker vs disk, when the marker records a file map), and the
# requested channel. Never writes anything.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUNDLE_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
BUNDLE_ID="agentteams-matrix"
PLUGIN_ID="agentteams-matrix-channel"
SUPPORTED_RUNTIMES=(qwenpaw openclaw hermes)
JSON=0
CHANNEL="${TEAMHARNESS_CHANNEL:-unknown}"
SELECTIONS=()

usage() {
  cat <<'EOF'
Usage: status.sh [--runtime auto|qwenpaw|openclaw|hermes]... [--json] [--channel NAME]

Prints the installed version, plugin id, marker path and file integrity for
each selected runtime. Read-only.
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --runtime)
      value="${2:?--runtime requires a value}"
      case "$value" in
        auto|qwenpaw|openclaw|hermes) ;;
        *) echo "ERROR: unknown runtime: $value" >&2; exit 2 ;;
      esac
      SELECTIONS+=("$value"); shift 2 ;;
    --json) JSON=1; shift ;;
    --channel) CHANNEL="${2:?--channel requires a value}"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "ERROR: unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

detected() {
  case "$1" in
    qwenpaw) command -v qwenpaw >/dev/null 2>&1 ;;
    openclaw) command -v openclaw >/dev/null 2>&1 || [ -n "${OPENCLAW_CONFIG_PATH:-}" ] || [ -n "${OPENCLAW_CONFIG:-}" ] || [ -f "$HOME/.openclaw/openclaw.json" ] ;;
    hermes) command -v hermes >/dev/null 2>&1 || [ -n "${HERMES_HOME:-}" ] || [ -f "$HOME/.hermes/config.yaml" ] ;;
    *) return 1 ;;
  esac
}

runtimes=()
if [ "${#SELECTIONS[@]}" -eq 0 ]; then
  for r in "${SUPPORTED_RUNTIMES[@]}"; do detected "$r" && runtimes+=("$r"); done
  [ "${#runtimes[@]}" -eq 0 ] && runtimes=(qwenpaw)
else
  for r in "${SELECTIONS[@]}"; do
    if [ "$r" = auto ]; then
      for x in "${SUPPORTED_RUNTIMES[@]}"; do detected "$x" && runtimes+=("$x"); done
    else
      runtimes+=("$r")
    fi
  done
fi

qwenpaw_status() {
  local workdir="${QWENPAW_WORKING_DIR:-$HOME/.qwenpaw}"
  local marker="$workdir/.agentteams/$BUNDLE_ID/installed.json"
  local plugin_dir="$workdir/plugins/$PLUGIN_ID"
  python3 - "$marker" "$plugin_dir" "$PLUGIN_ID" "$CHANNEL" <<'PY'
import hashlib, json, sys
from pathlib import Path
marker_path, plugin_dir, plugin_id, channel = Path(sys.argv[1]), Path(sys.argv[2]), sys.argv[3], sys.argv[4]
out = {"runtime": "qwenpaw", "installed": False, "version": "", "pluginId": plugin_id,
       "marker": str(marker_path), "markerPresent": marker_path.is_file(),
       "filesOk": None, "filesTotal": 0, "filesMissing": 0, "channel": channel}
if (plugin_dir / "plugin.json").is_file():
    try:
        out["version"] = json.loads((plugin_dir / "plugin.json").read_text()).get("version", "")
    except ValueError:
        pass
if marker_path.is_file():
    try:
        marker = json.loads(marker_path.read_text())
    except ValueError:
        marker = None
    if isinstance(marker, dict):
        files = marker.get("files")
        if isinstance(files, dict):
            out["filesTotal"] = len(files)
            missing = 0
            for rel, want in files.items():
                p = plugin_dir / rel
                if not p.is_file():
                    missing += 1
                    continue
                got = hashlib.sha256(p.read_bytes()).hexdigest()
                if got != want:
                    missing += 1
            out["filesMissing"] = missing
            out["filesOk"] = missing == 0
        out["installed"] = True
        if not out["version"]:
            out["version"] = str(marker.get("version") or "")
print(json.dumps(out, ensure_ascii=False))
PY
}

openclaw_status() {
  local cfg="${OPENCLAW_CONFIG_PATH:-${OPENCLAW_CONFIG:-$HOME/.openclaw/openclaw.json}}"
  python3 - "$cfg" "$PLUGIN_ID" "$CHANNEL" <<'PY'
import json, sys
from pathlib import Path
cfg, plugin_id, channel = Path(sys.argv[1]), sys.argv[2], sys.argv[3]
out = {"runtime": "openclaw", "installed": False, "version": "", "pluginId": plugin_id,
       "marker": str(cfg), "markerPresent": cfg.is_file(), "filesOk": None,
       "filesTotal": 0, "filesMissing": 0, "channel": channel}
if cfg.is_file():
    try:
        data = json.loads(cfg.read_text())
    except ValueError:
        data = {}
    if isinstance(data, dict):
        out["installed"] = bool(data.get("plugins", {}).get(plugin_id) or data.get("mcp", {}).get("servers", {}).get(plugin_id))
print(json.dumps(out, ensure_ascii=False))
PY
}

hermes_status() {
  local cfg="${HERMES_HOME:-$HOME/.hermes}/config.yaml"
  python3 - "$cfg" "$PLUGIN_ID" "$CHANNEL" <<'PY'
import json
import sys
from pathlib import Path
cfg, plugin_id, channel = Path(sys.argv[1]), sys.argv[2], sys.argv[3]
text = cfg.read_text(errors="ignore") if cfg.is_file() else ""
out = {"runtime": "hermes", "installed": plugin_id in text, "version": "", "pluginId": plugin_id,
       "marker": str(cfg), "markerPresent": cfg.is_file(), "filesOk": None,
       "filesTotal": 0, "filesMissing": 0, "channel": channel}
print(json.dumps(out, ensure_ascii=False))
PY
}

for r in "${runtimes[@]}"; do
  case "$r" in
    qwenpaw) line="$(qwenpaw_status)" ;;
    openclaw) line="$(openclaw_status)" ;;
    hermes) line="$(hermes_status)" ;;
  esac
  if [ "$JSON" -eq 1 ]; then
    printf '%s\n' "$line"
  else
    printf '%s\n' "$line" | python3 -c 'import json,sys;d=json.load(sys.stdin);print("runtime=%s installed=%s version=%s pluginId=%s marker=%s files=%s/%s channel=%s" % (d["runtime"], "yes" if d["installed"] else "no", d["version"] or "-", d["pluginId"], d["marker"], (d["filesTotal"]-d["filesMissing"]) if d["filesOk"] is not None else "n/a", d["filesTotal"] if d["filesOk"] is not None else "n/a", d["channel"]))'
  fi
done
