#!/usr/bin/env bash
set -eo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INSTALLER="${ROOT_DIR}/.installer-source/public/install.sh"
PRIMARY="higress-registry.cn-hangzhou.cr.aliyuncs.com"
workdir="$(mktemp -d)"
trap 'rm -rf "${workdir}"' EXIT
ENV_FILE="${workdir}/manager.env"
AGENTTEAMS_ENV_FILE="${ENV_FILE}"
eval "$(sed -n '/^load_current_params_from_env() {/,/^}/p' "${INSTALLER}")"
default_line="$(grep '^    AGENTTEAMS_HIGRESS_WASM_PLUGIN_REGISTRY=' "${INSTALLER}" || true)"
env_block="$(sed -n '/^    cat > "${ENV_FILE}" << EOF$/,/^EOF$/p' "${INSTALLER}")"
ctrl_block="$(sed -n '/^        local _ctrl_env_args=($/,/^        )$/p' "${INSTALLER}")"
[ -n "${env_block}" ] && [ -n "${ctrl_block}" ]
log() { :; }
msg() { :; }

check_registry() {
    AGENTTEAMS_REGISTRY="$1"
    AGENTTEAMS_HIGRESS_WASM_PLUGIN_REGISTRY="$2"
    local expected="$3"
    eval "${default_line}"
    eval "${env_block}"
    grep -Fxq "HIGRESS_ADMIN_WASM_PLUGIN_IMAGE_REGISTRY=${expected}" "${ENV_FILE}" || {
        echo "FAIL: generated plugin registry must be ${expected}"
        grep '^HIGRESS_ADMIN_WASM_PLUGIN_IMAGE_REGISTRY=' "${ENV_FILE}"
        exit 1
    }
    grep -Fxq "AGENTTEAMS_HIGRESS_WASM_PLUGIN_REGISTRY=${expected}" "${ENV_FILE}"
    # Execute the embedded Controller's original argument construction.
    local _ctrl_env_prefix=AGENTTEAMS_
    WORKER_IMAGE="${AGENTTEAMS_REGISTRY}/agentteams/agentteams-harness-openclaw:test"
    eval "${ctrl_block}"
    printf '%s\n' "${_ctrl_env_args[@]}" | grep -Fxq "HIGRESS_ADMIN_WASM_PLUGIN_IMAGE_REGISTRY=${expected}"
    printf '%s\n' "${_ctrl_env_args[@]}" | grep -Fxq "AGENTTEAMS_WORKER_IMAGE=${WORKER_IMAGE}"
}

check_registry higress-registry.us-west-1.cr.aliyuncs.com '' "${PRIMARY}"
check_registry higress-registry.ap-southeast-7.cr.aliyuncs.com '' "${PRIMARY}"
check_registry images.example.com plugins.example.com plugins.example.com
unset AGENTTEAMS_HIGRESS_WASM_PLUGIN_REGISTRY
# Match the installer's plain set -e semantics when older fields are absent.
set +o pipefail
load_current_params_from_env
set -o pipefail
[ "${AGENTTEAMS_HIGRESS_WASM_PLUGIN_REGISTRY}" = plugins.example.com ]
AGENTTEAMS_HIGRESS_WASM_PLUGIN_REGISTRY=override.example.com
set +o pipefail
load_current_params_from_env
set -o pipefail
[ "${AGENTTEAMS_HIGRESS_WASM_PLUGIN_REGISTRY}" = override.example.com ]
echo 'PASS: Bash env persistence and embedded WASM forwarding, with independent image registry'
