#!/usr/bin/env bash
# Idempotent, file-level install of the Hermes Matrix platform plugin.
#
# The installer copies the plugin payload into the Hermes home
# (``$HERMES_HOME``, default ``$HOME/.hermes``) and records per-file
# content hashes in a marker:
#
#   ${HERMES_HOME}/.agentteams/agentteams-matrix/installed.json
#
# Re-running with unchanged content is a no-op: no file is rewritten and
# the marker is left untouched. Re-running after a payload change updates
# only the files whose content changed and removes the files the previous
# marker recorded that the new payload no longer contains (plus the
# directories that become empty), so the marker always describes exactly
# what the installer owns and uninstall never leaves an installer-owned
# file behind. Files the installer never wrote are never touched.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUNDLE_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
export AGENTTEAMS_BUNDLE_DIR="${AGENTTEAMS_BUNDLE_DIR:-$BUNDLE_DIR}"

HERMES_HOME="${HERMES_HOME:-$HOME/.hermes}"

python3 - "$SCRIPT_DIR" "$HERMES_HOME" <<'PY'
import hashlib
import json
import shutil
import sys
from pathlib import Path

adapter_dir = Path(sys.argv[1])
hermes_home = Path(sys.argv[2])
# The shared runtime-neutral core package at the bundle root. The
# adapter imports it from the plugin directory in the installed layout,
# so the installed plugin ships it under ``core/`` there.
bundle_root = adapter_dir.parents[1]
core_dir = bundle_root / "core"

PLUGIN_DIR = "agentteams-matrix"
BUNDLE_ID = "agentteams-matrix"
# Installer artifacts, not part of the runtime plugin payload.
EXCLUDE_FILES = {"install.sh", "uninstall.sh", "README.md"}
EXCLUDE_DIRS = {".agentteams", "__pycache__", "node_modules"}


def payload_files():
    for path in sorted(adapter_dir.rglob("*")):
        if not path.is_file():
            continue
        rel = path.relative_to(adapter_dir)
        if any(part in EXCLUDE_DIRS for part in rel.parts):
            continue
        if len(rel.parts) == 1 and rel.name in EXCLUDE_FILES:
            continue
        yield rel, adapter_dir
    # The core package ships next to the installed plugin so ``import core``
    # resolves from the plugin directory. rel is target-relative
    # (``core/...``), so its source root is the bundle root.
    if core_dir.is_dir():
        for path in sorted(core_dir.rglob("*")):
            if not path.is_file():
                continue
            rel = Path("core") / path.relative_to(core_dir)
            if any(part in EXCLUDE_DIRS for part in rel.parts):
                continue
            yield rel, bundle_root


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


target_dir = hermes_home / "plugins" / PLUGIN_DIR
marker_path = hermes_home / ".agentteams" / BUNDLE_ID / "installed.json"

payload = list(payload_files())
entries = {rel.as_posix(): file_sha256(src_root / rel) for rel, src_root in payload}
sources = {rel.as_posix(): src_root for rel, src_root in payload}
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
    print(f"agentteams-matrix hermes adapter: up to date ({target_dir})")
    sys.exit(0)

changed = 0
for rel, sha in sorted(entries.items()):
    src = sources[rel] / rel
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
    "runtime": "hermes",
    "pluginId": PLUGIN_DIR,
    "contentHash": expected,
    "files": entries,
}
marker_path.parent.mkdir(parents=True, exist_ok=True)
tmp = marker_path.with_name(marker_path.name + ".tmp")
tmp.write_text(json.dumps(marker, indent=2, sort_keys=True) + "\n", encoding="utf-8")
tmp.replace(marker_path)
if removed:
    print(
        f"agentteams-matrix hermes adapter: removed {removed} file(s) no longer in the payload from {target_dir}"
    )
print(f"agentteams-matrix hermes adapter: installed {changed} file(s) to {target_dir}")
PY
