#!/usr/bin/env bash
# Install the agentteams-matrix OpenClaw plugin.
#
# Steps (idempotent — safe to re-run):
#   1. npm install + tsc build (skipped when a complete release for the
#      current version already exists)
#   2. materialize a self-contained release directory under
#      ${AGENTTEAMS_OPENCLAW_STATE_DIR:-~/.agentteams/openclaw-matrix}/releases/
#   3. openclaw plugins install --force --accept-capabilities <release-dir>
#      (registering the channel id first — the CLI rejects a config that
#      references an unknown channel, and a failed install must not leave
#      the channel section behind)
#   4. write channels.agentteams-matrix into the OpenClaw config
#      (enabled:true, socketPath, agent — env-driven, existing extra keys kept)
#   5. openclaw plugins enable agentteams-matrix
#   6. openclaw plugins reload agentteams-matrix --wait (best effort)
#   7. assert: openclaw plugins list --json + --enabled show the plugin
#
# Environment: see scripts/lib-lifecycle.sh (socket/agent/state/config paths).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib-lifecycle.sh
source "${SCRIPT_DIR}/scripts/lib-lifecycle.sh"

lifecycle_require_cmd "$OPENCLAW_BIN"

release_dir="$(lifecycle_build_release reuse)"
old_active="$(lifecycle_read_marker "$ACTIVE_FILE")"

lifecycle_install_release "$release_dir"

if [ -n "$old_active" ] && [ "$old_active" != "$release_dir" ]; then
  lifecycle_write_marker "$PREVIOUS_FILE" "$old_active"
fi
lifecycle_write_marker "$ACTIVE_FILE" "$release_dir"

lifecycle_log "Installed ${PLUGIN_ID} (release: ${release_dir})"
if [ -n "$old_active" ] && [ "$old_active" != "$release_dir" ]; then
  lifecycle_log "Previous release kept at: ${old_active} (rollback.sh restores it)"
fi
