#!/usr/bin/env bash
set -euo pipefail

# TeamHarness Hermes adapter — install.
#
# Projects the bundle contract onto the Hermes runtime, statically:
#
#   1. mcp_servers.teamharness -> the MCP server entry in
#      ~/.hermes/config.yaml (command: python3 <bundle>/mcp/server.py; env
#      carries a ${TEAMHARNESS_RUNTIME_CONFIG} reference — the variable
#      name, never a value; hermes resolves ${ENV_VAR} from os.environ,
#      including ~/.hermes/.env, and keeps the placeholder verbatim when
#      unset). There is no cwd field in hermes's MCP entry shape, so the
#      absolute script path is used; server.py imports its sibling modules
#      from its own directory, which works from any cwd.
#   2. The plugin (__init__.py + plugin.yaml + register.py + the bundle's
#      skills tree) is synced into ~/.hermes/plugins/agentteams-teamharness/ —
#      the upstream third-party plugin location. __init__.py is required:
#      hermes_cli/plugins.py's directory loader raises "No __init__.py" and
#      disables the plugin without it.
#
# Idempotent: the YAML is dumped canonically (sorted keys), files are only
# rewritten when the bytes would change, so two consecutive runs are
# byte-identical. --dry-run prints the resulting config.yaml on stdout and
# changes nothing. Requires python3 with PyYAML (hermes-worker itself
# declares pyyaml>=6.0).

usage() {
  cat <<'EOF'
Usage: install.sh [--config <path>] [--plugin-home <path>] [--dry-run]

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
BUNDLE_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
export AGENTTEAMS_BUNDLE_DIR="${AGENTTEAMS_BUNDLE_DIR:-$BUNDLE_DIR}"
HERMES_HOME="${HERMES_HOME:-$HOME/.hermes}"

if [ ! -f "${BUNDLE_DIR}/contract.yaml" ]; then
  echo "ERROR: bundle contract not found at ${BUNDLE_DIR}/contract.yaml" >&2
  exit 1
fi
if [ ! -f "${BUNDLE_DIR}/mcp/server.py" ]; then
  echo "ERROR: MCP server entry not found at ${BUNDLE_DIR}/mcp/server.py" >&2
  exit 1
fi
[ -n "$CONFIG" ] || CONFIG="${HERMES_HOME}/config.yaml"
[ -n "$PLUGIN_HOME" ] || PLUGIN_HOME="${HERMES_HOME}/plugins/agentteams-teamharness"

export TH_HM_ADAPTER_DIR="$SCRIPT_DIR"
export TH_HM_BUNDLE_DIR="$BUNDLE_DIR"
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

adapter_dir = Path(os.environ["TH_HM_ADAPTER_DIR"]).resolve()
bundle_dir = Path(os.environ["TH_HM_BUNDLE_DIR"]).resolve()
config_path = Path(os.environ["TH_HM_CONFIG"]).expanduser()
plugin_home = Path(os.environ["TH_HM_PLUGIN_HOME"]).expanduser()
dry_run = os.environ.get("TH_HM_DRY_RUN") == "1"

MCP_SERVER_ID = "teamharness"
ENV_VAR = "TEAMHARNESS_RUNTIME_CONFIG"
PLUGIN_FILES = ("plugin.yaml", "register.py", "__init__.py")


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
    """Atomic write; keeps the target's mode when it exists, 0600 for new files."""
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


def sync_file(src, dst):
    """Copy src over dst only when the bytes differ (idempotent)."""
    data = src.read_bytes()
    if dst.is_file() and dst.read_bytes() == data:
        return False
    write_atomic(dst, data.decode("utf-8"))
    return True


def sync_skills():
    """Mirror the bundle's skills tree into <plugin-home>/skills/.

    Files are copied when missing or changed; nothing is deleted, so a
    re-install is a no-op byte-for-byte."""
    bundle_skills = bundle_dir / "skills"
    if not bundle_skills.is_dir():
        return 0
    copied = 0
    for path in sorted(p for p in bundle_skills.rglob("*") if p.is_file()):
        rel = path.relative_to(bundle_skills)
        if sync_file(path, plugin_home / "skills" / rel):
            copied += 1
    return copied


config = load_yaml(config_path)

# 1. MCP server entry — upstream key: mcp_servers.<name> (tools/mcp_tool.py,
#    _load_mcp_config). Shape: command/args/env for stdio servers; there is
#    no cwd field, so the absolute server path is used instead.
mcp_entry = {
    "command": "python3",
    "args": [str(bundle_dir / "mcp" / "server.py")],
    # Pass-through by reference: hermes resolves ${TEAMHARNESS_RUNTIME_CONFIG}
    # from the environment (including ~/.hermes/.env); the value is never
    # written into the config.
    "env": {ENV_VAR: "${" + ENV_VAR + "}"},
}
config.setdefault("mcp_servers", {})[MCP_SERVER_ID] = mcp_entry

result = dump_yaml(config)

if dry_run:
    sys.stdout.write(dump_yaml(redact(config)))
    sys.stderr.write("hermes adapter: dry-run, no files changed\n")
else:
    if config_path.is_file() and config_path.read_text(encoding="utf-8") == result:
        sys.stderr.write("hermes adapter: %s already up to date\n" % config_path)
    else:
        write_atomic(config_path, result)
        sys.stderr.write("hermes adapter: wrote %s\n" % config_path)

    # 2. Plugin directory sync (upstream third-party plugin location).
    for name in PLUGIN_FILES:
        if sync_file(adapter_dir / name, plugin_home / name):
            sys.stderr.write("hermes adapter: synced %s\n" % (plugin_home / name))
    copied = sync_skills()
    sys.stderr.write(
        "hermes adapter: plugin home %s up to date (%d skill file%s copied)\n"
        % (plugin_home, copied, "" if copied == 1 else "s")
    )
PYEOF
