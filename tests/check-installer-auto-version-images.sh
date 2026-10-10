#!/usr/bin/env bash
set -euo pipefail
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INSTALLER="${ROOT_DIR}/install/agentteams-install.sh"
fail() { echo "FAIL: $*" >&2; exit 1; }
for fn in _normalize_version _ver_lt _supports_deepseek_harness _supports_qwen_code resolve_image_tags manager_image_for_runtime _check_install_image _check_version_images _select_available_auto_version; do
    eval "$(sed -n "/^${fn}()/,/^}/p" "$INSTALLER")"
done
log() { :; }
error() { :; }
die() { exit 9; }
CALLS=$(mktemp)
trap 'rm -f "$CALLS"' EXIT
mock_docker() {
    case "$1" in
        info) echo "${ENGINE_PLATFORM:-linux/aarch64}" ;;
        image)
            [[ "${*: -1}" != *"${BAD_IMAGE}"* ]] || return 1
            if grep -Fq -- "--platform ${EXPECTED_PLATFORM:-linux/arm64} ${*: -1}" "$CALLS"; then
                echo "${PULLED_PLATFORM:-${EXPECTED_PLATFORM:-linux/arm64}}"
            elif [ -n "${LOCAL_PLATFORM:-}" ]; then
                echo "$LOCAL_PLATFORM"
            else
                return 1
            fi ;;
        pull)
            echo "$*" >> "$CALLS"
            [[ "$*" == *"--platform ${EXPECTED_PLATFORM:-linux/arm64}"* ]] || fail "missing engine platform"
            if [[ "${*: -1}" == *"${BAD_IMAGE}"* ]]; then
                case "$FAILURE" in
                    missing) echo 'manifest unknown' >&2; return 1 ;;
                    arch) echo 'no matching manifest for linux/arm64/v8 in the manifest list entries' >&2; return 1 ;;
                    auth) echo 'unauthorized: authentication required' >&2; return 1 ;;
                    network) echo 'TLS handshake timeout' >&2; return 1 ;;
                esac
            fi ;;
        *) fail "unexpected command: $*" ;;
    esac
}
reset_case() {
    AGENTTEAMS_VERSION=v1.2.4
    AGENTTEAMS_AUTO_VERSION=1
    AGENTTEAMS_FALLBACK_VERSION=v1.2.3
    AGENTTEAMS_KNOWN_STABLE_VERSION=v1.2.4
    AGENTTEAMS_DEEPSEEK_HARNESS_MIN_VERSION=v1.2.4
    AGENTTEAMS_DEEPSEEK_HARNESS_WORKER_VERSION=v0.1.0
    AGENTTEAMS_QWEN_CODE_MIN_VERSION=v1.2.4
    AGENTTEAMS_QWEN_CODE_WORKER_VERSION=v0.1.0
    AGENTTEAMS_REGISTRY=registry.example
    AGENTTEAMS_MANAGER_RUNTIME=qwenpaw
    AGENTTEAMS_DEFAULT_WORKER_RUNTIME=qwenpaw
    AGENTTEAMS_UPGRADE=0
    AGENTTEAMS_DASHBOARD=1
    DOCKER_CMD=mock_docker
    BAD_IMAGE=does-not-match
    FAILURE=missing
    LOCAL_PLATFORM=""
    PULLED_PLATFORM=""
    ENGINE_PLATFORM=linux/aarch64
    EXPECTED_PLATFORM=linux/arm64
    unset AGENTTEAMS_INSTALL_WORKER_IMAGE
    : > "$CALLS"
}
reset_case
_select_available_auto_version
[ "$AGENTTEAMS_VERSION" = v1.2.4 ] || fail 'complete release rejected'
[ "$(wc -l < "$CALLS" | tr -d ' ')" = 7 ] || fail 'incomplete image set'
grep -q 'agentteams-manager-qwenpaw:v1.2.4' "$CALLS" || fail 'selected manager missing'
grep -q 'agentteams-deepseek-harness-worker:v0.1.0' "$CALLS" || fail 'independent runtime tag lost'
grep -q 'agentteams-dashboard:v1.2.4' "$CALLS" || fail 'dashboard missing'
for FAILURE_KIND in missing arch; do
    reset_case
    BAD_IMAGE=agentteams-hermes-worker:v1.2.4
    FAILURE=$FAILURE_KIND
    _select_available_auto_version
    [ "$AGENTTEAMS_VERSION" = v1.2.3 ] || fail 'incomplete release did not fall back'
    [ "$WORKER_IMAGE" = registry.example/agentteams/agentteams-worker:v1.2.3 ] || fail 'mixed release tags'
