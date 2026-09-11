#!/usr/bin/env bash

set -euo pipefail

# Verify that QwenPaw is a first-class local installer runtime.

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BASH_INSTALLER="${ROOT_DIR}/install/agentteams-install.sh"
POWERSHELL_INSTALLER="${ROOT_DIR}/install/agentteams-install.ps1"
INTEGRATION_WORKFLOW="${ROOT_DIR}/.github/workflows/test-integration.yml"
BUILD_WORKFLOW="${ROOT_DIR}/.github/workflows/build.yml"
RELEASE_WORKFLOW="${ROOT_DIR}/.github/workflows/release.yml"

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
eval "$(extract_bash_function manager_image_for_runtime)"

AGENTTEAMS_NON_INTERACTIVE=1
AGENTTEAMS_UPGRADE=0
AGENTTEAMS_VERSION=v1.2.1
AGENTTEAMS_DEFAULT_WORKER_RUNTIME=""
AGENTTEAMS_MANAGER_RUNTIME=""

step_runtime >/dev/null
step_manager_runtime >/dev/null

[ "${AGENTTEAMS_DEFAULT_WORKER_RUNTIME}" = "qwenpaw" ] ||
    fail "Bash installer must default new Workers to QwenPaw"
[ "${AGENTTEAMS_MANAGER_RUNTIME}" = "qwenpaw" ] ||
    fail "Bash installer must default the Manager to QwenPaw"

MANAGER_IMAGE="manager-openclaw:test"
MANAGER_QWENPAW_IMAGE="manager-qwenpaw:test"
MANAGER_COPAW_IMAGE="manager-copaw:test"
[ "$(manager_image_for_runtime qwenpaw)" = "${MANAGER_QWENPAW_IMAGE}" ] ||
    fail "Bash installer must route qwenpaw to the QwenPaw Manager image"
[ "$(manager_image_for_runtime copaw)" = "${MANAGER_COPAW_IMAGE}" ] ||
    fail "Bash installer must keep the legacy CoPaw Manager image route"
[ "$(manager_image_for_runtime openclaw)" = "${MANAGER_IMAGE}" ] ||
    fail "Bash installer must route openclaw to the OpenClaw Manager image"

bash_runtime_blocks="$(
    extract_bash_function step_runtime
    extract_bash_function step_manager_runtime
)"
grep -Fq '1) $(msg worker_runtime.qwenpaw)' <<<"${bash_runtime_blocks}" ||
    fail "Bash installer Worker menu must list QwenPaw first"
grep -Fq '4) $(msg worker_runtime.copaw)' <<<"${bash_runtime_blocks}" ||
    fail "Bash installer Worker menu must list legacy CoPaw last for current versions"
grep -Fq '1) $(msg manager_runtime.qwenpaw)' <<<"${bash_runtime_blocks}" ||
    fail "Bash installer Manager menu must list QwenPaw first"
grep -Fq '3) $(msg manager_runtime.copaw)' <<<"${bash_runtime_blocks}" ||
    fail "Bash installer Manager menu must list legacy CoPaw last"
grep -Fq 'CoPaw（旧版本，建议升级为 QwenPaw）' "${BASH_INSTALLER}" ||
    fail "Bash installer must recommend upgrading CoPaw to QwenPaw"
grep -E '_pull_image.*QWENPAW_WORKER_IMAGE' "${BASH_INSTALLER}" | grep -Eqv '^[[:space:]]*#' ||
    fail "Bash installer must pull the published QwenPaw Worker image"
grep -Fq 'AGENTTEAMS_INSTALL_MANAGER_QWENPAW_IMAGE' "${BASH_INSTALLER}" ||
    fail "Bash installer must support a QwenPaw Manager image override"
grep -Fq 'agentteams-manager-qwenpaw:${AGENTTEAMS_VERSION}' "${BASH_INSTALLER}" ||
    fail "Bash installer must resolve the published QwenPaw Manager image"

