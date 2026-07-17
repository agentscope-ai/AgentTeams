#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEAMHARNESS_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
SHARED_INSTALL="${SCRIPT_DIR}/../../../adapters/qwenpaw/install-plugin.sh"

export AGENTTEAMS_PLUGIN_INSTALL_LOG="${TEAMHARNESS_INSTALL_LOG:-}"

exec bash "${SHARED_INSTALL}" "${TEAMHARNESS_DIR}"
