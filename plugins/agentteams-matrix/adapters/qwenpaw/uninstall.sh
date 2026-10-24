#!/usr/bin/env bash
# Idempotent uninstall for the QwenPaw Matrix channel plugin.
#
# Removes exactly the files recorded in the installer marker, then prunes the
# directories that become empty (the plugin directory itself only when it is
# empty) and the bundle's installer state. Without a marker the uninstall is
# a no-op, so re-running it is always safe and leaves no residue.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUNDLE_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
export AGENTTEAMS_BUNDLE_DIR="${AGENTTEAMS_BUNDLE_DIR:-$BUNDLE_DIR}"

WORKING_DIR="${QWENPAW_WORKING_DIR:-$HOME/.qwenpaw}"

python3 - "$WORKING_DIR" <<'PY'
import json
import sys
from pathlib import Path

working_dir = Path(sys.argv[1])

PLUGIN_ID = "agentteams-matrix-channel"
BUNDLE_ID = "agentteams-matrix"
state_root = working_dir / ".agentteams"
state_dir = state_root / BUNDLE_ID
marker_path = state_dir / "installed.json"
target_dir = working_dir / "plugins" / PLUGIN_ID

if not marker_path.is_file():
    print("agentteams-matrix qwenpaw adapter: nothing to uninstall")
    sys.exit(0)

try:
    marker = json.loads(marker_path.read_text(encoding="utf-8"))
except ValueError:
    marker = {}

removed = 0
for rel in sorted(marker.get("files", {})):
    path = target_dir / rel
    if path.is_file():
        path.unlink()
        removed += 1

# Prune directories that became empty, deepest first. Never remove a
# directory that still holds files the installer did not write.
def prune_empty_dirs(root):
    for directory in sorted(root.rglob("*"), reverse=True):
        if directory.is_dir():
            try:
                directory.rmdir()
            except OSError:
                continue

prune_empty_dirs(target_dir)
if target_dir.is_dir() and not any(target_dir.iterdir()):
    target_dir.rmdir()
marker_path.unlink(missing_ok=True)
if state_dir.is_dir() and not any(state_dir.iterdir()):
    state_dir.rmdir()
if state_root.is_dir() and not any(state_root.iterdir()):
    state_root.rmdir()
print(f"agentteams-matrix qwenpaw adapter: removed {removed} file(s) from {target_dir}")
PY
