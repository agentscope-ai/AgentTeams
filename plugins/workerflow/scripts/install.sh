#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PLUGIN_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
export WORKERFLOW_PLUGIN_DIR="${WORKERFLOW_PLUGIN_DIR:-$PLUGIN_DIR}"

if ! command -v qwenpaw >/dev/null 2>&1; then
  echo "ERROR: qwenpaw command not found" >&2
  exit 1
fi

bash "${PLUGIN_DIR}/adapters/qwenpaw/install.sh"
