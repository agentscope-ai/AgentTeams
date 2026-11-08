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

# ---------------------------------------------------------------------------
# Legacy CoPaw worker image inheritance (upgrade path, issue #1310)
# ---------------------------------------------------------------------------
# PR #1335 removed the default CoPaw worker image from new installs, but an
# upgrade must not lose an existing deployment's private CoPaw worker image:
# legacy workers commonly carry an empty spec.image and resolve their image
# from the deployment env file. inherit_legacy_copaw_worker_image() must
# carry the pre-upgrade AGENTTEAMS_COPAW_WORKER_IMAGE forward so a
# post-upgrade wake/recreation keeps pulling the same image instead of the
# controller's built-in agentteams-copaw-worker:latest fallback.
# ---------------------------------------------------------------------------

eval "$(sed -n '/^inherit_legacy_copaw_worker_image()/,/^}/p' "${INSTALLER}")"
if ! declare -F inherit_legacy_copaw_worker_image >/dev/null; then
    echo "FAIL: function inherit_legacy_copaw_worker_image not found in installer" >&2
    exit 1
fi

log() { echo "$*"; }

LEGACY_IMAGE="private.registry.example/agentteams-copaw-worker:v1.2.3"
CO_PAW_ENV_FILE="$(mktemp)"
trap 'rm -f "${CO_PAW_ENV_FILE}"' EXIT

# case: upgrade inherits the non-empty legacy value
printf 'AGENTTEAMS_QWENPAW_IMAGE=old/qwenpaw:1.1\n' > "${CO_PAW_ENV_FILE}"
printf 'AGENTTEAMS_COPAW_WORKER_IMAGE=%s\n' "${LEGACY_IMAGE}" >> "${CO_PAW_ENV_FILE}"
COPAW_WORKER_IMAGE=""
inherit_legacy_copaw_worker_image "${CO_PAW_ENV_FILE}"
if [ "${COPAW_WORKER_IMAGE}" != "${LEGACY_IMAGE}" ]; then
    echo "FAIL: upgrade did not inherit legacy AGENTTEAMS_COPAW_WORKER_IMAGE (got '${COPAW_WORKER_IMAGE}')" >&2
    exit 1
fi
echo "ok: upgrade inherits non-empty legacy AGENTTEAMS_COPAW_WORKER_IMAGE"

# case: fresh install (no env file) stays image-less
COPAW_WORKER_IMAGE=""
inherit_legacy_copaw_worker_image ""
if [ -n "${COPAW_WORKER_IMAGE}" ]; then
    echo "FAIL: fresh install must not pick up a CoPaw worker image (got '${COPAW_WORKER_IMAGE}')" >&2
    exit 1
fi
echo "ok: fresh install (no env file) stays image-less"

# case: an explicit override always wins over the legacy value
COPAW_WORKER_IMAGE="override.registry.example/copaw:explicit"
inherit_legacy_copaw_worker_image "${CO_PAW_ENV_FILE}"
if [ "${COPAW_WORKER_IMAGE}" != "override.registry.example/copaw:explicit" ]; then
    echo "FAIL: explicit override was clobbered by legacy inheritance (got '${COPAW_WORKER_IMAGE}')" >&2
    exit 1
fi
echo "ok: explicit AGENTTEAMS_INSTALL_COPAW_WORKER_IMAGE override wins"

# case: an empty legacy value is NOT inherited
printf 'AGENTTEAMS_COPAW_WORKER_IMAGE=\n' > "${CO_PAW_ENV_FILE}"
COPAW_WORKER_IMAGE=""
inherit_legacy_copaw_worker_image "${CO_PAW_ENV_FILE}"
if [ -n "${COPAW_WORKER_IMAGE}" ]; then
    echo "FAIL: empty legacy value must not be inherited (got '${COPAW_WORKER_IMAGE}')" >&2
    exit 1
fi
echo "ok: empty legacy value is not inherited"

# case: the inheritance hook is wired into the upgrade branch of step_existing
# (capture the range before grepping: `sed | grep -q` under `set -o pipefail`
#  is racy — grep -q exits as soon as it matches, sed then dies on a broken
#  pipe and the pipeline reports failure even though the call is present)
step_existing_body="$(sed -n '/^step_existing()/,/^}/p' "${INSTALLER}")"
if ! grep -Fq 'inherit_legacy_copaw_worker_image "${existing_env}"' <<<"${step_existing_body}"; then
    echo "FAIL: step_existing upgrade branch does not call inherit_legacy_copaw_worker_image" >&2
    exit 1
fi
echo "ok: upgrade branch calls inherit_legacy_copaw_worker_image with the env file"

# case: the inherited value is written back into the new env file
if ! grep -Fq 'AGENTTEAMS_COPAW_WORKER_IMAGE=${COPAW_WORKER_IMAGE}' "${INSTALLER}"; then
    echo "FAIL: installer no longer writes AGENTTEAMS_COPAW_WORKER_IMAGE to the env file" >&2
    exit 1
fi
echo "ok: inherited image is written back to the env file"

echo "PASS: legacy CoPaw worker image inheritance (upgrade path) verified"
