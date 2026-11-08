#!/usr/bin/env bash
# Idempotent uninstall for the OpenClaw Matrix channel projection.
#
# Removes exactly the keys the installer writes from channels.matrix and
# nothing else: foreign keys (e.g. the controller's streaming settings)
# and foreign sub-keys of the dm/ and groups/ blocks are always
# preserved, as is the config file itself. When the matrix block ends up
# empty it is dropped, along with the channels key only if it is empty as
# well. Re-running is a no-op.
#
# Previews (--dry-run) always redact credential values in the rendered
# document: any non-empty string under a key ending in token/secret/
# password/passwd/apikey/credentials (case-insensitive) is printed as
# <redacted>, including credentials in blocks this adapter does not own;
# the file itself is written unredacted. The atomic write never loosens
# permissions, so a config that still carries a credential value after
# uninstall keeps its restrictive mode.
#
# Like the installer, the uninstaller is contract-driven: it re-derives
# the exact removal set from the bundle contract, so only keys the
# installer actually wrote (under the same contract) are removed.
#
# Usage:
#   uninstall.sh [--config <path>] [--dry-run]
#
#   --config <path>   target config file. Without it, the runtime's
#                     effective config is resolved in order:
#                     $OPENCLAW_CONFIG_PATH (exported by the Worker
#                     entrypoint), then the legacy $OPENCLAW_CONFIG,
#                     then $HOME/.openclaw/openclaw.json. A selected
#                     path that is a symlink is followed to its
#                     canonical target, which the read and the atomic
#                     write both operate on — the link itself is never
#                     replaced by a regular file.
#   --dry-run         print the resulting JSON to stdout, write nothing
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUNDLE_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
export AGENTTEAMS_BUNDLE_DIR="${AGENTTEAMS_BUNDLE_DIR:-$BUNDLE_DIR}"

CONFIG_PATH=""
DRY_RUN=0

usage() {
  cat <<'EOF'
Usage: uninstall.sh [--config <path>] [--dry-run]

Removes the agentteams-matrix keys from channels.matrix in the OpenClaw
config file. Without --config the effective config is resolved in
order: $OPENCLAW_CONFIG_PATH (exported by the Worker entrypoint), then
the legacy $OPENCLAW_CONFIG, then $HOME/.openclaw/openclaw.json. A
selected path that is a symlink is followed to its canonical target;
the link itself is never replaced. Foreign keys and the config file
itself are preserved.
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --config)
      if [ $# -lt 2 ]; then
        echo "ERROR: --config requires a path" >&2
        exit 2
      fi
      CONFIG_PATH="$2"
      shift 2
      ;;
    --dry-run)
      DRY_RUN=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "ERROR: unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

# Effective config target: explicit --config wins, then the runtime's
# config-path environment (the real Worker entrypoint exports
# OPENCLAW_CONFIG_PATH pointing at the canonical $HOME/openclaw.json,
# leaving $HOME/.openclaw/openclaw.json as a symlink to it), then the
# legacy variable, then the normal default.
if [ -z "${CONFIG_PATH}" ]; then
  if [ -n "${OPENCLAW_CONFIG_PATH:-}" ]; then
    CONFIG_PATH="${OPENCLAW_CONFIG_PATH}"
  elif [ -n "${OPENCLAW_CONFIG:-}" ]; then
    CONFIG_PATH="${OPENCLAW_CONFIG}"
  else
    CONFIG_PATH="${HOME}/.openclaw/openclaw.json"
  fi
fi

CONTRACT_PATH="${AGENTTEAMS_BUNDLE_DIR}/contract.yaml"
if [ ! -f "${CONTRACT_PATH}" ]; then
  echo "ERROR: bundle contract not found at ${CONTRACT_PATH}; set AGENTTEAMS_BUNDLE_DIR to the agentteams-matrix bundle directory" >&2
  exit 1
fi

python3 - "${CONFIG_PATH}" "${CONTRACT_PATH}" "${DRY_RUN}" <<'PY'
import json
import os
import stat
import sys
import tempfile
from pathlib import Path

config_path = Path(sys.argv[1])
contract_path = Path(sys.argv[2])
dry_run = sys.argv[3] == "1"

# The selected path may itself be a symlink (the Worker entrypoint
# ships $HOME/.openclaw/openclaw.json as a link to the canonical
# $HOME/openclaw.json): operate on the canonical target so the owned
# keys are removed from the target and the link itself is never
# replaced. strict=False also resolves a dangling link to its
# intended target.
if config_path.is_symlink():
    config_path = config_path.resolve()

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


def _document_has_credentials(document):
    if isinstance(document, dict):
        return any(
            (_is_credential_key(key) and isinstance(value, str) and value)
            or _document_has_credentials(value)
            for key, value in document.items()
        )
    if isinstance(document, list):
        return any(_document_has_credentials(item) for item in document)
    return False


