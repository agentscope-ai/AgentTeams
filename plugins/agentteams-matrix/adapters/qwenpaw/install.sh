#!/usr/bin/env bash
# Idempotent, file-level install of the QwenPaw Matrix channel plugin.
#
# The installer copies the plugin payload into the QwenPaw working directory
# and records per-file content hashes in a marker:
#
#   ${QWENPAW_WORKING_DIR}/.agentteams/agentteams-matrix/installed.json
#
# Re-running with unchanged content is a no-op: no file is rewritten and the
# marker is left untouched. Re-running after a payload change updates only
# the files whose content changed and removes the files the previous marker
# recorded that the new payload no longer contains (plus the directories
# that become empty), so the marker always describes exactly what the
# installer owns and uninstall never leaves an installer-owned file behind.
# Files the installer never wrote are never touched.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUNDLE_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
export AGENTTEAMS_BUNDLE_DIR="${AGENTTEAMS_BUNDLE_DIR:-$BUNDLE_DIR}"
export AGENTTEAMS_PLUGIN_DIR="${AGENTTEAMS_PLUGIN_DIR:-$BUNDLE_DIR}"

WORKING_DIR="${QWENPAW_WORKING_DIR:-$HOME/.qwenpaw}"

python3 - "$SCRIPT_DIR" "$WORKING_DIR" <<'PY'
import hashlib
import json
import os
import shutil
import stat
import sys
import tempfile
from pathlib import Path

adapter_dir = Path(sys.argv[1])
working_dir = Path(sys.argv[2])

PLUGIN_ID = "agentteams-matrix-channel"
BUNDLE_ID = "agentteams-matrix"
# Installer artifacts, not part of the runtime plugin payload.
EXCLUDE_FILES = {"install.sh", "uninstall.sh", "README.md"}
EXCLUDE_DIRS = {".agentteams", "__pycache__", "node_modules"}

CREDENTIAL_SUFFIXES = ("token", "secret", "password", "passwd", "apikey", "credentials")


def _is_credential_key(key):
    normalized = "".join(ch for ch in str(key).lower() if ch.isalnum())
    return normalized.endswith(CREDENTIAL_SUFFIXES)


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


def payload_files():
    for path in sorted(adapter_dir.rglob("*")):
        if not path.is_file():
            continue
        rel = path.relative_to(adapter_dir)
        if any(part in EXCLUDE_DIRS for part in rel.parts):
            continue
        if len(rel.parts) == 1 and rel.name in EXCLUDE_FILES:
            continue
        yield rel


def file_sha256(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def content_hash(entries):
    digest = hashlib.sha256()
    for rel in sorted(entries):
        digest.update(rel.encode("utf-8"))
        digest.update(b"\0")
        digest.update(entries[rel].encode("utf-8"))
        digest.update(b"\0")
    return digest.hexdigest()


target_dir = working_dir / "plugins" / PLUGIN_ID
marker_path = working_dir / ".agentteams" / BUNDLE_ID / "installed.json"

entries = {rel.as_posix(): file_sha256(adapter_dir / rel) for rel in payload_files()}
expected = content_hash(entries)

installed = None
if marker_path.is_file():
    try:
        installed = json.loads(marker_path.read_text(encoding="utf-8"))
    except ValueError:
        installed = None

# Files the previous install owned, used to reconcile files the new payload
# no longer contains. A missing or malformed field means nothing is known
# to be owned, so nothing is removed.
previous = installed.get("files") if isinstance(installed, dict) else None
if not isinstance(previous, dict):
    previous = {}

up_to_date = (
    installed is not None
    and installed.get("contentHash") == expected
    and installed.get("files") == entries
    and all(
        (target_dir / rel).is_file() and file_sha256(target_dir / rel) == sha
        for rel, sha in entries.items()
    )
)
if up_to_date:
    print(f"agentteams-matrix qwenpaw adapter: up to date ({target_dir})")
    sys.exit(0)

changed = 0
for rel, sha in sorted(entries.items()):
    src = adapter_dir / rel
    dst = target_dir / rel
    dst.parent.mkdir(parents=True, exist_ok=True)
    if dst.is_file() and file_sha256(dst) == sha:
        continue
    # copy2 preserves the source mtime, so unchanged files keep a stable
    # timestamp even when rewritten.
    shutil.copy2(src, dst)
    changed += 1

# Reconcile ownership: a file the previous marker recorded but the new
# payload no longer contains is installer-owned residue — remove it (and
# prune the directories that become empty) so the new marker, which
# records only the current payload, stays complete for uninstall. Only
# paths the previous marker recorded are ever removed; user files are
# untouched.
removed = 0
for rel in sorted(set(previous) - set(entries)):
    path = target_dir / rel
    if not path.is_file():
        continue
    path.unlink()
    removed += 1
    parts = Path(rel).parts
    for depth in range(len(parts) - 1, 0, -1):
        try:
            target_dir.joinpath(*parts[:depth]).rmdir()
        except OSError:
            # The directory still holds files (installer or user) or is
            # gone; either way there is nothing to prune.
            pass

marker = {
    "schemaVersion": 1,
    "bundle": BUNDLE_ID,
    "runtime": "qwenpaw",
    "pluginId": PLUGIN_ID,
    "contentHash": expected,
    "files": entries,
}
write_atomic(
    marker_path,
    json.dumps(marker, indent=2, sort_keys=True) + "\n",
    marker,
)
if removed:
    print(
        f"agentteams-matrix qwenpaw adapter: removed {removed} file(s) no longer in the payload from {target_dir}"
    )
print(f"agentteams-matrix qwenpaw adapter: installed {changed} file(s) to {target_dir}")
PY
