#!/usr/bin/env bash

set -euo pipefail

# Verify that the experimental Qwen Code worker is wired through both
# local installers at the same boundary as the other Worker runtimes.

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BASH_INSTALLER="${ROOT_DIR}/install/agentteams-install.sh"
POWERSHELL_INSTALLER="${ROOT_DIR}/install/agentteams-install.ps1"
MAKEFILE="${ROOT_DIR}/Makefile"
CORE_BUILD_WORKFLOW="${ROOT_DIR}/.github/workflows/build.yml"
RC_BUILD_WORKFLOW="${ROOT_DIR}/.github/workflows/build-rc.yml"
DSH_BUILD_WORKFLOW="${ROOT_DIR}/.github/workflows/build-qwen-code.yml"
RELEASE_WORKFLOW="${ROOT_DIR}/.github/workflows/release.yml"

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

extract_bash_function() {
    local name="$1"
    sed -n "/^${name}()/,/^}/p" "${BASH_INSTALLER}"
}

eval "$(extract_bash_function _ver_lt)"
eval "$(extract_bash_function _supports_qwen_code)"
eval "$(extract_bash_function _refresh_known_stable_version)"

AGENTTEAMS_KNOWN_STABLE_VERSION="v1.2.3"
AGENTTEAMS_QWEN_CODE_MIN_VERSION="v1.2.4"
unset AGENTTEAMS_INSTALL_QWEN_CODE_WORKER_IMAGE
if _supports_qwen_code "v1.2.3" || _supports_qwen_code "latest"; then
    fail "Bash installer must not offer an unpublished Qwen Code image for v1.2.3"
fi
_supports_qwen_code "v1.2.4" ||
    fail "Bash installer must offer Qwen Code starting at v1.2.4"
AGENTTEAMS_INSTALL_QWEN_CODE_WORKER_IMAGE="agentteams/qwen-code-worker:test"
if _supports_qwen_code "v1.2.3"; then
    fail "A Worker-only image override must not bypass the v1.2.4 Controller contract"
fi
AGENTTEAMS_INSTALL_CONTROLLER_IMAGE="agentteams/controller:dsh-test"
if _supports_qwen_code "v1.2.3"; then
    fail "A Controller image override must not bypass the embedded control-plane contract"
fi
AGENTTEAMS_INSTALL_EMBEDDED_IMAGE="agentteams/embedded:dsh-test"
_supports_qwen_code "v1.2.3" ||
    fail "Compatible Worker and embedded Controller overrides must enable development installs"
unset AGENTTEAMS_INSTALL_QWEN_CODE_WORKER_IMAGE
unset AGENTTEAMS_INSTALL_CONTROLLER_IMAGE
unset AGENTTEAMS_INSTALL_EMBEDDED_IMAGE

log() { :; }
msg() { printf '%s' "$1"; }
curl() { printf '%s\n' '{"tag_name":"v1.2.4"}'; }
_refresh_known_stable_version
[[ "${AGENTTEAMS_KNOWN_STABLE_VERSION}" == "v1.2.4" ]] ||
    fail "Bash latest probe must update the stable Controller feature-gate version"
_supports_qwen_code "latest" ||
    fail "Bash latest installs must expose DSH after the v1.2.4 probe succeeds"

grep -Fq 'AGENTTEAMS_INSTALL_QWEN_CODE_WORKER_IMAGE' "${BASH_INSTALLER}" ||
    fail "Bash installer must support a Qwen Code Worker image override"
grep -Fq 'agentteams-qwen-code-worker:${AGENTTEAMS_QWEN_CODE_WORKER_VERSION}' "${BASH_INSTALLER}" ||
    fail "Bash installer must resolve the independently versioned Qwen Code Worker image"
grep -Fq '_refresh_known_stable_version' "${BASH_INSTALLER}" ||
    fail "Bash installer must refresh the stable version used by the latest feature gate"
grep -Fq '5) $(msg worker_runtime.qwen_code)' "${BASH_INSTALLER}" ||
    fail "Bash installer Worker menu must list Qwen Code"
grep -Fq 'AGENTTEAMS_DEFAULT_WORKER_RUNTIME="qwen-code"' "${BASH_INSTALLER}" ||
    fail "Bash installer must map menu choice 5 to qwen-code"