done
for FAILURE_KIND in auth network; do
    reset_case
    BAD_IMAGE=agentteams-worker:v1.2.4
    FAILURE=$FAILURE_KIND
    if ( _select_available_auto_version ); then fail 'ambiguous error accepted'; fi
    ! grep -q ':v1.2.3' "$CALLS" || fail 'ambiguous error caused downgrade'
done
reset_case
BAD_IMAGE=agentteams-worker:
if ( _select_available_auto_version ); then fail 'incomplete fallback accepted'; fi
reset_case
BAD_IMAGE=agentteams-worker:v1.2.4
AGENTTEAMS_UPGRADE=1
if ( _select_available_auto_version ); then fail 'upgrade silently downgraded'; fi
! grep -q ':v1.2.3' "$CALLS" || fail 'upgrade probed fallback'
reset_case
AGENTTEAMS_AUTO_VERSION=0
_select_available_auto_version
[ ! -s "$CALLS" ] || fail 'explicit version probed'
reset_case
AGENTTEAMS_DASHBOARD=0
AGENTTEAMS_MANAGER_RUNTIME=openclaw
_select_available_auto_version
! grep -q 'dashboard\|manager-qwenpaw' "$CALLS" || fail 'unused image probed'
grep -q 'agentteams-manager:v1.2.4' "$CALLS" || fail 'openclaw manager missing'
reset_case
ENGINE_PLATFORM=linux/x86_64
EXPECTED_PLATFORM=linux/amd64
_select_available_auto_version
reset_case
LOCAL_PLATFORM=linux/arm64
AGENTTEAMS_INSTALL_WORKER_IMAGE=local/worker:test
_select_available_auto_version
[ ! -s "$CALLS" ] || fail 'compatible local images unnecessarily pulled'
[ "$WORKER_IMAGE" = local/worker:test ] || fail 'explicit image override lost'
reset_case
LOCAL_PLATFORM=linux/amd64
_select_available_auto_version
[ -s "$CALLS" ] || fail 'wrong local architecture accepted'
reset_case
BAD_IMAGE=agentteams-worker:v1.2.4
AGENTTEAMS_DEFAULT_WORKER_RUNTIME=deepseek-harness
if ( _select_available_auto_version ); then fail 'unsupported runtime fallback accepted'; fi
reset_case
BAD_IMAGE=agentteams-dashboard:v1.2.4
if ( _select_available_auto_version ); then fail 'missing independent dashboard accepted'; fi
reset_case
PULLED_PLATFORM=linux/amd64
if ( _select_available_auto_version ); then fail 'wrong pulled platform accepted'; fi
reset_case
ENGINE_PLATFORM=windows/amd64
if ( _select_available_auto_version ); then fail 'unsupported platform accepted'; fi
# Exercise interactive selection: stable is automatic, latest/custom are explicit.
eval "$(sed -n '/^step_version()/,/^}/p' "$INSTALLER")"
_refresh_known_stable_version() { :; }
msg() { :; }
for choice in stable latest custom; do
    reset_case
    AGENTTEAMS_VERSION=""
    AGENTTEAMS_AUTO_VERSION=0
    step_version > /dev/null <<< "$choice
v1.2.4"
    if [ "$choice" = stable ]; then
        [ "$AGENTTEAMS_AUTO_VERSION" = 1 ] || fail 'stable selection not checked'
    else
        [ "$AGENTTEAMS_AUTO_VERSION" = 0 ] || fail 'explicit selection marked automatic'
    fi
done
echo 'PASS: automatic release selection checks required images and platform'
