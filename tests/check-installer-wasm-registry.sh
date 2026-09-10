#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SHELL_INSTALLER="${ROOT_DIR}/install/agentteams-install.sh"
POWERSHELL_INSTALLER="${ROOT_DIR}/install/agentteams-install.ps1"
PRIMARY_REGISTRY="higress-registry.cn-hangzhou.cr.aliyuncs.com"

require_line() {
    local path="$1"
    local expected="$2"
    if ! grep -Fq "${expected}" "${path}"; then
        echo "FAIL: expected ${path} to contain: ${expected}" >&2
        exit 1
    fi
}

if grep -Fq 'HIGRESS_ADMIN_WASM_PLUGIN_IMAGE_REGISTRY=${AGENTTEAMS_REGISTRY}' "${SHELL_INSTALLER}"; then
    echo "FAIL: shell installer ties Higress WASM plugins to the regional image registry" >&2
    exit 1
fi

if grep -Fq 'HIGRESS_ADMIN_WASM_PLUGIN_IMAGE_REGISTRY=$($Config.REGISTRY)' "${POWERSHELL_INSTALLER}"; then
    echo "FAIL: PowerShell installer ties Higress WASM plugins to the regional image registry" >&2
    exit 1
fi

require_line "${SHELL_INSTALLER}" "AGENTTEAMS_HIGRESS_WASM_PLUGIN_REGISTRY=\"\${AGENTTEAMS_HIGRESS_WASM_PLUGIN_REGISTRY:-${PRIMARY_REGISTRY}}\""
require_line "${POWERSHELL_INSTALLER}" 'AGENTTEAMS_HIGRESS_WASM_PLUGIN_REGISTRY=$($Config.HIGRESS_WASM_PLUGIN_REGISTRY)'
require_line "${SHELL_INSTALLER}" "HIGRESS_ADMIN_WASM_PLUGIN_IMAGE_REGISTRY=\${AGENTTEAMS_HIGRESS_WASM_PLUGIN_REGISTRY}"
require_line "${POWERSHELL_INSTALLER}" 'HIGRESS_ADMIN_WASM_PLUGIN_IMAGE_REGISTRY=$($Config.HIGRESS_WASM_PLUGIN_REGISTRY)'

echo "PASS: installers keep Higress WASM plugin registry on the primary registry by default"
