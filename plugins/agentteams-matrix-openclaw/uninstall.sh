#!/usr/bin/env bash
# Uninstall the agentteams-matrix OpenClaw plugin. Idempotent, and
# fail-closed on an unreadable plugin inventory.
#
# Usage: uninstall.sh [--purge]
#
# Steps:
#   1. read the cold inventory ('openclaw plugins list --json'):
#        - unreadable  -> ABORT (non-zero exit), release markers kept.
#          An unreadable inventory says nothing about the install; clearing
#          the markers on top of it would leave the plugin installed but
#          unmanaged.
#        - listed      -> openclaw plugins uninstall agentteams-matrix
#          --force (removes the managed install, plugins.entries policy, and
#          allow/deny entries; leaves an enabled:false marker so startup
#          repair does not re-enable it)
#        - absent      -> nothing to uninstall (absence is established)
#   2. clear the active/previous release markers — only after step 1 has
#      established absence or removed the plugin
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

# The inventory answer is three-valued (see lifecycle_is_listed):
# listed / absent / unreadable. An unreadable inventory must fail the
# uninstall — "nothing to uninstall" is only true once absence is
# established, and clearing the release markers on top of an inventory
# error would leave the plugin installed but unmanaged (the marker loss
# would also break any later rollback target).
listed_rc=0
lifecycle_is_listed || listed_rc=$?
if [ "$listed_rc" -eq 2 ]; then
  lifecycle_fail "cannot read the plugin inventory ('openclaw plugins list --json' failed); the active/previous release markers are kept — repair the OpenClaw config/state and re-run"
fi

if [ "$listed_rc" -eq 0 ]; then
  lifecycle_log "Uninstalling ${PLUGIN_ID} from OpenClaw"
  # --force: skip the interactive confirmation prompt (scripts/CI).
  "$OPENCLAW_BIN" plugins uninstall "$PLUGIN_ID" --force \
    || lifecycle_fail "openclaw plugins uninstall failed"
else
  lifecycle_log "Plugin ${PLUGIN_ID} is not in the inventory; nothing to uninstall"
fi

# Reached only when absence is established (no-op path) or after a
# successful authoritative removal — never on top of an inventory error.
lifecycle_write_marker "$ACTIVE_FILE" ""
lifecycle_write_marker "$PREVIOUS_FILE" ""

if [ "$purge" -eq 1 ]; then
  lifecycle_purge_channel_config
else
  lifecycle_log "Channel config kept (re-run with --purge to remove channels.${PLUGIN_ID})"
fi

if [ -f "${STATE_DIR}/sidecar-config.json" ]; then
  lifecycle_log "Sidecar config kept at ${STATE_DIR}/sidecar-config.json (it holds your credentials; remove it by hand if you no longer run the sidecar)"
fi

lifecycle_log "Uninstall complete. Release directories kept under ${RELEASES_DIR}."
