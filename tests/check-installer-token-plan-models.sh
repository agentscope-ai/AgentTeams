#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SHELL_INSTALLER="${ROOT_DIR}/install/agentteams-install.sh"
POWERSHELL_INSTALLER="${ROOT_DIR}/install/agentteams-install.ps1"
WINDOWS_DOC="${ROOT_DIR}/docs/usage/deployment/windows.md"
WINDOWS_DOC_ZH="${ROOT_DIR}/docs/zh-cn/usage/deployment/windows.md"

require_line() {
    local path="$1"
    local expected="$2"
    if ! grep -Fq "${expected}" "${path}"; then
        echo "FAIL: expected ${path} to contain: ${expected}" >&2
        exit 1
    fi
}

reject_line() {
    local path="$1"
    local unexpected="$2"
    if grep -Fq "${unexpected}" "${path}"; then
        echo "FAIL: ${path} still contains stale Token Plan model entry: ${unexpected}" >&2
        exit 1
    fi
}

for path in "${SHELL_INSTALLER}" "${POWERSHELL_INSTALLER}" "${WINDOWS_DOC}" "${WINDOWS_DOC_ZH}"; do
    require_line "${path}" "glm-5.2"
    reject_line "${path}" "  2) glm-5  -"
done

require_line "${SHELL_INSTALLER}" '2|glm-5|glm-5.2) AGENTTEAMS_DEFAULT_MODEL="glm-5.2"'
require_line "${POWERSHELL_INSTALLER}" '"^(2|glm-5|glm-5\.2)$"  { $script:config.DEFAULT_MODEL = "glm-5.2" }'

echo "PASS: installer Token Plan menus use glm-5.2"
