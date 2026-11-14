#!/bin/bash
set -euo pipefail

CLI=/opt/agentteams/optional/browser-automation/node_modules/.bin/agent-browser
export AGENT_BROWSER_EXECUTABLE_PATH="${AGENT_BROWSER_EXECUTABLE_PATH:-/usr/bin/chromium}"
if [ ! -x "${CLI}" ] || [ ! -x "${AGENT_BROWSER_EXECUTABLE_PATH}" ]; then
    echo "Browser dependencies are missing. Run this skill's install.sh in your selected Worker container." >&2
    exit 1
fi

# Keep browser runtime state outside the Worker's synchronized HOME.
BROWSER_SKILL_STATE=/tmp/agentteams-browser-poc
mkdir -p "${BROWSER_SKILL_STATE}"
chmod 700 "${BROWSER_SKILL_STATE}"
export XDG_CACHE_HOME="${BROWSER_SKILL_STATE}/cache"
export XDG_CONFIG_HOME="${BROWSER_SKILL_STATE}/config"
export AGENT_BROWSER_SOCKET_DIR="${BROWSER_SKILL_STATE}/sockets"
export AGENT_BROWSER_DOWNLOAD_PATH="${BROWSER_SKILL_STATE}/downloads"
export AGENT_BROWSER_SCREENSHOT_DIR="${BROWSER_SKILL_STATE}/screenshots"
export AGENT_BROWSER_CONFIG="${BROWSER_SKILL_STATE}/config.json"
[ -f "${AGENT_BROWSER_CONFIG}" ] || printf '{}\n' > "${AGENT_BROWSER_CONFIG}"
export AGENT_BROWSER_SESSION="${BROWSER_SKILL_SESSION:-browser-poc}"
case "${AGENT_BROWSER_SESSION}" in
    *[!a-zA-Z0-9_-]*) echo "Use a short alphanumeric task session name." >&2; exit 1 ;;
esac
exec "${CLI}" --profile "${BROWSER_SKILL_STATE}/profile-${AGENT_BROWSER_SESSION}" --idle-timeout 60s "$@"
