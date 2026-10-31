#!/usr/bin/env bash
set -euo pipefail

# TeamHarness OpenClaw adapter — install.
#
# Projects the bundle contract onto the OpenClaw runtime, statically:
#
#   1. mcp.servers.teamharness  -> the MCP server entry in openclaw.json
#      (command: python3 -m mcp.server, cwd: <bundle root>, env carries a
#      ${TEAMHARNESS_RUNTIME_CONFIG} reference — the variable name, never a
#      value; OpenClaw expands ${VAR} over the whole parsed config).
#   2. A self-contained plugin root is staged next to the runtime config --
#      <config dir>/plugins/agentteams-teamharness/ -- from the adapter
#      directory: index.mjs, openclaw.plugin.json (skills rewritten to in-root
#      paths) and a copy of the bundle's skills/ tree. The bare adapter
#      directory is never registered: OpenClaw rejects a manifest skill path
#      that escapes the plugin root, and it also expects the plugin root to be
#      owned by the runtime user (staging satisfies both).
#   3. plugins.allow / plugins.load.paths / plugins.entries -> register that
#      staged root (plugins.load.paths carries it; an older install's bare
#      adapter path is removed). Uninstall removes the staged root again.
#
# The effective config target is resolved in priority order:
# --config > $OPENCLAW_CONFIG_PATH (the Worker export) > $OPENCLAW_CONFIG
# (legacy) > $HOME/.openclaw/openclaw.json. When the selected path is a
# symlink (the Worker links ~/.openclaw/openclaw.json to its canonical
# $HOME/openclaw.json), the canonical target is read and written -- the
# symlink itself is preserved, never atomically replaced by a regular file.
# The target is derived from the runtime, never guessed from a variable name.
#
# Idempotent: canonical serialization (sorted keys, 2-space indent) means two
# consecutive runs produce byte-identical output; the file is only rewritten
# when the bytes would change. --dry-run prints the resulting configuration
# on stdout and changes nothing. Uninstall removes exactly the keys written
# here (see uninstall.sh); it never removes bundle source files.

usage() {
  cat <<'EOF'
Usage: install.sh [--config <path>] [--workspace <path>] [--dry-run]

  --config <path>  OpenClaw config file (default: $OPENCLAW_CONFIG_PATH,
                   then $OPENCLAW_CONFIG, then $HOME/.openclaw/openclaw.json).
                   A symlink is honoured: its canonical target is read and
                   written, the link itself is preserved.
  --workspace <path>
                   OpenClaw workspace directory for isolated runs (created if
                   absent). The plugin registration is absolute-path based
                   (plugins.load.paths) and does not depend on it.
  --dry-run        Print the resulting configuration on stdout; change nothing
EOF
}

CONFIG=""
WORKSPACE=""
DRY_RUN=0
while [ $# -gt 0 ]; do
  case "$1" in
    --config) CONFIG="${2:?--config requires a value}"; shift 2 ;;
    --workspace) WORKSPACE="${2:?--workspace requires a value}"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "ERROR: unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUNDLE_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
export AGENTTEAMS_BUNDLE_DIR="${AGENTTEAMS_BUNDLE_DIR:-$BUNDLE_DIR}"

if [ ! -f "${BUNDLE_DIR}/contract.yaml" ]; then
  echo "ERROR: bundle contract not found at ${BUNDLE_DIR}/contract.yaml" >&2
  exit 1
fi
if [ ! -f "${BUNDLE_DIR}/mcp/server.py" ]; then
  echo "ERROR: MCP server entry not found at ${BUNDLE_DIR}/mcp/server.py" >&2
  exit 1
fi
if [ ! -f "${SCRIPT_DIR}/index.mjs" ]; then
  echo "ERROR: loadable plugin entry not found at ${SCRIPT_DIR}/index.mjs" >&2
  exit 1
fi
if [ -n "$WORKSPACE" ] && [ ! -d "$WORKSPACE" ]; then
  mkdir -p "$WORKSPACE"
fi
[ -n "$CONFIG" ] || \
  CONFIG="${OPENCLAW_CONFIG_PATH:-${OPENCLAW_CONFIG:-$HOME/.openclaw/openclaw.json}}"

