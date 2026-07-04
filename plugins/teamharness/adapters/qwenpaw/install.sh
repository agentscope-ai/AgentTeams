#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEAMHARNESS_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
BUILD_SCRIPT="${SCRIPT_DIR}/scripts/build-qwenpaw-plugin.rb"

if ! command -v qwenpaw >/dev/null 2>&1; then
  echo "ERROR: qwenpaw command not found" >&2
  exit 1
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

log_file="${TEAMHARNESS_INSTALL_LOG:-}"
if [ -n "$log_file" ]; then
  mkdir -p "$(dirname "$log_file")"
  printf '{"event":"install","runtime":"qwenpaw","pluginDir":"%s"}\n' "${AGENTTEAMS_PLUGIN_DIR:-${PWD}}" >> "$log_file"
fi
