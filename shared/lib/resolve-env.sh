#!/bin/bash
# resolve-env.sh - Dual-prefix environment variable resolution for HiClaw → AgentTeams rename
#
# Provides resolve_env() which reads AGENTTEAMS_* first, falls back to HICLAW_*.
# During the transition period, prints a one-time deprecation warning per variable
# when only the old HICLAW_* prefix is set.
#
# Usage:
#   source /opt/hiclaw/scripts/lib/resolve-env.sh
#   val=$(resolve_env "LLM_API_KEY" "default_value")
#   # Checks AGENTTEAMS_LLM_API_KEY, then HICLAW_LLM_API_KEY, then uses default

declare -A _RESOLVE_ENV_WARNED 2>/dev/null || true

resolve_env() {
    local suffix="$1"
    local default="${2:-}"
    local new_key="AGENTTEAMS_${suffix}"
    local old_key="HICLAW_${suffix}"
    local new_val="${!new_key:-}"
    local old_val="${!old_key:-}"

    if [ -n "$new_val" ]; then
        echo "$new_val"
        return
    fi

    if [ -n "$old_val" ]; then
        if [ -z "${_RESOLVE_ENV_WARNED[$old_key]:-}" ]; then
            echo "[DEPRECATED] $old_key is deprecated, use $new_key instead" >&2
            _RESOLVE_ENV_WARNED[$old_key]=1
        fi
        echo "$old_val"
        return
    fi

    echo "$default"
}

resolve_env_export() {
    local suffix="$1"
    local default="${2:-}"
    local new_key="AGENTTEAMS_${suffix}"
    local val
    val=$(resolve_env "$suffix" "$default")
    export "$new_key"="$val"
    export "HICLAW_${suffix}"="$val"
}
