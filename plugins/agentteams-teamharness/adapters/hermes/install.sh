#!/usr/bin/env bash
set -euo pipefail

# TeamHarness Hermes adapter — install.
#
# Projects the bundle contract onto the Hermes runtime, statically:
#
#   1. mcp_servers.teamharness -> the MCP server entry in
#      ~/.hermes/config.yaml (command: python3 <bundle>/mcp/server.py).
#      env carries a narrow, documented allowlist of ${NAME} references —
#      variable names, never values, never whole-env inheritance: the
#      supported hermes SDK (tools/mcp_tool.py) gives the MCP child only a
#      small safe baseline (PATH/HOME/etc. + XDG_*) from the parent plus
#      the entry's env map (${NAME} resolved from the environment,
#      including $HERMES_HOME/.env, at config load), so the child must be
#      projected the storage/Matrix context it needs to pull the
#      authoritative project state and push task files. Each allowlisted
#      variable is written only when resolvable: set in the installer's
#      environment, or its key present in $HERMES_HOME/.env (keys are
#      parsed; values are never read). Worker role overrides
#      (AGENTTEAMS_AGENT_ROLE / AGENTTEAMS_WORKER_ROLE) stay excluded —
#      role is operator-set only. There is no cwd field in hermes's MCP
#      entry shape, so the absolute script path is used; server.py imports
#      its sibling modules from its own directory, which works from any cwd.
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
export TH_HM_ENV_FILE="${HERMES_HOME}/.env"
export TH_HM_DRY_RUN="$DRY_RUN"

python3 - <<'PYEOF'
import os
import re
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
PLUGIN_FILES = ("plugin.yaml", "register.py", "__init__.py")
ENV_FILE = Path(os.environ["TH_HM_ENV_FILE"]).expanduser()

# MCP child environment — a narrow, documented allowlist (maintainer P1 on
# 1143b604). The supported hermes SDK (tools/mcp_tool.py) gives the MCP
# child only a small safe baseline (PATH, HOME, TMPDIR, LANG, TERM,
# LC_ALL, SHELL, USER, plus XDG_*) from the parent process and this
# entry's env map — nothing else crosses the boundary. The server needs
# the storage/Matrix context to pull the authoritative project state and
# push task files, so every required variable is projected as a ${NAME}
# reference — the variable name, never a value (hermes resolves ${NAME}
# from the environment, including ~/.hermes/.env, at config load and
# keeps the placeholder verbatim when unresolvable). A variable is
# projected only when something can resolve it: set (non-empty) in the
# installer's environment, or its key present in $HERMES_HOME/.env (keys
# are parsed; values are never read). Otherwise the child would receive a
# literal "${NAME}" it could never resolve.
#
# Explicitly excluded:
#   AGENTTEAMS_AGENT_ROLE, AGENTTEAMS_WORKER_ROLE — Worker role overrides
#       stay blocked: role is operator-set only, never MCP-child-set.
#   everything else (PATH, HOME, arbitrary variables) — no whole-env
#       inheritance.
MCP_CHILD_ENV_ALLOWLIST = (
    # state/runtime
    "TEAMHARNESS_RUNTIME_CONFIG",
    "TEAMHARNESS_SHARED_DIR",
    "AGENTTEAMS_SHARED_DIR",
    "AGENTTEAMS_STORAGE_PREFIX",
    "AGENTTEAMS_SHARED_STORAGE_PREFIX",
    "TEAMHARNESS_MATRIX_CONTEXT_FILE",
    # Matrix
    "AGENTTEAMS_MATRIX_URL",
    "AGENTTEAMS_WORKER_MATRIX_TOKEN",
    "AGENTTEAMS_WORKER_NAME",
    "AGENTTEAMS_MATRIX_USER_ID",
    "AGENTTEAMS_MATRIX_DOMAIN",
    # storage credentials
    "AGENTTEAMS_FS_ENDPOINT",
    "AGENTTEAMS_FS_ACCESS_KEY",
    "AGENTTEAMS_FS_SECRET_KEY",
)


def _dotenv_keys(path):
    """Key names of a dotenv file — keys only, values are never parsed."""
    keys = set()
    if not path.is_file():
        return keys
    for line in path.read_text(encoding="utf-8", errors="replace").splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key = line.split("=", 1)[0].strip()
        if key.startswith(("export ", "export\t")):
            key = key.split(None, 1)[1].strip()
        if key and re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", key):
            keys.add(key)
    return keys


def _projected_mcp_env():
    """The entry's env map: allowlisted vars as ${NAME} references, only
    when resolvable (set in the installer env or a $HERMES_HOME/.env key)."""
    resolvable = {name for name, value in os.environ.items() if value}
    resolvable |= _dotenv_keys(ENV_FILE)
    return {
        name: "${" + name + "}"
        for name in MCP_CHILD_ENV_ALLOWLIST
        if name in resolvable
    }


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
    # Pass-through by reference: hermes resolves each ${NAME} from the
    # environment (including ~/.hermes/.env); values are never written
    # into the config (see MCP_CHILD_ENV_ALLOWLIST above).
    "env": _projected_mcp_env(),
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
