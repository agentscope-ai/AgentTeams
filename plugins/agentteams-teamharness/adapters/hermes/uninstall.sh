#!/usr/bin/env bash
set -euo pipefail

# TeamHarness Hermes adapter — uninstall.
#
# Removes exactly what install.sh wrote:
#   1. mcp_servers.teamharness from ~/.hermes/config.yaml (every other key
#      preserved), and
#   2. the plugin directory ~/.hermes/plugins/agentteams-teamharness/ — but
#      only when the plugin.yaml inside it identifies as
#      name: agentteams-teamharness, so a directory that belongs to something
#      else is never touched.
# Idempotent: a second run is a no-op that leaves the config byte-identical.
# --dry-run prints the resulting config.yaml on stdout and changes nothing.
# Requires python3 with PyYAML (hermes-worker itself declares pyyaml>=6.0).

usage() {
  cat <<'EOF'
Usage: uninstall.sh [--config <path>] [--plugin-home <path>] [--dry-run]

  --config <path>      Hermes config file (default: $HERMES_HOME/config.yaml
                       or ~/.hermes/config.yaml)
  --plugin-home <path> Plugin directory (default: $HERMES_HOME/plugins/
                       agentteams-teamharness or ~/.hermes/plugins/
                       agentteams-teamharness)
  --dry-run            Print the resulting config.yaml on stdout; change nothing
EOF
}

CONFIG=""
PLUGIN_HOME=""
DRY_RUN=0
while [ $# -gt 0 ]; do
  case "$1" in
    --config) CONFIG="${2:?--config requires a value}"; shift 2 ;;
    --plugin-home) PLUGIN_HOME="${2:?--plugin-home requires a value}"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "ERROR: unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HERMES_HOME="${HERMES_HOME:-$HOME/.hermes}"
[ -n "$CONFIG" ] || CONFIG="${HERMES_HOME}/config.yaml"
[ -n "$PLUGIN_HOME" ] || PLUGIN_HOME="${HERMES_HOME}/plugins/agentteams-teamharness"

export TH_HM_CONFIG="$CONFIG"
export TH_HM_PLUGIN_HOME="$PLUGIN_HOME"
export TH_HM_DRY_RUN="$DRY_RUN"

python3 - <<'PYEOF'
import os
import shutil
import stat
import sys
import tempfile
from pathlib import Path

import yaml

config_path = Path(os.environ["TH_HM_CONFIG"]).expanduser()
plugin_home = Path(os.environ["TH_HM_PLUGIN_HOME"]).expanduser()
dry_run = os.environ.get("TH_HM_DRY_RUN") == "1"

MCP_SERVER_ID = "teamharness"
PLUGIN_NAME = "agentteams-teamharness"


def dump_yaml(obj):
    return yaml.safe_dump(obj, sort_keys=True, default_flow_style=False,
                          allow_unicode=True, width=120)


def load_yaml(path):
    if not path.is_file():
        return {}
    text = path.read_text(encoding="utf-8")
    if not text.strip():
        return {}
    data = yaml.safe_load(text)
    return data if isinstance(data, dict) else {}


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


config = load_yaml(config_path)
removed = []

servers = config.get("mcp_servers")
if isinstance(servers, dict) and MCP_SERVER_ID in servers:
    del servers[MCP_SERVER_ID]
    removed.append("mcp_servers.%s" % MCP_SERVER_ID)
    if not servers:
        del config["mcp_servers"]

result = dump_yaml(config)
plugin_manifest = plugin_home / "plugin.yaml"
ours = plugin_home.is_dir() and (
    (load_yaml(plugin_manifest) or {}).get("name") == PLUGIN_NAME
)

if dry_run:
    sys.stdout.write(dump_yaml(redact(config)))
    sys.stderr.write("hermes adapter: dry-run, no files changed")
    if ours:
        sys.stderr.write("; would remove plugin home %s" % plugin_home)
    sys.stderr.write("\n")
else:
    if not config_path.is_file():
        sys.stderr.write("hermes adapter: %s absent, nothing to uninstall\n" % config_path)
    elif config_path.read_text(encoding="utf-8") == result:
        sys.stderr.write("hermes adapter: nothing to remove from %s\n" % config_path)
    else:
        write_atomic(config_path, result)
        sys.stderr.write(
            "hermes adapter: removed %s from %s\n"
            % (", ".join(removed) if removed else "nothing", config_path)
        )

    if ours:
        shutil.rmtree(plugin_home)
        sys.stderr.write("hermes adapter: removed plugin home %s\n" % plugin_home)
    elif plugin_home.exists():
        sys.stderr.write(
            "hermes adapter: leaving %s alone (not identified as %s)\n"
            % (plugin_home, PLUGIN_NAME)
        )
PYEOF
