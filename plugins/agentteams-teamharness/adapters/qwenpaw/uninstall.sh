#!/usr/bin/env bash
set -euo pipefail

WORKING_DIR="${QWENPAW_WORKING_DIR:-$HOME/.qwenpaw}"

# The QwenPaw-owned plugin files are removed by the QwenPaw CLI itself; the
# installer's own footprint is the state marker, which is removed here so a
# re-run leaves no residue.
if command -v qwenpaw >/dev/null 2>&1; then
  printf 'y\n' | qwenpaw plugin uninstall teamharness >/dev/null 2>&1 || true
fi

python3 - "$WORKING_DIR" <<'PY'
import sys
from pathlib import Path

working_dir = Path(sys.argv[1])
state_root = working_dir / ".agentteams"
state_dir = state_root / "teamharness"
marker_path = state_dir / "installed.json"

if marker_path.is_file():
    marker_path.unlink()
if state_dir.is_dir() and not any(state_dir.iterdir()):
    state_dir.rmdir()
if state_root.is_dir() and not any(state_root.iterdir()):
    state_root.rmdir()
PY

log_file="${TEAMHARNESS_INSTALL_LOG:-}"
if [ -n "$log_file" ]; then
  mkdir -p "$(dirname "$log_file")"
  printf '{"event":"uninstall","runtime":"qwenpaw","pluginDir":"%s"}\n' "${AGENTTEAMS_PLUGIN_DIR:-${PWD}}" >> "$log_file"
fi
