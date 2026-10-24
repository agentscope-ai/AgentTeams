#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEAMHARNESS_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
BUILD_SCRIPT="${SCRIPT_DIR}/scripts/build-qwenpaw-plugin.rb"
export AGENTTEAMS_BUNDLE_DIR="${AGENTTEAMS_BUNDLE_DIR:-$TEAMHARNESS_DIR}"
export AGENTTEAMS_PLUGIN_DIR="${AGENTTEAMS_PLUGIN_DIR:-$TEAMHARNESS_DIR}"

if ! command -v qwenpaw >/dev/null 2>&1; then
  echo "ERROR: qwenpaw command not found" >&2
  exit 1
fi

WORKING_DIR="${QWENPAW_WORKING_DIR:-$HOME/.qwenpaw}"

# Content hash of exactly what build-qwenpaw-plugin.rb would package: the
# manifest, prompts, skills, mcp, the adapter entrypoints, and the version
# used in the generated plugin.json. When the marker matches this hash, the
# QwenPaw plugin is already current and the install is a no-op.
package_hash="$(python3 - "$TEAMHARNESS_DIR" <<'PY'
import hashlib
import sys
from pathlib import Path

root = Path(sys.argv[1])
EXCLUDED_DIRS = {"__pycache__", ".git"}

def source_files():
    for entry in ("plugin.yaml", "prompts", "skills", "mcp"):
        base = root / entry
        if base.is_file():
            yield base
            continue
        for path in sorted(base.rglob("*")):
            if path.is_file() and not any(p in EXCLUDED_DIRS for p in path.relative_to(root).parts) \
                    and path.name != ".DS_Store" and not path.name.endswith(".pyc"):
                yield path
    for entry in ("adapters/qwenpaw/plugin.py", "adapters/qwenpaw/task_trace.py"):
        yield root / entry

version = ""
for line in (root / "plugin.yaml").read_text(encoding="utf-8").splitlines():
    if line.strip() == "version:" or line.strip().startswith("version:"):
        version = line.split(":", 1)[1].strip().strip('"').strip("'")
        break

digest = hashlib.sha256()
digest.update(version.encode("utf-8"))
digest.update(b"\0")
for path in sorted(source_files(), key=lambda p: p.as_posix()):
    rel = path.relative_to(root).as_posix()
    digest.update(rel.encode("utf-8"))
    digest.update(b"\0")
    digest.update(path.read_bytes())
    digest.update(b"\0")
print(digest.hexdigest())
PY
)"

MARKER_FILE="${WORKING_DIR}/.agentteams/agentteams-teamharness/installed.json"
if [ -f "$MARKER_FILE" ]; then
  recorded_hash="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("contentHash",""))' "$MARKER_FILE" 2>/dev/null || true)"
  if [ "$recorded_hash" = "$package_hash" ]; then
    echo "teamharness qwenpaw adapter: up to date (${WORKING_DIR})"
    exit 0
  fi
fi

if ! command -v ruby >/dev/null 2>&1; then
  echo "ERROR: ruby is required to build the QwenPaw plugin package" >&2
  exit 1
fi

stage_dir="$(mktemp -d "${TMPDIR:-/tmp}/teamharness-qwenpaw-install.XXXXXX")"
cleanup() {
  rm -rf "$stage_dir"
}
trap cleanup EXIT

package_zip="$(OUT_DIR="${stage_dir}/dist" ruby "${BUILD_SCRIPT}" "${TEAMHARNESS_DIR}/plugin.yaml" | tail -n 1)"
unpack_dir="${stage_dir}/unpacked"
mkdir -p "$unpack_dir"

python3 - "$package_zip" "$unpack_dir" <<'PY'
import sys
import zipfile

with zipfile.ZipFile(sys.argv[1]) as archive:
    archive.extractall(sys.argv[2])
PY

package_dir="$(find "$unpack_dir" -mindepth 1 -maxdepth 1 -type d | head -n 1)"
if [ -z "$package_dir" ] || [ ! -f "$package_dir/plugin.json" ]; then
  echo "ERROR: generated QwenPaw plugin package is invalid" >&2
  exit 1
fi

qwenpaw plugin install "$package_dir" --force

python3 - "$MARKER_FILE" "$package_hash" "$TEAMHARNESS_DIR" <<'PY'
import json
import sys
from pathlib import Path

marker_path = Path(sys.argv[1])
package_hash = sys.argv[2]
root = Path(sys.argv[3])

version = ""
for line in (root / "plugin.yaml").read_text(encoding="utf-8").splitlines():
    if line.strip().startswith("version:"):
        version = line.split(":", 1)[1].strip().strip('"').strip("'")
        break

marker = {
    "schemaVersion": 1,
    "bundle": "agentteams-teamharness",
    "runtime": "qwenpaw",
    "pluginId": "teamharness",
    "version": version,
    "contentHash": package_hash,
}
marker_path.parent.mkdir(parents=True, exist_ok=True)
tmp = marker_path.with_name(marker_path.name + ".tmp")
tmp.write_text(json.dumps(marker, indent=2, sort_keys=True) + "\n", encoding="utf-8")
tmp.replace(marker_path)
PY

log_file="${TEAMHARNESS_INSTALL_LOG:-}"
if [ -n "$log_file" ]; then
  mkdir -p "$(dirname "$log_file")"
  printf '{"event":"install","runtime":"qwenpaw","pluginDir":"%s"}\n' "${AGENTTEAMS_PLUGIN_DIR:-${PWD}}" >> "$log_file"
fi
