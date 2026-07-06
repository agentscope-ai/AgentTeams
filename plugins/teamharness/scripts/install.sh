#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PLUGIN_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
export AGENTTEAMS_PLUGIN_DIR="${AGENTTEAMS_PLUGIN_DIR:-$PLUGIN_DIR}"

ran=0

if command -v qwenpaw >/dev/null 2>&1; then
  bash "${PLUGIN_DIR}/adapters/qwenpaw/install.sh"
  ran=1
fi

if [ "$ran" -eq 0 ]; then
  echo "ERROR: no supported TeamHarness local runtime found; expected qwenpaw" >&2
  exit 1
fi