def write_atomic(path, text, document):
    """Atomic write. 0600 whenever the document carries credentials, otherwise
    preserve the target's mode (0600 for a new file). The temp file is created
    0600 by mkstemp and only then chmod-ed to the final mode."""
    path.parent.mkdir(parents=True, exist_ok=True)
    if _document_has_credentials(document):
        mode = 0o600
    elif path.exists():
        mode = stat.S_IMODE(path.stat().st_mode)
    else:
        mode = 0o600
    fd, tmp = tempfile.mkstemp(dir=str(path.parent), prefix=path.name + ".", suffix=".tmp")
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            handle.write(text)
        os.chmod(tmp, mode)
        os.replace(tmp, path)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def render(document):
    return json.dumps(document, indent=2, sort_keys=True) + "\n"


# Removal set, re-derived from the contract exactly as the installer
# derives its write set (see install.sh for the upstream evidence):
#   enabled / accessToken — always written (the latter with
#     --with-credentials; removed defensively either way)
#   homeserver / userId / historyLimit — when declared
#   groupPolicy / groupAllowFrom — when group_allow_from is non-empty
#   autoJoin / autoJoinAllowlist — when rooms is non-empty
#   dm.policy / dm.allowFrom — when allow_from is non-empty (deep)
#   groups.<room>.requireMention — per contract room when require_mention
#     is declared (deep; an entry that empties out is dropped)

if not config_path.is_file():
    print("agentteams-matrix openclaw adapter: nothing to uninstall (no config file)")
    sys.exit(0)

try:
    config = json.loads(config_path.read_text(encoding="utf-8"))
except ValueError as exc:
    print(
        f"ERROR: existing config {config_path} is not valid JSON: {exc}",
        file=sys.stderr,
    )
    sys.exit(1)
if not isinstance(config, dict):
    print(
        f"ERROR: existing config {config_path} must contain a JSON object at the top level",
        file=sys.stderr,
    )
    sys.exit(1)

try:
    import yaml
except ImportError:
    print(
        "ERROR: pyyaml is required to read the bundle contract; install pyyaml>=6.0",
        file=sys.stderr,
    )
    sys.exit(1)
try:
    contract = yaml.safe_load(contract_path.read_text(encoding="utf-8"))
except yaml.YAMLError as exc:
    print(f"ERROR: bundle contract {contract_path} is not valid YAML: {exc}", file=sys.stderr)
    sys.exit(1)
if not isinstance(contract, dict):
    print(f"ERROR: bundle contract {contract_path} must be a YAML mapping", file=sys.stderr)
    sys.exit(1)
matrix = contract.get("matrix")
if not isinstance(matrix, dict) or not matrix:
    print(f"ERROR: bundle contract {contract_path} has no 'matrix' section", file=sys.stderr)
    sys.exit(1)

channels = config.get("channels")
matrix_cfg = channels.get("matrix") if isinstance(channels, dict) else None
if not isinstance(matrix_cfg, dict):
    print("agentteams-matrix openclaw adapter: nothing to uninstall")
    sys.exit(0)

flat_owned = {"enabled", "accessToken"}
if matrix.get("homeserver") is not None:
    flat_owned.add("homeserver")
if matrix.get("user_id") is not None:
    flat_owned.add("userId")
if matrix.get("history_limit") is not None:
    flat_owned.add("historyLimit")
if matrix.get("group_allow_from"):
    flat_owned.update({"groupPolicy", "groupAllowFrom"})
if matrix.get("rooms"):
    flat_owned.update({"autoJoin", "autoJoinAllowlist"})

removed = [key for key in matrix_cfg if key in flat_owned]
for key in removed:
    del matrix_cfg[key]

# dm: the installer writes policy and allowFrom together.
dm = matrix_cfg.get("dm")
if isinstance(dm, dict) and matrix.get("allow_from"):
    for key in ("policy", "allowFrom"):
        if key in dm:
            del dm[key]
            removed.append(f"dm.{key}")
    if not dm:
        del matrix_cfg["dm"]

# groups: per contract room, drop the requireMention key the installer
# wrote, then drop the entry it created once it is empty (the installer
# always creates an entry per contract room — with requireMention when
# declared, or as an empty admission marker). The groups key itself is
# dropped when it empties out.
groups = matrix_cfg.get("groups")
if isinstance(groups, dict) and matrix.get("rooms"):
    for room in matrix["rooms"]:
        entry = groups.get(room)
        if not isinstance(entry, dict):
            continue
        if matrix.get("require_mention") is not None and "requireMention" in entry:
            del entry["requireMention"]
            removed.append(f"groups.{room}.requireMention")
        if not entry:
            del groups[room]
            removed.append(f"groups.{room}")
    if not groups:
        del matrix_cfg["groups"]

if not removed:
    print("agentteams-matrix openclaw adapter: nothing to uninstall")
    sys.exit(0)

if not matrix_cfg:
    del channels["matrix"]
    if not channels:
        del config["channels"]

if dry_run:
    sys.stdout.write(render(redact(config)))
    print(
        f"agentteams-matrix openclaw adapter: dry run, would remove "
        f"{len(removed)} key(s) from channels.matrix (target: {config_path})",
        file=sys.stderr,
    )
    sys.exit(0)

write_atomic(config_path, render(config), config)
print(
    f"agentteams-matrix openclaw adapter: removed {len(removed)} key(s) "
    f"from channels.matrix in {config_path}"
)
PY
