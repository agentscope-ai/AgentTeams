#!/bin/bash
set -euo pipefail

# POC only: install into an explicitly selected disposable Worker container.
if [ ! -f /.dockerenv ] || [ "$(id -u)" != 0 ] || ! command -v apt-get >/dev/null; then
    echo "This POC installer requires a root Debian-based Docker Worker container." >&2
    exit 1
fi

exec 9>/tmp/agentteams-browser-skill-install.lock
flock -n 9 || { echo "Another browser skill installation is running." >&2; exit 1; }

CLI_VERSION=0.41.2
INSTALL_DIR=/opt/agentteams/optional/browser-automation
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y --no-install-recommends chromium fonts-noto-cjk
mkdir -p "${INSTALL_DIR}"
npm install --prefix "${INSTALL_DIR}" --save-exact --ignore-scripts "agent-browser@${CLI_VERSION}"
"${INSTALL_DIR}/node_modules/.bin/agent-browser" --version
chromium --version
