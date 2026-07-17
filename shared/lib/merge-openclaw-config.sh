#!/bin/bash
# merge-openclaw-config.sh - Merge remote (MinIO) and local (Worker) openclaw.json
#
# Implementation: thin wrapper around the shared Python package
# ``agentteams_openclaw_merge`` (see shared/python/agentteams_openclaw_merge).
# Merge semantics are documented in that module's MERGE_RULES docstring.
#
# Usage (as sourced function):
#   source /opt/hiclaw/scripts/lib/merge-openclaw-config.sh
#   merge_openclaw_config <remote_path> <local_path> [<output_path>]
#
# If output_path is omitted, writes merged result to local_path.

merge_openclaw_config() {
    local remote_path="$1"
    local local_path="$2"
    local output_path="${3:-$local_path}"

    if ! command -v python3 >/dev/null 2>&1; then
        echo "merge_openclaw_config: python3 not found on PATH" >&2
        return 1
    fi

    python3 -m agentteams_openclaw_merge "${remote_path}" "${local_path}" "${output_path}"
}
