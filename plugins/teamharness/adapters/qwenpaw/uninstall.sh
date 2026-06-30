#!/usr/bin/env bash
set -euo pipefail

if command -v qwenpaw >/dev/null 2>&1; then
  printf 'y\n' | qwenpaw plugin uninstall teamharness >/dev/null 2>&1 || true
fi

log_file="${TEAMHARNESS_INSTALL_LOG:-}"
if [ -n "$log_file" ]; then
  mkdir -p "$(dirname "$log_file")"
  printf '{"event":"uninstall","runtime":"qwenpaw","pluginDir":"%s"}\n' "${AGENTTEAMS_PLUGIN_DIR:-${PWD}}" >> "$log_file"
fi