export TH_OC_ADAPTER_DIR="$SCRIPT_DIR"
export TH_OC_BUNDLE_DIR="$BUNDLE_DIR"
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
bundle_dir = Path(os.environ["TH_OC_BUNDLE_DIR"]).resolve()
config_path = Path(os.environ["TH_OC_CONFIG"]).expanduser()
# Effective config target: when the selected path is a symlink (the Worker
# links ~/.openclaw/openclaw.json to its canonical $HOME/openclaw.json),
# operate on the canonical target — read from it, and let write_atomic's
# temp file + os.replace land in the canonical directory — so the symlink
# survives instead of being atomically replaced by a regular file.
# strict=False: a dangling link resolves to its intended target, which the
# write then creates.
config_path = config_path.resolve(strict=False)
dry_run = os.environ.get("TH_OC_DRY_RUN") == "1"

PLUGIN_ID = "agentteams-teamharness"
MCP_SERVER_ID = "teamharness"
ENV_VAR = "TEAMHARNESS_RUNTIME_CONFIG"
MANIFEST_NAME = "openclaw.plugin.json"


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


def discover_skill_entries():
    """Skill directories (each containing a SKILL.md) as paths relative to the
    *staged* plugin root, sorted. Empty when the bundle ships no skills."""
    roots = bundle_dir / "skills"
    entries = []
    if roots.is_dir():
        for skill_md in roots.glob("*/*/SKILL.md"):
            entries.append(
                "skills/" + skill_md.parent.relative_to(roots).as_posix()
            )
    return sorted(entries)


def stage_plugin(skills):
    """Stage a self-contained plugin root next to the runtime config.

    OpenClaw rejects a manifest skill path that escapes the plugin root, and
    it refuses a plugin root whose ownership differs from the running user.
    The repository adapter directory therefore cannot be the plugin root: copy
    the entry, the manifest (with in-root skill paths) and the bundle skills
    into a runtime-owned directory and register that instead.
    """
    plugin_dir = config_path.parent / "plugins" / PLUGIN_ID
    if not dry_run:
        plugin_dir.mkdir(parents=True, exist_ok=True)
        shutil.copy2(adapter_dir / "index.mjs", plugin_dir / "index.mjs")
        manifest = load_json(adapter_dir / MANIFEST_NAME)
        manifest["skills"] = skills
        write_atomic(plugin_dir / MANIFEST_NAME, dump_json(manifest))
        staged_skills = plugin_dir / "skills"
        if staged_skills.exists():
            shutil.rmtree(staged_skills)
        source_skills = bundle_dir / "skills"
        if source_skills.is_dir():
            shutil.copytree(source_skills, staged_skills)
    return plugin_dir


def project(config):
    skills = discover_skill_entries()
    plugin_dir = stage_plugin(skills)
    # 1. MCP server entry — upstream key: mcp.servers.<name> (McpServerConfig
    #    in higress-group/openclaw src/config/types.mcp.ts).
    mcp_entry = {
        "command": "python3",
        "args": ["-m", "mcp.server"],
        "cwd": str(bundle_dir),
        # Pass-through by reference: OpenClaw expands ${TEAMHARNESS_RUNTIME_CONFIG}
        # from the environment; the value is never written into the config.
        "env": {ENV_VAR: "${" + ENV_VAR + "}"},
    }
    config.setdefault("mcp", {}).setdefault("servers", {})[MCP_SERVER_ID] = mcp_entry

    # 2. Plugin registration (openclaw-cms-plugin precedent in this repo).
    plugins = config.setdefault("plugins", {})
    allow = plugins.setdefault("allow", [])
    if PLUGIN_ID not in allow:
        allow.append(PLUGIN_ID)
    load = plugins.setdefault("load", {})
    paths = load.setdefault("paths", [])
    # The staged, self-contained plugin root (see stage_plugin). The bare
    # repository adapter directory is never registered: its skills live
    # outside the directory and discovery rejects them.
    if str(adapter_dir) in paths:
        paths.remove(str(adapter_dir))
    if str(plugin_dir) not in paths:
        paths.append(str(plugin_dir))
    entries = plugins.setdefault("entries", {})
    entries.setdefault(PLUGIN_ID, {})["enabled"] = True

    if not dry_run:
        sys.stderr.write(
            "openclaw adapter: staged plugin root %s with %d skill director%s\n"
            % (plugin_dir, len(skills), "y" if len(skills) == 1 else "ies")
        )


config = load_json(config_path)
project(config)
result = dump_json(config)

if dry_run:
    sys.stdout.write(dump_json(redact(config)))
    sys.stderr.write("openclaw adapter: dry-run, no files changed\n")
else:
    if config_path.is_file() and config_path.read_text(encoding="utf-8") == result:
        sys.stderr.write("openclaw adapter: %s already up to date\n" % config_path)
    else:
        write_atomic(config_path, result)
        sys.stderr.write("openclaw adapter: wrote %s\n" % config_path)
PYEOF
