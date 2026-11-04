#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: install.sh [--dry-run]

  --dry-run  Print the planned install on stdout; change nothing
EOF
}

DRY_RUN=0
while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "ERROR: unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

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

if [ "$DRY_RUN" -eq 1 ]; then
  printf '{"event":"install","runtime":"qwenpaw","dryRun":true,"pluginDir":"%s"}\n' \
    "${WORKING_DIR}/plugins/teamharness"
  echo "qwenpaw adapter: dry-run, no files changed" >&2
  exit 0
fi

# Content hash of exactly what build-qwenpaw-plugin.rb would package: the
# manifest, prompts, skills, mcp, the adapter entrypoints, and the version
# used in the generated plugin.json. The no-op path requires the marker to
# match this hash and every recorded installed file to exist in the plugin
# directory unchanged; otherwise the plugin is rebuilt and reinstalled.
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

# The QwenPaw CLI installs a package under ${WORKING_DIR}/plugins/<plugin id>
# (verified against the real CLI, which reports the install location as
# <working dir>/plugins/teamharness). Native `qwenpaw plugin uninstall
# teamharness` removes this directory but keeps the marker, so the no-op
# check must verify the installed payload on disk, not only the marker.
PLUGIN_DIR="${WORKING_DIR}/plugins/teamharness"
MARKER_FILE="${WORKING_DIR}/.agentteams/teamharness/installed.json"
if [ -f "$MARKER_FILE" ]; then
  up_to_date="$(python3 - "$MARKER_FILE" "$package_hash" "$PLUGIN_DIR" <<'PY'
import hashlib
import json
import sys
from pathlib import Path

marker_path, package_hash, plugin_dir = Path(sys.argv[1]), sys.argv[2], Path(sys.argv[3])
try:
    marker = json.loads(marker_path.read_text(encoding="utf-8"))
except (OSError, ValueError):
    print("no")
    sys.exit(0)
if not isinstance(marker, dict) or marker.get("contentHash") != package_hash:
    print("no")
    sys.exit(0)
# A marker without the installed file set predates the files field: treat
# it as stale so one reinstall brings the marker up to date.
files = marker.get("files")
if not isinstance(files, dict) or not files:
    print("no")
    sys.exit(0)
if not (plugin_dir / "plugin.json").is_file():
    print("no")
    sys.exit(0)
for rel, sha in files.items():
    path = plugin_dir / rel
    if not path.is_file() or hashlib.sha256(path.read_bytes()).hexdigest() != sha:
        print("no")
        sys.exit(0)
print("yes")
PY
)"
  if [ "$up_to_date" = "yes" ]; then
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

python3 - "$MARKER_FILE" "$package_hash" "$TEAMHARNESS_DIR" "$package_dir" "$PLUGIN_DIR" <<'PY'
import hashlib
import json
import sys
from pathlib import Path

marker_path = Path(sys.argv[1])
package_hash = sys.argv[2]
root = Path(sys.argv[3])
package_dir = Path(sys.argv[4])
plugin_dir = Path(sys.argv[5])

version = ""
for line in (root / "plugin.yaml").read_text(encoding="utf-8").splitlines():
    if line.strip().startswith("version:"):
        version = line.split(":", 1)[1].strip().strip('"').strip("'")
        break

# The installed file set, keyed by path relative to the plugin directory.
# The no-op check requires every entry to exist with this exact hash, so a
# plugin removed out-of-band (native uninstall) forces a reinstall.
files = {
    path.relative_to(package_dir).as_posix(): hashlib.sha256(path.read_bytes()).hexdigest()
    for path in sorted(package_dir.rglob("*"))
    if path.is_file()
}

marker = {
    "schemaVersion": 1,
    "bundle": "agentteams-teamharness",
    "runtime": "qwenpaw",
    "pluginId": "teamharness",
    "version": version,
    "contentHash": package_hash,
    "pluginDir": str(plugin_dir.resolve()),
    "files": files,
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