powershell_runtime_blocks="$(
    sed -n '/^function Step-Runtime {/,/^}/p' "${POWERSHELL_INSTALLER}"
    sed -n '/^function Step-ManagerRuntime {/,/^}/p' "${POWERSHELL_INSTALLER}"
)"
grep -Fq "1) \$(Get-Msg 'worker_runtime.qwenpaw')" <<<"${powershell_runtime_blocks}" ||
    fail "PowerShell installer Worker menu must list QwenPaw first"
grep -Fq "4) \$(Get-Msg 'worker_runtime.copaw')" <<<"${powershell_runtime_blocks}" ||
    fail "PowerShell installer Worker menu must list legacy CoPaw last"
grep -Fq "1) \$(Get-Msg 'manager_runtime.qwenpaw')" <<<"${powershell_runtime_blocks}" ||
    fail "PowerShell installer Manager menu must list QwenPaw first"
grep -Fq "3) \$(Get-Msg 'manager_runtime.copaw')" <<<"${powershell_runtime_blocks}" ||
    fail "PowerShell installer Manager menu must list legacy CoPaw last"
grep -Fq 'CoPaw（旧版本，建议升级为 QwenPaw）' "${POWERSHELL_INSTALLER}" ||
    fail "PowerShell installer must recommend upgrading CoPaw to QwenPaw"
grep -Fq 'AGENTTEAMS_INSTALL_MANAGER_QWENPAW_IMAGE' "${POWERSHELL_INSTALLER}" ||
    fail "PowerShell installer must support a QwenPaw Manager image override"
powershell_worker_images="$(sed -n '/\$workerImages = @(/,/^    )/p' "${POWERSHELL_INSTALLER}")"
grep -F 'QWENPAW_WORKER_IMAGE' <<<"${powershell_worker_images}" | grep -Eqv '^[[:space:]]*#' ||
    fail "PowerShell installer must pull the published QwenPaw Worker image"

grep -F 'echo "targets=' "${BUILD_WORKFLOW}" | grep -F 'qwenpaw-worker' >/dev/null ||
    fail "Tag-triggered image builds must publish qwenpaw-worker"
grep -Fq 'docker pull ${REGISTRY}/${REPO}/agentteams-qwenpaw-worker:${VERSION}' \
    "${RELEASE_WORKFLOW}" ||
    fail "Release notes must list the versioned QwenPaw Worker image"

for manager_crd in \
    "${ROOT_DIR}/agentteams-controller/config/crd/managers.agentteams.io.yaml" \
    "${ROOT_DIR}/helm/agentteams/crds/managers.agentteams.io.yaml"; do
    grep -Eq 'enum: \[[^]]*qwenpaw[^]]*\]' "${manager_crd}" ||
        fail "Manager CRD must accept QwenPaw: ${manager_crd}"
    grep -Eq 'enum: \[[^]]*copaw[^]]*\]' "${manager_crd}" ||
        fail "Manager CRD must keep accepting legacy CoPaw: ${manager_crd}"
done

# Fork CI must exercise the PR-built QwenPaw Manager directly while retaining
# one compatibility mapping for legacy CoPaw matrix entries.
grep -Fq 'AGENTTEAMS_INSTALL_MANAGER_QWENPAW_IMAGE: agentteams/manager-qwenpaw:latest' \
    "${INTEGRATION_WORKFLOW}" ||
    fail "Integration CI must map the QwenPaw runtime to the PR-built Manager image"
grep -Fq 'AGENTTEAMS_INSTALL_MANAGER_COPAW_IMAGE: agentteams/manager-qwenpaw:latest' \
    "${INTEGRATION_WORKFLOW}" ||
    fail "Integration CI must explicitly map the CoPaw compatibility label to the PR-built QwenPaw Manager image"

echo "PASS: QwenPaw Manager and Worker are first-class local installer options"
