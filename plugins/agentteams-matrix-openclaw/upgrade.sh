#!/usr/bin/env bash
# Upgrade the agentteams-matrix OpenClaw plugin from the current source.
#
# Always rebuilds and materializes a NEW release directory (never reuses the
# active one), installs it, and keeps the previously active release on disk
# as the rollback target (STATE_DIR/previous).
#
# Steps:
#   1. npm install + tsc build
#   2. materialize a fresh release directory
#   3. write channels.agentteams-matrix into the OpenClaw config
#   4. openclaw plugins install --force --accept-capabilities <new-release>
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

release_dir="$(lifecycle_build_release fresh)"
old_active="$(lifecycle_read_marker "$ACTIVE_FILE")"

lifecycle_install_release "$release_dir"

if [ -n "$old_active" ] && [ "$old_active" != "$release_dir" ]; then
  lifecycle_write_marker "$PREVIOUS_FILE" "$old_active"
else
  lifecycle_write_marker "$PREVIOUS_FILE" ""
fi
lifecycle_write_marker "$ACTIVE_FILE" "$release_dir"

lifecycle_log "Upgraded ${PLUGIN_ID} (release: ${release_dir})"
if [ -n "$old_active" ] && [ "$old_active" != "$release_dir" ]; then
  lifecycle_log "Previous release kept at: ${old_active} (rollback.sh restores it)"
else
  lifecycle_log "No previous release to keep (first tracked install)."
fi
