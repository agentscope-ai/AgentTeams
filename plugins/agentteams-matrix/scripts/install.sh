#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUNDLE_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
export AGENTTEAMS_BUNDLE_DIR="${AGENTTEAMS_BUNDLE_DIR:-$BUNDLE_DIR}"
export AGENTTEAMS_PLUGIN_DIR="${AGENTTEAMS_PLUGIN_DIR:-$BUNDLE_DIR}"

ran=0

if command -v qwenpaw >/dev/null 2>&1; then
  bash "${BUNDLE_DIR}/adapters/qwenpaw/install.sh"
  ran=1
fi

if command -v hermes >/dev/null 2>&1; then
  bash "${BUNDLE_DIR}/adapters/hermes/install.sh"
  ran=1
fi

if [ "$ran" -eq 0 ]; then
  echo "ERROR: no supported Matrix channel runtime found; expected qwenpaw or hermes" >&2
  exit 1
fi
