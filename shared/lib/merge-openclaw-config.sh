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
#
# Usage (as sourced function):
#   source /opt/agentteams/scripts/lib/merge-openclaw-config.sh
#   merge_openclaw_config <remote_path> <local_path> [<output_path>]
#
# If output_path is omitted, writes merged result to local_path.

merge_openclaw_config() {
    local remote_path="$1"
    local local_path="$2"
    local output_path="${3:-$local_path}"

    if [ ! -f "${remote_path}" ]; then
        # No remote version, keep local as-is
        return 0
    fi

    if [ ! -f "${local_path}" ]; then
        # No local version, use remote directly
        mv "${remote_path}" "${output_path}"
        return 0
    fi

    local merged
    merged=$(jq -n --argfile remote "${remote_path}" --argfile local "${local_path}" '
        $local
        | if ($remote.models // null) != null then .models = $remote.models else . end
        | if ($remote.gateway // null) != null then .gateway = $remote.gateway else . end
        | if ($remote.channels // null) != null or ($local.channels // null) != null then
            .channels = (($local.channels // {}) * ($remote.channels // {}))
          else . end
        | if ($local.channels.matrix.accessToken // null) != null then
            .channels.matrix.accessToken = $local.channels.matrix.accessToken
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
    ' 2>/dev/null)

    if [ $? -eq 0 ] && [ -n "${merged}" ]; then
        echo "${merged}" > "${output_path}"
    else
        # jq merge failed — keep local (do not replace with remote)
        :
    fi
}
