#!/usr/bin/env bash
# install-plugin.sh - Shared QwenPaw plugin install for TeamHarness and WorkerFlow adapters
#
# Usage:
#   install-plugin.sh <PLUGIN_ROOT>
#
# PLUGIN_ROOT is the plugin package root (e.g. plugins/teamharness or plugins/workerflow).

set -euo pipefail

PLUGIN_ROOT="${1:?plugin root directory required}"
ADAPTER_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${ADAPTER_DIR}/../../.." && pwd)"
BUILD_SCRIPT="${PLUGIN_ROOT}/adapters/qwenpaw/scripts/build-qwenpaw-plugin.rb"
PLUGIN_YAML="${PLUGIN_ROOT}/plugin.yaml"
INSTALL_LOG="${AGENTTEAMS_PLUGIN_INSTALL_LOG:-}"

if ! command -v qwenpaw >/dev/null 2>&1; then
  echo "ERROR: qwenpaw command not found" >&2
  exit 1
fi

if ! command -v ruby >/dev/null 2>&1; then
  echo "ERROR: ruby is required to build the QwenPaw plugin package" >&2
  exit 1
fi

if [ ! -f "${BUILD_SCRIPT}" ] || [ ! -f "${PLUGIN_YAML}" ]; then
  echo "ERROR: missing QwenPaw adapter build inputs under ${PLUGIN_ROOT}" >&2
  exit 1
fi

stage_dir="$(mktemp -d "${TMPDIR:-/tmp}/qwenpaw-plugin-install.XXXXXX")"
cleanup() {
  rm -rf "$stage_dir"
}
trap cleanup EXIT

package_zip="$(OUT_DIR="${stage_dir}/dist" ruby "${BUILD_SCRIPT}" "${PLUGIN_YAML}" | tail -n 1)"
unpack_dir="${stage_dir}/unpacked"
mkdir -p "$unpack_dir"

export PYTHONPATH="${REPO_ROOT}/qwenpaw/src:${PYTHONPATH:-}"
package_dir="$(python3 -m qwenpaw_worker.plugin_install extract "$package_zip" "$unpack_dir")"

if [ -z "$package_dir" ] || [ ! -f "$package_dir/plugin.json" ]; then
  echo "ERROR: generated QwenPaw plugin package is invalid" >&2
  exit 1
fi

qwenpaw plugin install "$package_dir" --force

if [ -n "$INSTALL_LOG" ]; then
  mkdir -p "$(dirname "$INSTALL_LOG")"
  printf '{"event":"install","runtime":"qwenpaw","pluginDir":"%s","pluginRoot":"%s"}\n' \
    "${AGENTTEAMS_PLUGIN_DIR:-${PWD}}" "${PLUGIN_ROOT}" >> "$INSTALL_LOG"
fi