grep -Fq 'DEFAULT_WORKER_RUNTIME}" = "qwen-code"' "${BASH_INSTALLER}" ||
    fail "Bash installer must reject unavailable non-interactive Qwen Code selections"
grep -E '_pull_image.*QWEN_CODE_WORKER_IMAGE' "${BASH_INSTALLER}" | grep -Eqv '^[[:space:]]*#' ||
    fail "Bash installer must pull the Qwen Code Worker image"
grep -Fq 'AGENTTEAMS_QWEN_CODE_WORKER_IMAGE=${QWEN_CODE_WORKER_IMAGE}' "${BASH_INSTALLER}" ||
    fail "Bash installer env file must expose the Qwen Code Worker image"

grep -Fq 'AGENTTEAMS_INSTALL_QWEN_CODE_WORKER_IMAGE' "${POWERSHELL_INSTALLER}" ||
    fail "PowerShell installer must support a Qwen Code Worker image override"
grep -Fq 'agentteams-qwen-code-worker:$($script:AGENTTEAMS_QWEN_CODE_WORKER_VERSION)' "${POWERSHELL_INSTALLER}" ||
    fail "PowerShell installer must resolve the independently versioned Qwen Code Worker image"
grep -Fq 'Update-AgentTeamsKnownStableVersion' "${POWERSHELL_INSTALLER}" ||
    fail "PowerShell installer must refresh the stable version used by the latest feature gate"
grep -Fq 'repos/agentscope-ai/AgentTeams/releases/latest' "${POWERSHELL_INSTALLER}" ||
    fail "PowerShell installer must query the latest GitHub release"
grep -Fq "5) \$(Get-Msg 'worker_runtime.qwen_code')" "${POWERSHELL_INSTALLER}" ||
    fail "PowerShell installer Worker menu must list Qwen Code"
grep -Fq '"5" { if ($qwenCodeAvailable) { "qwen-code" }' "${POWERSHELL_INSTALLER}" ||
    fail "PowerShell installer must map menu choice 5 to qwen-code"
grep -Fq 'DEFAULT_WORKER_RUNTIME -eq "qwen-code" -and -not $qwenCodeAvailable' "${POWERSHELL_INSTALLER}" ||
    fail "PowerShell installer must reject unavailable non-interactive Qwen Code selections"
grep -Fq '$workerImages += $script:QWEN_CODE_WORKER_IMAGE' "${POWERSHELL_INSTALLER}" ||
    fail "PowerShell installer must pull the Qwen Code Worker image"
grep -Fq 'AGENTTEAMS_QWEN_CODE_WORKER_IMAGE=$($Config.QWEN_CODE_WORKER_IMAGE)' "${POWERSHELL_INSTALLER}" ||
    fail "PowerShell installer env file must expose the Qwen Code Worker image"
pwsh -NoProfile -File "${ROOT_DIR}/tests/check-powershell-qwen-code-latest.ps1"

grep -Fq 'QWEN_CODE_WORKER_VERSION ?= v0.1.0' "${MAKEFILE}" ||
    fail "Makefile must give the Qwen Code runtime an independent version"
if sed -n '/^push-native:/,/^push-native-manager:/p' "${MAKEFILE}" | grep -Fq 'QWEN_CODE'; then
    fail "The aggregate native push must not publish or retag the Qwen Code runtime"
fi
if grep -Fq 'qwen-code-worker' "${CORE_BUILD_WORKFLOW}" || grep -Fq 'qwen-code-worker' "${RC_BUILD_WORKFLOW}"; then
    fail "Core and RC workflows must not publish the Qwen Code runtime"
fi
grep -Fq 'make push-qwen-code-worker' "${DSH_BUILD_WORKFLOW}" ||
    fail "Qwen Code must have a dedicated image publishing workflow"
grep -Fq 'QWEN_CODE_WORKER_VERSION: v0.1.0' "${RELEASE_WORKFLOW}" ||
    fail "Release notes must pin the independently published Qwen Code runtime"
grep -Fq 'docker buildx imagetools inspect' "${RELEASE_WORKFLOW}" ||
    fail "A core release must verify that its pinned Qwen Code runtime is pullable"
grep -Fq 'python3 -m py_compile' "${RELEASE_WORKFLOW}" ||
    fail "A core release must smoke-test the pinned Qwen Code runtime contract"

echo "PASS: Qwen Code installer and independent release contracts are wired"
