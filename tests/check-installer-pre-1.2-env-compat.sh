#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INSTALLER="${ROOT_DIR}/install/agentteams-install.sh"
AGENTTEAMS_KNOWN_STABLE_VERSION="v1.1.2"

eval "$(
    sed -n \
        -e '/^_normalize_version()/,/^}/p' \
        -e '/^_ver_lt()/,/^}/p' \
        -e '/^_use_legacy_image_env()/,/^}/p' \
        -e '/^_controller_env_prefix()/,/^}/p' \
        -e '/^_controller_storage_prefix()/,/^}/p' \
        "${INSTALLER}"
)"

assert_normalized_version() {
    local input="$1"
    local expected="$2"
    local actual
    actual="$(_normalize_version "${input}")"
    if [ "${actual}" != "${expected}" ]; then
        echo "FAIL: expected ${input} to normalize to ${expected}, got ${actual}" >&2
        exit 1
    fi
}

assert_normalized_version "1.2.0.beta.1" "v1.2.0-beta.1"
assert_normalized_version "v1.2.0-beta.1" "v1.2.0-beta.1"
assert_normalized_version "1.1.2" "v1.1.2"
assert_normalized_version "latest" "latest"

assert_legacy() {
    if ! _use_legacy_image_env "$1"; then
        echo "FAIL: expected legacy env compatibility for $1" >&2
        exit 1
    fi
}

assert_current() {
    if _use_legacy_image_env "$1"; then
        echo "FAIL: did not expect legacy env compatibility for $1" >&2
        exit 1
    fi
}

assert_legacy "v1.1.2"
assert_legacy "v1.1.9"
assert_legacy "latest"
assert_current "v1.2.0"
assert_current "v1.2.0-beta.1"
assert_current "v1.3.0"
AGENTTEAMS_KNOWN_STABLE_VERSION="v1.2.0"
assert_current "latest"

legacy_prefix='HIC''LAW_'
assert_prefix() {
    local version="$1"
    local expected="$2"
    local actual
    actual="$(_controller_env_prefix "${version}")"
    if [ "${actual}" != "${expected}" ]; then
        echo "FAIL: expected ${version} to use ${expected}, got ${actual}" >&2
        exit 1
    fi
}

AGENTTEAMS_KNOWN_STABLE_VERSION="v1.1.2"
assert_prefix "v1.1.2" "${legacy_prefix}"
assert_prefix "latest" "${legacy_prefix}"
assert_prefix "v1.2.0" "AGENTTEAMS_"
assert_prefix "v1.2.0-beta.1" "AGENTTEAMS_"
assert_prefix "$(_normalize_version "1.2.0.beta.1")" "AGENTTEAMS_"
assert_prefix "v1.3.0" "AGENTTEAMS_"

legacy_storage_alias='hic''law'
assert_storage_prefix() {
    local version="$1"
    local expected="$2"
    local actual
    actual="$(_controller_storage_prefix "${version}")"
    if [ "${actual}" != "${expected}" ]; then
        echo "FAIL: expected ${version} to use storage prefix ${expected}, got ${actual}" >&2
        exit 1
    fi
}

AGENTTEAMS_KNOWN_STABLE_VERSION="v1.1.2"
assert_storage_prefix "v1.1.2" "${legacy_storage_alias}/agentteams-storage"
assert_storage_prefix "latest" "${legacy_storage_alias}/agentteams-storage"
assert_storage_prefix "v1.2.0" "agentteams/agentteams-storage"
assert_storage_prefix "v1.2.0-beta.1" "agentteams/agentteams-storage"
assert_storage_prefix "v1.3.0" "agentteams/agentteams-storage"
AGENTTEAMS_KNOWN_STABLE_VERSION="v1.2.0"
assert_storage_prefix "latest" "agentteams/agentteams-storage"

controller_env_block="$(
    sed -n \
        '/        # Controller env args/,/        # shellcheck disable=SC2086/p' \
        "${INSTALLER}"
)"

for suffix in \
    REGISTRATION_TOKEN \
    MINIO_USER \
    MINIO_PASSWORD \
    MANAGER_IMAGE \
    WORKER_IMAGE \
    COPAW_WORKER_IMAGE \
    HERMES_WORKER_IMAGE \
    MATRIX_DOMAIN \
    MATRIX_URL \
    MINIO_ENDPOINT \
    STORAGE_PREFIX \
    FS_BUCKET \
    CONTROLLER_URL \
    DOCKER_NETWORK \
    RESOURCE_PREFIX
do
    if ! grep -Fq "\${_ctrl_env_prefix}${suffix}=" <<<"${controller_env_block}"; then
        echo "FAIL: controller env block does not select ${suffix} through one versioned prefix" >&2
        exit 1
    fi
    if grep -Fq "\"AGENTTEAMS_${suffix}=" <<<"${controller_env_block}" ||
        grep -Fq "\"${legacy_prefix}${suffix}=" <<<"${controller_env_block}"; then
        echo "FAIL: controller env block injects an additional fixed-prefix ${suffix}" >&2
        exit 1
    fi
done

echo "PASS: installer selects exactly one controller env contract by image version"
