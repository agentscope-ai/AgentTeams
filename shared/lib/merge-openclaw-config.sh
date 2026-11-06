#!/bin/bash
# merge-openclaw-config.sh - Merge remote (MinIO) and local (Worker) openclaw.json
#
# Design principle (local-first):
#   Local (Worker disk) is the authoritative base. Periodic pulls from MinIO only
#   overlay Manager-managed slices so the Worker keeps its own customizations.
#   Remote (MinIO/Manager) overwrites only: models, gateway, and channels (deep
#   merge where remote wins on conflicting keys).
#   All other top-level fields (tools, agents, mcp, etc.) stay from local.
#   Merge rules:
#     - plugins.entries: deep merge — remote provides base/defaults, local wins
#       on shared keys so user customizations (e.g. memory-core dreaming schedule)
#       survive periodic syncs
#     - plugins.load.paths: union of both sides
#     - channels: deep merge (remote wins shared keys, local-only keys preserved)
#     - channels.matrix.accessToken: local wins (Worker re-login)
#     - memory.search: remote wins per key; local-only keys preserved
#     - legacy v2026.10.6 keys are migrated after merge (see
#       migrate-openclaw-config.sh)
#
# Usage (as sourced function):
#   source /opt/agentteams/scripts/lib/merge-openclaw-config.sh
#   merge_openclaw_config <remote_path> <local_path> [<output_path>]
#
# If output_path is omitted, writes merged result to local_path.

if [ -n "${BASH_SOURCE:-}" ]; then
    _MERGE_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
else
    # agentteams-sync.sh is /bin/sh and sources this file from the image path;
    # BASH_SOURCE is unavailable there.
    _MERGE_LIB_DIR=/opt/agentteams/scripts/lib
fi
. "${_MERGE_LIB_DIR}/migrate-openclaw-config.sh"

merge_openclaw_config() {
    local remote_path="$1"
    local local_path="$2"
    local output_path="${3:-$local_path}"

    if [ ! -f "${remote_path}" ]; then
        # No remote version; still migrate the local config in place so a
        # pre-v2026.10.6 workspace does not keep rejected keys forever.
        migrate_openclaw_config "${local_path}" "${output_path}" || return 1
        return 0
    fi

    if [ ! -f "${local_path}" ]; then
        # No local version, use remote directly (after canonicalization).
        migrate_openclaw_config "${remote_path}" "${output_path}" || return 1
        return 0
    fi

    local merged
    if ! merged=$(jq -n --slurpfile remote_file "${remote_path}" --slurpfile local_file "${local_path}" '
        ($remote_file[0]) as $remote
        | ($local_file[0]) as $local
        |
        $local
        | if ($remote.models // null) != null then .models = $remote.models else . end
        | if ($remote.gateway // null) != null then .gateway = $remote.gateway else . end
        | if ($remote.channels // null) != null or ($local.channels // null) != null then
            .channels = (($local.channels // {}) * ($remote.channels // {}))
          else . end
        | if ($local.channels.matrix.accessToken // null) != null then
            .channels.matrix.accessToken = $local.channels.matrix.accessToken
          else . end
        | if (($remote.memory.search? // null) != null) then
            .memory = ((.memory // {}) | .search = (((.search // {}) | if type == "object" then . else {} end) * ($remote.memory.search | if type == "object" then . else {} end)))
          else . end
        | if ($remote.plugins // null) != null or ($local.plugins // null) != null then
            .plugins = (
              ($local.plugins // {})
              | if ($remote.plugins.entries // null) != null or ($local.plugins.entries // null) != null then
                  .entries = (($remote.plugins.entries // {}) * ($local.plugins.entries // {}))
                else . end
              | if ($remote.plugins.load.paths // null) != null or ($local.plugins.load.paths // null) != null then
                  .load = ((.load // {}) | .paths = ([($remote.plugins.load.paths // [])[], ($local.plugins.load.paths // [])[]] | unique))
                else . end
            )
          else . end
    ' 2>/dev/null); then
        # Keep local unchanged and let the caller decide whether to retry.
        return 1
    fi

    [ -n "${merged}" ] || return 1
    printf '%s\n' "${merged}" > "${output_path}"
    migrate_openclaw_config "${output_path}" || return 1
}
