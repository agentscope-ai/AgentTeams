#!/bin/bash
# Deprecated compatibility entrypoint. Use agentteams-install.sh.

set -e

echo "[AgentTeams] install/hiclaw-install.sh is deprecated; use install/agentteams-install.sh." >&2

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" 2>/dev/null && pwd || true)"
if [ -n "${SCRIPT_DIR}" ] && [ -f "${SCRIPT_DIR}/agentteams-install.sh" ]; then
    exec bash "${SCRIPT_DIR}/agentteams-install.sh" "$@"
fi

ref="${AGENTTEAMS_VERSION:-main}"
[ "${ref}" = "latest" ] && ref="main"
exec bash <(curl -fsSL "https://raw.githubusercontent.com/agentscope-ai/AgentTeams/${ref}/install/agentteams-install.sh") "$@"
