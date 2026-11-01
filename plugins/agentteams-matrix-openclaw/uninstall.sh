#!/usr/bin/env bash
# Uninstall the agentteams-matrix OpenClaw plugin. Idempotent.
#
# Usage: uninstall.sh [--purge]
#
# Steps:
#   1. openclaw plugins uninstall agentteams-matrix --force
#      (removes the managed install, plugins.entries policy, and allow/deny
#       entries; leaves an enabled:false marker so startup repair does not
#       re-enable it)
#   2. clear the active/previous release markers
#   3. with --purge: also delete channels.agentteams-matrix (and
#      plugins.entries.agentteams-matrix, if present) from the OpenClaw config
#
# The release directories under STATE_DIR/releases are KEPT after uninstall
# (a rollback target is only useful while the plugin is installed, but the
# directories are inert without the install record); remove them manually if
# you no longer need them.
#
# Environment: see scripts/lib-lifecycle.sh (state/config paths).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib-lifecycle.sh
source "${SCRIPT_DIR}/scripts/lib-lifecycle.sh"

lifecycle_require_cmd "$OPENCLAW_BIN"

purge=0
for arg in "$@"; do
  case "$arg" in
    --purge) purge=1 ;;
    *) lifecycle_fail "unknown argument: ${arg} (supported: --purge)" ;;
  esac
done

if lifecycle_is_listed; then
  lifecycle_log "Uninstalling ${PLUGIN_ID} from OpenClaw"
  # --force: skip the interactive confirmation prompt (scripts/CI).
  "$OPENCLAW_BIN" plugins uninstall "$PLUGIN_ID" --force \
    || lifecycle_fail "openclaw plugins uninstall failed"
else
  lifecycle_log "Plugin ${PLUGIN_ID} is not in the inventory; nothing to uninstall"
fi

lifecycle_write_marker "$ACTIVE_FILE" ""
lifecycle_write_marker "$PREVIOUS_FILE" ""

if [ "$purge" -eq 1 ]; then
  lifecycle_purge_channel_config
else
  lifecycle_log "Channel config kept (re-run with --purge to remove channels.${PLUGIN_ID})"
fi

lifecycle_log "Uninstall complete. Release directories kept under ${RELEASES_DIR}."
