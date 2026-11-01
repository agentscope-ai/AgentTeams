#!/usr/bin/env bash
# Idempotent config projection of the agentteams-matrix bundle contract
# onto the OpenClaw runtime.
#
# The adapter reads the bundle contract (contract.yaml, matrix: section)
# and merges its keys into channels.matrix of the OpenClaw config file.
# It is a pure file projector: no openclaw binary is invoked and no
# network access is performed.
#
# Usage:
#   install.sh [--config <path>] [--dry-run] [--with-credentials]
#
#   --config <path>      target config file. Without it, the runtime's
#                        effective config is resolved in order:
#                        $OPENCLAW_CONFIG_PATH (exported by the Worker
#                        entrypoint), then the legacy $OPENCLAW_CONFIG,
#                        then $HOME/.openclaw/openclaw.json. A selected
#                        path that is a symlink is followed to its
#                        canonical target, which the read and the
#                        atomic write both operate on — the link
#                        itself is never replaced by a regular file.
#   --dry-run            print the resulting JSON to stdout, write nothing
#   --with-credentials   also write the access token read from
#                        $AGENTTEAMS_MATRIX_ACCESS_TOKEN; the config
#                        file is written 0600 whenever it carries
#                        credential values
#
# Only the keys this adapter owns are merged into channels.matrix; every
# other key in the config file is preserved, including foreign sub-keys of
# the dm/ and groups/ blocks (both are deep-merged, never replaced).
# Re-running with an unchanged contract produces a byte-identical file.
# Previews (--dry-run) always redact credential values in the rendered
# document: any non-empty string under a key ending in token/secret/
# password/passwd/apikey/credentials (case-insensitive) is printed as
# <redacted>, including credentials in blocks this adapter does not own;
# the file itself is written unredacted. The atomic write never loosens
# permissions: a document that carries credential values lands at 0600,
# and an existing restrictive mode survives every rewrite. By default no
# credential material is written at all (the contract's
# access_token_ref has no OpenClaw key — see README.md).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUNDLE_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
export AGENTTEAMS_BUNDLE_DIR="${AGENTTEAMS_BUNDLE_DIR:-$BUNDLE_DIR}"

CONFIG_PATH=""
DRY_RUN=0
WITH_CREDENTIALS=0

usage() {
  cat <<'EOF'
Usage: install.sh [--config <path>] [--dry-run] [--with-credentials]

Projects the agentteams-matrix contract onto channels.matrix in the
OpenClaw config file. Without --config the effective config is
resolved in order: $OPENCLAW_CONFIG_PATH (exported by the Worker
entrypoint), then the legacy $OPENCLAW_CONFIG, then
$HOME/.openclaw/openclaw.json. A selected path that is a symlink is
followed to its canonical target; the link itself is never replaced.
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
    --with-credentials)
      WITH_CREDENTIALS=1
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

python3 - "${CONFIG_PATH}" "${CONTRACT_PATH}" "${DRY_RUN}" "${WITH_CREDENTIALS}" <<'PY'
import json
import os
import stat
import sys
import tempfile
from pathlib import Path

config_path = Path(sys.argv[1])
contract_path = Path(sys.argv[2])
dry_run = sys.argv[3] == "1"
with_credentials = sys.argv[4] == "1"

# The selected path may itself be a symlink (the Worker entrypoint
# ships $HOME/.openclaw/openclaw.json as a link to the canonical
# $HOME/openclaw.json): operate on the canonical target so the read
# and the atomic write go through the link instead of replacing it —
# the temp file and os.replace then happen in the target's directory.
# strict=False also resolves a dangling link to its intended target,
# which the write then creates.
if config_path.is_symlink():
    config_path = config_path.resolve()

TOKEN_KEY = "accessToken"

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
    fail("pyyaml is required to read the bundle contract; install pyyaml>=6.0")

try:
    contract = yaml.safe_load(contract_path.read_text(encoding="utf-8"))
except yaml.YAMLError as exc:
    fail(f"bundle contract {contract_path} is not valid YAML: {exc}")
if not isinstance(contract, dict):
    fail(f"bundle contract {contract_path} must be a YAML mapping")
matrix = contract.get("matrix")
if not isinstance(matrix, dict) or not matrix:
    fail(f"bundle contract {contract_path} has no 'matrix' section")

# contract.yaml (snake_case) -> openclaw.json channels.matrix (camelCase).
# Key names and shapes follow the OpenClaw matrix extension's channel
# config schema (upstream OpenClaw, extensions/matrix/src/config-schema.ts
# unless noted); every mapping is annotated with its upstream evidence.

# Flat owned keys.
flat = {}
# enabled — config-schema.ts:103. The channel is active once projected:
# the runtime treats channels.matrix presence as the provider being
# configured (monitor/index.ts:238).
flat["enabled"] = True
# homeserver — config-schema.ts:127; docs setup.md:48.
if matrix.get("homeserver") is not None:
    flat["homeserver"] = matrix["homeserver"]
# userId — config-schema.ts:130; docs setup.md:64.
if matrix.get("user_id") is not None:
    flat["userId"] = matrix["user_id"]
# historyLimit — config-schema.ts:160; runtime monitor/index.ts:267.
if matrix.get("history_limit") is not None:
    flat["historyLimit"] = matrix["history_limit"]

