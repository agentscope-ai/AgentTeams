#!/bin/bash
# migrate-openclaw-config.sh - canonicalize AgentTeams openclaw.json keys.
#
# OpenClaw v2026.10.6 retired several keys that older AgentTeams templates
# emitted.  The same migration is implemented in manager sync.py; keep the
# two filters in sync.
#
# Usage:
#   source /opt/agentteams/scripts/lib/migrate-openclaw-config.sh
#   migrate_openclaw_config <input.json> [output.json]
#
# On jq failure the input is left untouched and the function returns 1.

migrate_openclaw_config() {
    local input="$1"
    local output="${2:-$1}"
    local tmp
    [ -f "${input}" ] || return 0
    tmp="$(mktemp "${output}.migrate.XXXXXX")" || return 1
    if ! jq '
        . as $root
        # agents.defaults.memorySearch -> top-level memory.search
        | (($root.agents.defaults.memorySearch? // $root.memorySearch?) // null) as $legacy_mem
        | if $legacy_mem != null then
            .memory = ((.memory // {}) | .search = (($legacy_mem | if type == "object" then . else {} end) + ((.search // {}) | if type == "object" then . else {} end)))
          else . end
        | del(.agents.defaults.memorySearch, .memorySearch)
        # session.resetByType.dm -> direct
        | if (.session.resetByType.dm? != null) then
            .session.resetByType.direct = ((.session.resetByType.dm | if type == "object" then . else {} end) + ((.session.resetByType.direct // {}) | if type == "object" then . else {} end))
            | del(.session.resetByType.dm)
          else . end
        # retired control-ui auth flags
        | if ((.gateway.controlUi? // null) | type) == "object" then
            del(.gateway.controlUi.allowInsecureAuth, .gateway.controlUi.dangerouslyDisableDeviceAuth)
          else . end
        # Matrix flat streaming aliases -> nested streaming object
        | if ((.channels.matrix? // null) | type) == "object" then
            .channels.matrix |= (
              . as $m
              | if ($m.streaming | type) == "string" then .streaming = {mode: $m.streaming}
                elif ($m.streaming | type) == "boolean" then .streaming = {mode: (if $m.streaming then "partial" else "off" end)}
                elif ($m.streaming | type) == "object" then .
                else .streaming = {} end
              | if ($m.streamMode? != null) then .streaming.mode = (.streaming.mode // $m.streamMode) | del(.streamMode) else . end
              | if ($m.chunkMode? != null) then .streaming.chunkMode = (.streaming.chunkMode // $m.chunkMode) | del(.chunkMode) else . end
              | if ($m.blockStreaming? != null) then
                  .streaming.block = ((.streaming.block // {}) |
                    .enabled = (if .enabled != null then .enabled else $m.blockStreaming end))
                  | del(.blockStreaming)
                else . end
              | if ($m.blockStreamingCoalesce? != null) then
                  .streaming.block.coalesce = (.streaming.block.coalesce // $m.blockStreamingCoalesce)
                  | del(.blockStreamingCoalesce)
                else . end
            )
          else . end
        # retired fork-only matrix group switch: groups.<room>.allow
        # Upstream room entries accept requireMention/enabled/... but not
        # "allow". allow=true meant "the default" (room enabled), so drop it;
        # allow=false meant "do not answer in this room", which upstream
        # spells enabled=false, so keep the deny.
        | if ((.channels.matrix.groups? // null) | type) == "object" then
            .channels.matrix.groups |= with_entries(
              .value |= (if type == "object" then
                           (if (.allow == false) and (.enabled == null) then .enabled = false else . end)
                           | del(.allow)
                         else . end)
            )
          else . end
        # legacy agents.defaults.models restriction -> explicit modelPolicy
        | if ((.agents.defaults? // null) | type) == "object"
             and ((.agents.defaults.models? // null) | type) == "object"
             and (.agents.defaults.modelPolicy? == null) then
            .agents.defaults.modelPolicy = {allow: (.agents.defaults.models | keys)}
          else . end
      ' "${input}" > "${tmp}"; then
        rm -f "${tmp}"
        return 1
    fi
    mv "${tmp}" "${output}"
}
