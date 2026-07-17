#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKERFLOW_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
SHARED_INSTALL="${SCRIPT_DIR}/../../../adapters/qwenpaw/install-plugin.sh"

exec bash "${SHARED_INSTALL}" "${WORKERFLOW_DIR}"
