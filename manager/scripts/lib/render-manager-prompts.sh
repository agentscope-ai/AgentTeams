#!/bin/bash
# render-manager-prompts.sh - Assemble AGENTS.md / HEARTBEAT.md from shared fragments
#
# Usage:
#   render-manager-prompts.sh openclaw [output_dir]
#   render-manager-prompts.sh copaw   [output_dir]
#   render-manager-prompts.sh all     [agent_src_dir]
#
# Default output_dir for openclaw/copaw: stdout
# Default agent_src_dir for all: /opt/hiclaw/agent (or manager/agent when run from repo)

set -euo pipefail

FRAGMENTS="${FRAGMENTS:-}"
RUNTIME="${1:-}"
OUT_DIR="${2:-}"

if [ -z "${FRAGMENTS}" ]; then
    SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    FRAGMENTS="$(cd "${SCRIPT_DIR}/../../agent/fragments" && pwd)"
fi

_concat_fragments() {
    local dest="$1"
    shift
    local first=1
    : > "${dest}.tmp"
    for frag in "$@"; do
        if [ ! -f "${frag}" ]; then
            echo "ERROR: missing fragment: ${frag}" >&2
            return 1
        fi
        if [ "${first}" -eq 1 ]; then
            cat "${frag}" >> "${dest}.tmp"
            first=0
        else
            printf '\n' >> "${dest}.tmp"
            cat "${frag}" >> "${dest}.tmp"
        fi
    done
    mv "${dest}.tmp" "${dest}"
}

_render_agents_openclaw() {
    local dest="$1"
    _concat_fragments "${dest}" \
        "${FRAGMENTS}/AGENTS/header-openclaw.md" \
        "${FRAGMENTS}/AGENTS/host-files.md" \
        "${FRAGMENTS}/AGENTS/every-session.md" \
        "${FRAGMENTS}/AGENTS/minio.md" \
        "${FRAGMENTS}/AGENTS/gotchas-openclaw.md" \
        "${FRAGMENTS}/AGENTS/controller-api.md" \
        "${FRAGMENTS}/AGENTS/memory.md" \
        "${FRAGMENTS}/AGENTS/tools.md" \
        "${FRAGMENTS}/AGENTS/management-skills.md" \
        "${FRAGMENTS}/AGENTS/group-rooms-openclaw.md" \
        "${FRAGMENTS}/AGENTS/heartbeat-section.md" \
        "${FRAGMENTS}/AGENTS/safety.md"
}

_render_agents_copaw() {
    local dest="$1"
    _concat_fragments "${dest}" \
        "${FRAGMENTS}/AGENTS/header-copaw.md" \
        "${FRAGMENTS}/AGENTS/host-files.md" \
        "${FRAGMENTS}/AGENTS/every-session.md" \
        "${FRAGMENTS}/AGENTS/minio.md" \
        "${FRAGMENTS}/AGENTS/gotchas-copaw.md" \
        "${FRAGMENTS}/AGENTS/message-sending-copaw.md" \
        "${FRAGMENTS}/AGENTS/controller-api.md" \
        "${FRAGMENTS}/AGENTS/memory.md" \
        "${FRAGMENTS}/AGENTS/tools.md" \
        "${FRAGMENTS}/AGENTS/management-skills.md" \
        "${FRAGMENTS}/AGENTS/group-rooms-copaw.md" \
        "${FRAGMENTS}/AGENTS/heartbeat-section.md" \
        "${FRAGMENTS}/AGENTS/safety.md"
}

_render_heartbeat_openclaw() {
    local dest="$1"
    _concat_fragments "${dest}" \
        "${FRAGMENTS}/HEARTBEAT/header-openclaw.md" \
        "${FRAGMENTS}/HEARTBEAT/step-01-state.md" \
        "${FRAGMENTS}/HEARTBEAT/openclaw-body.md"
}

_render_heartbeat_copaw() {
    local dest="$1"
    _concat_fragments "${dest}" \
        "${FRAGMENTS}/HEARTBEAT/header-copaw.md" \
        "${FRAGMENTS}/HEARTBEAT/step-01-state.md" \
        "${FRAGMENTS}/HEARTBEAT/copaw-body.md" \
        "${FRAGMENTS}/HEARTBEAT/copaw-cli-reference.md"
}

case "${RUNTIME}" in
    openclaw)
        if [ -n "${OUT_DIR}" ]; then
            _render_agents_openclaw "${OUT_DIR}/AGENTS.md"
            _render_heartbeat_openclaw "${OUT_DIR}/HEARTBEAT.md"
        else
            _render_agents_openclaw /dev/stdout
        fi
        ;;
    copaw)
        if [ -n "${OUT_DIR}" ]; then
            mkdir -p "${OUT_DIR}"
            _render_agents_copaw "${OUT_DIR}/AGENTS.md"
            _render_heartbeat_copaw "${OUT_DIR}/HEARTBEAT.md"
        else
            _render_agents_copaw /dev/stdout
        fi
        ;;
    all)
        AGENT_SRC="${OUT_DIR:-/opt/hiclaw/agent}"
        _render_agents_openclaw "${AGENT_SRC}/AGENTS.md"
        _render_heartbeat_openclaw "${AGENT_SRC}/HEARTBEAT.md"
        mkdir -p "${AGENT_SRC}/copaw-manager-agent"
        _render_agents_copaw "${AGENT_SRC}/copaw-manager-agent/AGENTS.md"
        _render_heartbeat_copaw "${AGENT_SRC}/copaw-manager-agent/HEARTBEAT.md"
        echo "Rendered manager prompts under ${AGENT_SRC}"
        ;;
    *)
        echo "Usage: $0 {openclaw|copaw|all} [output_dir]" >&2
        exit 1
        ;;
esac