# dm.allowFrom — the nested dm block is {enabled, policy, allowFrom}
# (src/channels/plugins/config-schema.ts:89-96). Policy values are
# pairing|allowlist|open|disabled (src/config/zod-schema.core.ts:436) and
# the runtime default is "pairing" (monitor/index.ts:264), so an explicit
# allowlist policy is required for allowFrom to be a hard gate.
dm = {}
allow_from = list(matrix.get("allow_from") or [])
if allow_from:
    dm["policy"] = "allowlist"
    dm["allowFrom"] = allow_from

# groupAllowFrom — group sender allowlist (config-schema.ts:142,163).
# groupPolicy is written alongside so the list is a hard gate; the
# fail-closed default is "allowlist" once channels.matrix exists
# (src/config/zod-schema.channel-messaging-common.ts:65,
# monitor/index.ts:236), and the production controller writes the same
# pair (agentteams-controller .../agentconfig/generator.go).
group_allow_from = list(matrix.get("group_allow_from") or [])
if group_allow_from:
    flat["groupPolicy"] = "allowlist"
    flat["groupAllowFrom"] = group_allow_from

# rooms — a room list has no top-level key: the schema's groups/rooms are
# room->entry maps (config-schema.ts:169-170; docs setup.md:122 call rooms
# the legacy alias). Invite acceptance is gated by autoJoin, default "off"
# (onboarding.ts:187), restricted by autoJoinAllowlist, which accepts
# literal room IDs (config-schema.ts:161-162, target-ids.ts:52-61).
# Per-room admission under groupPolicy=allowlist comes from the room entry
# existing in groups (monitor/rooms.ts:39-52,
# monitor/handler-ingress-access.ts:160-170).
rooms = list(matrix.get("rooms") or [])
groups = {}
if rooms:
    flat["autoJoin"] = "allowlist"
    flat["autoJoinAllowlist"] = rooms
    require_mention = matrix.get("require_mention")
    for room in rooms:
        entry = {}
        if require_mention is not None:
            # requireMention is a per-room group-entry field
            # (src/channels/plugins/config-schema.ts:62; docs
            # access-control.md:69) — there is no top-level requireMention
            # in MatrixConfigSchema.
            entry["requireMention"] = bool(require_mention)
        groups[room] = entry

# NOT projected (no upstream key — see README.md):
#   access_token_ref — SecretRef ids must match ^[A-Z][A-Z0-9_]{0,127}$
#     for env/store sources (src/config/zod-schema.secret-input.ts:22-55),
#     so the ref cannot be expressed; credentials go through
#     --with-credentials (accessToken, config-schema.ts:131) only.
#   history_mode     — no such key in the matrix extension.

access_token = None
if with_credentials:
    access_token = os.environ.get("AGENTTEAMS_MATRIX_ACCESS_TOKEN", "")
    if not access_token:
        fail(
            "--with-credentials requires the AGENTTEAMS_MATRIX_ACCESS_TOKEN "
            "environment variable to be set"
        )

config = {}
if config_path.is_file():
    raw = config_path.read_text(encoding="utf-8")
    if raw.strip():
        try:
            config = json.loads(raw)
        except ValueError as exc:
            fail(f"existing config {config_path} is not valid JSON: {exc}")
    if not isinstance(config, dict):
        fail(f"existing config {config_path} must contain a JSON object at the top level")

channels = config.get("channels")
if channels is None:
    channels = {}
if not isinstance(channels, dict):
    fail(f"'channels' in {config_path} is not a JSON object")
matrix_cfg = channels.get("matrix")
if matrix_cfg is None:
    matrix_cfg = {}
if not isinstance(matrix_cfg, dict):
    fail(f"'channels.matrix' in {config_path} is not a JSON object")

# Merge: flat owned keys overwrite; dm/ and groups/ are deep-merged so
# foreign sub-keys survive.
merged = {**matrix_cfg, **flat}
if access_token:
    merged[TOKEN_KEY] = access_token
if dm:
    existing_dm = matrix_cfg.get("dm")
    if existing_dm is None:
        existing_dm = {}
    if not isinstance(existing_dm, dict):
        fail(f"'channels.matrix.dm' in {config_path} is not a JSON object")
    merged["dm"] = {**existing_dm, **dm}
if groups:
    existing_groups = matrix_cfg.get("groups")
    if existing_groups is None:
        existing_groups = {}
    if not isinstance(existing_groups, dict):
        fail(f"'channels.matrix.groups' in {config_path} is not a JSON object")
    new_groups = {**existing_groups}
    for room, entry in groups.items():
        current = new_groups.get(room)
        new_groups[room] = {**current, **entry} if isinstance(current, dict) else entry
    merged["groups"] = new_groups
channels["matrix"] = merged
config["channels"] = channels


def render(document):
    return json.dumps(document, indent=2, sort_keys=True) + "\n"


if dry_run:
    sys.stdout.write(render(redact(config)))
    print(
        f"agentteams-matrix openclaw adapter: dry run, no files written "
        f"(target: {config_path})",
        file=sys.stderr,
    )
    sys.exit(0)

write_atomic(config_path, render(config), config)
print(
    f"agentteams-matrix openclaw adapter: wrote {len(flat)} key(s) to "
    f"channels.matrix in {config_path}"
)
PY
