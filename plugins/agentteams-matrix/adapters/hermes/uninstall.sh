#!/usr/bin/env bash
# Idempotent uninstall for the Hermes Matrix channel projection.
#
# Removes exactly what the installer writes — in both config shapes it
# has produced — and nothing else:
#
#   * the current nested shape: enabled and token (typed
#     PlatformConfig fields) plus homeserver, user_id, allowed_users,
#     allowed_rooms, require_mention inside the block's extra map;
#   * the legacy flat shape written before the #1458 fix: the same
#     non-typed keys at the top level of the platforms.matrix block.
#
# Foreign keys already present in the block (or in its extra map) are
# preserved, as is the rest of the config file. When the extra map ends
# up empty it is dropped; when the platforms.matrix block ends up empty
# it is dropped, along with the platforms key only if it is empty as
# well. A partial-projection marker left by the installer
# (<config dir>/.agentteams-matrix/hermes-install.json) is removed as
# well, including its directory when it becomes empty. Re-running is a
# no-op.
#
# Previews (--dry-run) always redact credential values in the rendered
# document: any non-empty string under a key ending in token/secret/
# password/passwd/apikey/credentials (case-insensitive) is printed as
# <redacted>, including credentials in blocks this adapter does not own;
# the file itself is written unredacted. The atomic write never loosens
# permissions, so a config that still carries a credential value after
# uninstall keeps its restrictive mode.
#
# Usage:
#   uninstall.sh [--config <path>] [--dry-run]
#
#   --config <path>   target config file
#                     (default: $HOME/.hermes/config.yaml)
#   --dry-run         print the resulting YAML to stdout, write nothing
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUNDLE_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
export AGENTTEAMS_BUNDLE_DIR="${AGENTTEAMS_BUNDLE_DIR:-$BUNDLE_DIR}"

CONFIG_PATH="${HOME}/.hermes/config.yaml"
DRY_RUN=0

usage() {
  cat <<'EOF'
Usage: uninstall.sh [--config <path>] [--dry-run]

Removes the agentteams-matrix keys from the platforms.matrix block of
the Hermes config file (default: $HOME/.hermes/config.yaml), in both
the nested extra shape the installer writes and the legacy flat shape
it wrote before the #1458 fix, and removes the partial-projection
marker it may have left. Foreign keys and the config file itself are
preserved.
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

python3 - "${CONFIG_PATH}" "${DRY_RUN}" <<'PY'
import os
import stat
import sys
import tempfile
from pathlib import Path

config_path = Path(sys.argv[1])
dry_run = sys.argv[2] == "1"

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


def fail(message):
    print(f"ERROR: {message}", file=sys.stderr)
    sys.exit(1)


try:
    import yaml
except ImportError:
    fail("pyyaml is required to read the config file; install pyyaml>=6.0")


def render(document):
    return yaml.safe_dump(
        document,
        sort_keys=False,
        allow_unicode=True,
        default_flow_style=False,
    )


# Exactly the keys the installer can write:
#  - top level (typed fields, plus the legacy flat shape written by
#    pre-#1458 installers): enabled, token, homeserver, user_id,
#    allowed_users, allowed_rooms, require_mention
#  - inside the block's extra map (the #1458 shape): homeserver,
#    user_id, allowed_users, allowed_rooms, require_mention
OWNED_KEYS = {
    "enabled",
    "homeserver",
    "user_id",
    "allowed_users",
    "allowed_rooms",
    "require_mention",
    "token",
}
OWNED_EXTRA_KEYS = {
    "homeserver",
    "user_id",
    "allowed_users",
    "allowed_rooms",
    "require_mention",
}
MARKER_PATH = config_path.parent / ".agentteams-matrix" / "hermes-install.json"

if not config_path.is_file() and not MARKER_PATH.is_file():
    print("agentteams-matrix hermes adapter: nothing to uninstall (no config file)")
    sys.exit(0)

config = {}
if config_path.is_file():
    try:
        loaded = yaml.safe_load(config_path.read_text(encoding="utf-8"))
    except yaml.YAMLError as exc:
        fail(f"existing config {config_path} is not valid YAML: {exc}")
    if loaded is not None:
        config = loaded
if not isinstance(config, dict):
    fail(f"existing config {config_path} must be a YAML mapping at the top level")

platforms = config.get("platforms")
matrix_cfg = platforms.get("matrix") if isinstance(platforms, dict) else None
if not isinstance(matrix_cfg, dict):
    matrix_cfg = None

removed = []
if matrix_cfg is not None:
    for key in [key for key in matrix_cfg if key in OWNED_KEYS]:
        del matrix_cfg[key]
        removed.append(key)
    extra = matrix_cfg.get("extra")
    if isinstance(extra, dict):
        for key in [key for key in extra if key in OWNED_EXTRA_KEYS]:
            del extra[key]
            removed.append(f"extra.{key}")
        if not extra:
            del matrix_cfg["extra"]
            removed.append("extra")
    if not matrix_cfg:
        del platforms["matrix"]
        if not platforms:
            del config["platforms"]

marker_present = MARKER_PATH.is_file()
if not removed and not marker_present:
    print("agentteams-matrix hermes adapter: nothing to uninstall")
    sys.exit(0)

if dry_run:
    parts = []
    if removed:
        parts.append(f"{len(removed)} key(s) from the platforms.matrix block")
    if marker_present:
        parts.append(f"the install marker {MARKER_PATH}")
    sys.stdout.write(render(redact(config)))
    print(
        f"agentteams-matrix hermes adapter: dry run, would remove "
        f"{' and '.join(parts)} (target: {config_path})",
        file=sys.stderr,
    )
    sys.exit(0)

if removed:
    write_atomic(config_path, render(config), config)
if marker_present:
    MARKER_PATH.unlink()
    try:
        MARKER_PATH.parent.rmdir()
    except OSError:
        pass

parts = []
if removed:
    parts.append(f"removed {len(removed)} key(s) from the platforms.matrix block in {config_path}")
if marker_present:
    parts.append(f"removed the install marker {MARKER_PATH}")
print("agentteams-matrix hermes adapter: " + "; ".join(parts))
PY
