#!/usr/bin/env bash
# Roll the agentteams-matrix OpenClaw plugin back to the previous release.
#
# The rollback target is the release directory recorded in STATE_DIR/previous
# by the last install.sh/upgrade.sh. It is already built (self-contained,
# including node_modules), so no npm/tsc step is needed here.
#
# Steps:
#   1. read STATE_DIR/previous (fail clearly when absent)
#   2. write channels.agentteams-matrix into the OpenClaw config
#   3. openclaw plugins install --force --accept-capabilities <previous-release>
#   4. openclaw plugins enable agentteams-matrix
#   5. openclaw plugins reload agentteams-matrix --wait (best effort)
#   6. assert: openclaw plugins list --json + --enabled show the plugin
#
# After rollback, STATE_DIR/active points at the previous release and
# STATE_DIR/previous is cleared; the superseded release directory is kept on
# disk (re-run upgrade.sh to move forward again).
#
# Environment: see scripts/lib-lifecycle.sh (socket/agent/state/config paths).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib-lifecycle.sh
source "${SCRIPT_DIR}/scripts/lib-lifecycle.sh"

lifecycle_require_cmd "$OPENCLAW_BIN"

prev_release="$(lifecycle_read_marker "$PREVIOUS_FILE")"
if [ -z "$prev_release" ]; then
  lifecycle_fail "no previous release recorded (${PREVIOUS_FILE}); nothing to roll back to"
fi
if [ ! -d "$prev_release" ]; then
  lifecycle_fail "previous release directory missing on disk: ${prev_release}"
fi

current_active="$(lifecycle_read_marker "$ACTIVE_FILE")"

lifecycle_install_release "$prev_release"

lifecycle_write_marker "$ACTIVE_FILE" "$prev_release"
lifecycle_write_marker "$PREVIOUS_FILE" ""

lifecycle_log "Rolled back ${PLUGIN_ID} to release: ${prev_release}"
if [ -n "$current_active" ] && [ "$current_active" != "$prev_release" ]; then
  lifecycle_log "Superseded release kept at: ${current_active} (upgrade.sh moves forward again)"
fi
