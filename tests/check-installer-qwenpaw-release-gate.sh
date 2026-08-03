#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BASH_INSTALLER="${ROOT_DIR}/install/agentteams-install.sh"
POWERSHELL_INSTALLER="${ROOT_DIR}/install/agentteams-install.ps1"
INTEGRATION_WORKFLOW="${ROOT_DIR}/.github/workflows/test-integration.yml"

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

extract_bash_function() {
    local name="$1"
    sed -n "/^${name}()/,/^}/p" "${BASH_INSTALLER}"
}

msg() { printf '%s' "$1"; }
log() { :; }
_ver_lt() { return 1; }

eval "$(extract_bash_function step_runtime)"
eval "$(extract_bash_function step_manager_runtime)"

AGENTTEAMS_NON_INTERACTIVE=1
AGENTTEAMS_UPGRADE=0
AGENTTEAMS_VERSION=v1.2.0
AGENTTEAMS_DEFAULT_WORKER_RUNTIME=""
AGENTTEAMS_MANAGER_RUNTIME=""

step_runtime >/dev/null
step_manager_runtime >/dev/null

[ "${AGENTTEAMS_DEFAULT_WORKER_RUNTIME}" = "copaw" ] ||
    fail "Bash installer must keep CoPaw as the default Worker runtime before QwenPaw is released"
[ "${AGENTTEAMS_MANAGER_RUNTIME}" = "copaw" ] ||
    fail "Bash installer must keep CoPaw as the default Manager runtime before QwenPaw is released"

bash_runtime_blocks="$(
    extract_bash_function step_runtime
    extract_bash_function step_manager_runtime
)"
grep -Fq 'worker_runtime.copaw' <<<"${bash_runtime_blocks}" ||
    fail "Bash installer Worker menu must expose CoPaw"
grep -Fq 'manager_runtime.copaw' <<<"${bash_runtime_blocks}" ||
    fail "Bash installer Manager menu must expose CoPaw"
if grep -Fq 'runtime.qwenpaw' <<<"${bash_runtime_blocks}"; then
    fail "Bash installer must not expose QwenPaw in runtime menus before release"
fi
if grep -E '_pull_image.*QWENPAW' "${BASH_INSTALLER}" | grep -Eqv '^[[:space:]]*#'; then
    fail "Bash installer must not pull a QwenPaw image before release"
fi

powershell_runtime_blocks="$(
    sed -n '/^function Step-Runtime {/,/^}/p' "${POWERSHELL_INSTALLER}"
    sed -n '/^function Step-ManagerRuntime {/,/^}/p' "${POWERSHELL_INSTALLER}"
)"
grep -Fq "worker_runtime.copaw" <<<"${powershell_runtime_blocks}" ||
    fail "PowerShell installer Worker menu must expose CoPaw"
grep -Fq "manager_runtime.copaw" <<<"${powershell_runtime_blocks}" ||
    fail "PowerShell installer Manager menu must expose CoPaw"
if grep -Fq 'runtime.qwenpaw' <<<"${powershell_runtime_blocks}"; then
    fail "PowerShell installer must not expose QwenPaw in runtime menus before release"
fi
if grep -Ei 'qwenpaw.*MANAGER_QWENPAW_IMAGE' "${POWERSHELL_INSTALLER}" | grep -Eqv '^[[:space:]]*#'; then
    fail "PowerShell installer must not select a QwenPaw Manager image before release"
fi
powershell_worker_images="$(sed -n '/\$workerImages = @(/,/^    )/p' "${POWERSHELL_INSTALLER}")"
if grep -F 'QWENPAW_WORKER_IMAGE' <<<"${powershell_worker_images}" | grep -Eqv '^[[:space:]]*#'; then
    fail "PowerShell installer must not pull a QwenPaw Worker image before release"
fi

for manager_crd in \
    "${ROOT_DIR}/agentteams-controller/config/crd/managers.agentteams.io.yaml" \
    "${ROOT_DIR}/helm/agentteams/crds/managers.agentteams.io.yaml"; do
    grep -Eq 'enum: \[[^]]*copaw[^]]*\]' "${manager_crd}" ||
        fail "Manager CRD must keep accepting CoPaw while the installer defaults to CoPaw: ${manager_crd}"
done

# Fork CI must exercise the PR-built QwenPaw Manager explicitly without
# changing the public installer's CoPaw default or registry pull behavior.
grep -Fq 'AGENTTEAMS_INSTALL_MANAGER_COPAW_IMAGE: agentteams/manager-qwenpaw:latest' \
    "${INTEGRATION_WORKFLOW}" ||
    fail "Integration CI must explicitly map the CoPaw compatibility label to the PR-built QwenPaw Manager image"

echo "PASS: installers keep QwenPaw behind the release gate"
