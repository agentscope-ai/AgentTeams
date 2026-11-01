#!/usr/bin/env bash
set -euo pipefail

# TeamHarness OpenClaw adapter — uninstall.
#
# Removes exactly the keys install.sh wrote to openclaw.json:
#   mcp.servers.teamharness, the agentteams-teamharness id in plugins.allow,
#   this adapter directory in plugins.load.paths, and
#   plugins.entries.agentteams-teamharness.
# Every other key is preserved. Idempotent: a second run is a no-op that
# leaves the file byte-identical. --dry-run prints the resulting
# configuration on stdout and changes nothing.
#
# The effective config target is resolved with the same rule as install.sh:
# --config > $OPENCLAW_CONFIG_PATH (the Worker export) > $OPENCLAW_CONFIG
# (legacy) > $HOME/.openclaw/openclaw.json, and a symlink is read and
# written through to its canonical target — the link itself is preserved,
# never atomically replaced by a regular file.

usage() {
  cat <<'EOF'
Usage: uninstall.sh [--config <path>] [--dry-run]

  --config <path>  OpenClaw config file (default: $OPENCLAW_CONFIG_PATH,
                   then $OPENCLAW_CONFIG, then $HOME/.openclaw/openclaw.json).
                   A symlink is honoured: its canonical target is read and
                   written, the link itself is preserved.
  --dry-run        Print the resulting configuration on stdout; change nothing
EOF
}

CONFIG=""
DRY_RUN=0
while [ $# -gt 0 ]; do
  case "$1" in
    --config) CONFIG="${2:?--config requires a value}"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "ERROR: unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ -n "$CONFIG" ] || \
  CONFIG="${OPENCLAW_CONFIG_PATH:-${OPENCLAW_CONFIG:-$HOME/.openclaw/openclaw.json}}"

export TH_OC_ADAPTER_DIR="$SCRIPT_DIR"
export TH_OC_CONFIG="$CONFIG"
export TH_OC_DRY_RUN="$DRY_RUN"

python3 - <<'PYEOF'
import json
import os
import shutil
import stat
import sys
import tempfile
from pathlib import Path

adapter_dir = Path(os.environ["TH_OC_ADAPTER_DIR"]).resolve()
config_path = Path(os.environ["TH_OC_CONFIG"]).expanduser()
# Effective config target: same rule as install.sh — a symlink (the Worker
# links ~/.openclaw/openclaw.json to its canonical $HOME/openclaw.json) is
# read and written through to its canonical target, so the link survives
# and the staged root is found next to the canonical config. strict=False:
# a dangling link resolves to its intended target.
config_path = config_path.resolve(strict=False)
dry_run = os.environ.get("TH_OC_DRY_RUN") == "1"

PLUGIN_ID = "agentteams-teamharness"
MCP_SERVER_ID = "teamharness"


def dump_json(obj):
    return json.dumps(obj, indent=2, sort_keys=True) + "\n"


def load_json(path):
    if not path.is_file():
        return {}
    text = path.read_text(encoding="utf-8")
    if not text.strip():
        return {}
    return json.loads(text)


CREDENTIAL_SUFFIXES = ("token", "secret", "password", "passwd", "apikey", "credentials")
REDACTED = "<redacted>"


def _is_credential_key(key):
    normalized = "".join(ch for ch in str(key).lower() if ch.isalnum())
    return normalized.endswith(CREDENTIAL_SUFFIXES)


def redact(document):
    """Deep copy with credential values replaced by REDACTED (previews only)."""
    if isinstance(document, dict):
        out = {}
        for key, value in document.items():
            if _is_credential_key(key) and isinstance(value, str) and value:
                out[key] = REDACTED
            else:
                out[key] = redact(value)
        return out
    if isinstance(document, list):
        return [redact(item) for item in document]
    return document


def write_atomic(path, text):
    path.parent.mkdir(parents=True, exist_ok=True)
    mode = stat.S_IMODE(path.stat().st_mode) if path.exists() else 0o600
    fd, tmp = tempfile.mkstemp(dir=str(path.parent), prefix=path.name + ".", suffix=".tmp")
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            fh.write(text)
        os.chmod(tmp, mode)
        os.replace(tmp, path)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


config = load_json(config_path)
removed = []
staged_plugin_dir = config_path.parent / "plugins" / PLUGIN_ID

mcp = config.get("mcp")
if isinstance(mcp, dict):
    servers = mcp.get("servers")
    if isinstance(servers, dict) and MCP_SERVER_ID in servers:
        del servers[MCP_SERVER_ID]
        removed.append("mcp.servers.%s" % MCP_SERVER_ID)
        if not servers:
            del mcp["servers"]
    if isinstance(mcp, dict) and not mcp:
        del config["mcp"]

plugins = config.get("plugins")
if isinstance(plugins, dict):
    allow = plugins.get("allow")
    if isinstance(allow, list) and PLUGIN_ID in allow:
        allow.remove(PLUGIN_ID)
        removed.append("plugins.allow[agentteams-teamharness]")
    load = plugins.get("load")
    if isinstance(load, dict):
        paths = load.get("paths")
        if isinstance(paths, list):
            # The staged plugin root is what install registers; the bare
            # adapter directory is only cleaned up for older installs.
            for stale in (str(staged_plugin_dir), str(adapter_dir)):
                if stale in paths:
                    paths.remove(stale)
                    removed.append("plugins.load.paths[%s]" % stale)
    entries = plugins.get("entries")
    if isinstance(entries, dict) and PLUGIN_ID in entries:
        del entries[PLUGIN_ID]
        removed.append("plugins.entries.%s" % PLUGIN_ID)

result = dump_json(config)

if dry_run:
    sys.stdout.write(dump_json(redact(config)))
    sys.stderr.write("openclaw adapter: dry-run, no files changed\n")
else:
    if staged_plugin_dir.is_dir():
        shutil.rmtree(staged_plugin_dir)
        removed.append("staged plugin root %s" % staged_plugin_dir)
    if not config_path.is_file():
        sys.stderr.write("openclaw adapter: %s absent, nothing to uninstall\n" % config_path)
    elif config_path.read_text(encoding="utf-8") == result:
        sys.stderr.write("openclaw adapter: nothing to remove, %s untouched\n" % config_path)
    else:
        write_atomic(config_path, result)
        sys.stderr.write(
            "openclaw adapter: removed %s from %s\n"
            % (", ".join(removed) if removed else "nothing", config_path)
        )
PYEOF
