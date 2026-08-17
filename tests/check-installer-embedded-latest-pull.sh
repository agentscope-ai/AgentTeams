#!/usr/bin/env bash

set -euo pipefail

# Prevent regressions where selecting `latest` only assigns the embedded image
# name and lets Docker reuse a stale local tag without contacting the registry.

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BASH_INSTALLER="${ROOT_DIR}/install/agentteams-install.sh"
POWERSHELL_INSTALLER="${ROOT_DIR}/install/agentteams-install.ps1"

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

extract_bash_function() {
    local name="$1"
    sed -n "/^${name}()/,/^}/p" "${BASH_INSTALLER}"
}

extract_powershell_function() {
    local name="$1"
    sed -n "/^function ${name} {/,/^}/p" "${POWERSHELL_INSTALLER}"
}

PULL_CALLS=0
PULLED_IMAGE=""
record_pull() {
    [ "$1" = "pull" ] || fail "unexpected container command: $*"
    PULL_CALLS=$((PULL_CALLS + 1))
    PULLED_IMAGE="$2"
}

msg() { printf '%s' "$1"; }
log() { :; }
_ver_lt() { return 1; }

eval "$(extract_bash_function resolve_embedded_image)"

AGENTTEAMS_INSTALL_EMBEDDED_IMAGE=""
AGENTTEAMS_REGISTRY="registry.example.com"
AGENTTEAMS_VERSION="latest"
AGENTTEAMS_FORCE_LEGACY=0
DOCKER_CMD=record_pull

resolve_embedded_image

[ "${PULL_CALLS}" -eq 1 ] ||
    fail "Bash installer must pull embedded:latest exactly once when it is available"
[ "${PULLED_IMAGE}" = "registry.example.com/agentteams/agentteams-embedded:latest" ] ||
    fail "Bash installer pulled the wrong embedded image: ${PULLED_IMAGE}"
[ "${EMBEDDED_IMAGE}" = "${PULLED_IMAGE}" ] ||
    fail "Bash installer did not select the pulled embedded image"

# Versioned installs must retain their existing behavior and prefer the exact tag.
PULL_CALLS=0
PULLED_IMAGE=""
AGENTTEAMS_VERSION="v1.2.2"
resolve_embedded_image
[ "${PULL_CALLS}" -eq 1 ] ||
    fail "Bash installer must pull an available versioned embedded image exactly once"
[ "${PULLED_IMAGE}" = "registry.example.com/agentteams/agentteams-embedded:v1.2.2" ] ||
    fail "Bash installer did not prefer the versioned embedded image"

# Explicit local build overrides are intentionally not probed in the registry.
PULL_CALLS=0
PULLED_IMAGE=""
AGENTTEAMS_INSTALL_EMBEDDED_IMAGE="agentteams/agentteams-embedded:latest"
resolve_embedded_image
[ "${PULL_CALLS}" -eq 0 ] ||
    fail "Bash installer must not pull an explicitly overridden local embedded image"
[ "${EMBEDDED_IMAGE}" = "${AGENTTEAMS_INSTALL_EMBEDDED_IMAGE}" ] ||
    fail "Bash installer did not preserve the explicit embedded image override"

# The install phase must route explicit remote overrides through the pull helper.
bash_install_pull_block="$(sed -n '/# Embedded controller image (resolve versioned tag, fallback to latest)/,/^    # Manager image is always required/p' "${BASH_INSTALLER}")"
grep -Fq '_pull_image "${EMBEDDED_IMAGE}" "install.image.embedded_exists" "install.image.pulling_embedded"' \
    <<<"${bash_install_pull_block}" ||
    fail "Bash installer must pull an explicitly overridden remote embedded image"

powershell_resolver="$(extract_powershell_function Resolve-EmbeddedImage)"
if grep -Fq 'if ($script:AGENTTEAMS_VERSION -eq "latest")' <<<"${powershell_resolver}"; then
    fail "PowerShell installer must not return before pulling embedded:latest"
fi
grep -Fq 'docker pull $versioned' <<<"${powershell_resolver}" ||
    fail "PowerShell installer must pull the resolved embedded image"
grep -Fq 'docker pull $latestTag' <<<"${powershell_resolver}" ||
    fail "PowerShell installer must pull embedded:latest"

# Docker availability and daemon health must be checked before image resolution,
# otherwise a missing/stopped runtime is misreported as an unavailable image.
powershell_install_manager="$(extract_powershell_function Install-Manager)"
docker_command_check_line="$(awk 'index($0, "Get-Command \"docker\"") { print NR; exit }' <<<"${powershell_install_manager}")"
docker_running_check_line="$(awk 'index($0, "if (-not (Test-DockerRunning))") { print NR; exit }' <<<"${powershell_install_manager}")"
embedded_resolve_line="$(awk '$0 ~ /^[[:space:]]*Resolve-EmbeddedImage[[:space:]]*$/ { print NR; exit }' <<<"${powershell_install_manager}")"

[ -n "${docker_command_check_line}" ] ||
    fail "PowerShell installer is missing the docker command availability check"
[ -n "${docker_running_check_line}" ] ||
    fail "PowerShell installer is missing the docker daemon health check"
[ -n "${embedded_resolve_line}" ] ||
    fail "PowerShell installer is missing the embedded image resolution call"
[ "${docker_command_check_line}" -lt "${embedded_resolve_line}" ] ||
    fail "PowerShell installer must check for docker/podman before resolving the embedded image"
[ "${docker_running_check_line}" -lt "${embedded_resolve_line}" ] ||
    fail "PowerShell installer must check container runtime health before resolving the embedded image"

powershell_install_pull_block="$(sed -n '/# Embedded image was already pulled by Resolve-EmbeddedImage unless overridden;/,/^        # Manager image/p' "${POWERSHELL_INSTALLER}")"
grep -Fq 'if ($LASTEXITCODE -ne 0) { Exit-Script 1 }' <<<"${powershell_install_pull_block}" ||
    fail "PowerShell installer must stop when an explicit embedded image pull fails"

echo "PASS: installers refresh the remote embedded:latest image"
