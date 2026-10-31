#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUNDLE_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
export AGENTTEAMS_BUNDLE_DIR="${AGENTTEAMS_BUNDLE_DIR:-$BUNDLE_DIR}"

if command -v qwenpaw >/dev/null 2>&1; then
  bash "${BUNDLE_DIR}/adapters/qwenpaw/uninstall.sh"
fi

if command -v hermes >/dev/null 2>&1; then
  bash "${BUNDLE_DIR}/adapters/hermes/uninstall.sh"
fi
