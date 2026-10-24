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
# the files whose content changed.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUNDLE_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
export AGENTTEAMS_BUNDLE_DIR="${AGENTTEAMS_BUNDLE_DIR:-$BUNDLE_DIR}"
export AGENTTEAMS_PLUGIN_DIR="${AGENTTEAMS_PLUGIN_DIR:-$BUNDLE_DIR}"

WORKING_DIR="${QWENPAW_WORKING_DIR:-$HOME/.qwenpaw}"

python3 - "$SCRIPT_DIR" "$WORKING_DIR" <<'PY'
import hashlib
import json
import shutil
import sys
from pathlib import Path

adapter_dir = Path(sys.argv[1])
working_dir = Path(sys.argv[2])

PLUGIN_ID = "agentteams-matrix-channel"
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

marker = {
    "schemaVersion": 1,
    "bundle": BUNDLE_ID,
    "runtime": "qwenpaw",
    "pluginId": PLUGIN_ID,
    "contentHash": expected,
    "files": entries,
}
marker_path.parent.mkdir(parents=True, exist_ok=True)
tmp = marker_path.with_name(marker_path.name + ".tmp")
tmp.write_text(json.dumps(marker, indent=2, sort_keys=True) + "\n", encoding="utf-8")
tmp.replace(marker_path)
print(f"agentteams-matrix qwenpaw adapter: installed {changed} file(s) to {target_dir}")
PY
