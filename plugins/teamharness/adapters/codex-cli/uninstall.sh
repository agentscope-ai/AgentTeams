#!/usr/bin/env bash
set -euo pipefail

project_dir="${AGENTTEAMS_PROJECT_DIR:-${PWD}}"
marker="${project_dir}/.agentteams/codex-cli/adapter.json"
if [ -f "$marker" ]; then
  rm -f -- "$marker"
fi

log_file="${TEAMHARNESS_INSTALL_LOG:-}"
if [ -n "$log_file" ]; then
  mkdir -p "$(dirname "$log_file")"
  printf '{"event":"uninstall","runtime":"codex-cli","pluginDir":"%s"}\n' "${AGENTTEAMS_PLUGIN_DIR:-${PWD}}" >> "$log_file"
fi
