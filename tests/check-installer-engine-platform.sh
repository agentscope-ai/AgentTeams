#!/usr/bin/env bash
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
function_body=$(sed -n '/^_select_available_auto_version() {/,/^}/p' "$ROOT/install/agentteams-install.sh")
for engine in docker podman docker-podman; do
    for arch in amd64 arm64; do
        result=$(ENGINE="$engine" ARCH="$arch" bash -s <<BASH
set -e
AGENTTEAMS_AUTO_VERSION=1
AGENTTEAMS_VERSION=v1.2.7
DOCKER_CMD=mock_engine
mock_engine() {
    [ "\$1" = info ] || return 91
    if [ "\$ENGINE" = docker ]; then
        [ "\$3" = '{{.OSType}}/{{.Architecture}}' ] || return 92
    else
        [ "\$3" = '{{.Host.OS}}/{{.Host.Arch}}' ] || return 92
    fi
    printf 'linux/%s\n' "\$ARCH"
}
die() { echo "\$*" >&2; exit 70; }
_check_version_images() { printf '%s\n' "\$2"; }
resolve_image_tags() { :; }
$function_body
_select_available_auto_version
BASH
)
        [ "$result" = "linux/$arch" ] || exit 1
    done
done
printf 'PASS: Docker and Podman platform preflight (amd64/arm64)\n'
