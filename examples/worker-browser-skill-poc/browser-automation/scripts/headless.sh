#!/bin/bash
set -euo pipefail
SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
export AGENT_BROWSER_EXECUTABLE_PATH=/tmp/chromium
export LD_LIBRARY_PATH="/tmp/al2023/lib:/tmp${LD_LIBRARY_PATH:+:${LD_LIBRARY_PATH}}"
export FONTCONFIG_PATH=/tmp/fonts
exec bash "${SCRIPT_DIR}/browser.sh" "$@"
