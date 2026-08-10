#!/bin/bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

test_merges_manager_fields_with_jq_1_7() {
    local tmpdir remote local_config
    tmpdir="$(mktemp -d)"
    remote="${tmpdir}/remote.json"
    local_config="${tmpdir}/local.json"

    printf '%s\n' \
        '{"models":{"remote":true},"channels":{"matrix":{"homeserver":"https://matrix.example"}}}' \
        > "${remote}"
    printf '%s\n' \
        '{"models":{"local":true},"channels":{"matrix":{"accessToken":"worker-token"}},"tools":{"local":true}}' \
        > "${local_config}"

    # shellcheck source=../lib/merge-openclaw-config.sh
    source "${REPO_ROOT}/shared/lib/merge-openclaw-config.sh"
    merge_openclaw_config "${remote}" "${local_config}"

    jq -e '
        .models.remote == true
        and .channels.matrix.homeserver == "https://matrix.example"
        and .channels.matrix.accessToken == "worker-token"
        and .tools.local == true
    ' "${local_config}" >/dev/null || fail "merged config did not preserve the local-first contract"

    rm -rf "${tmpdir}"
}

test_invalid_remote_config_is_recoverable_under_errexit() {
    local tmpdir remote local_config output
    tmpdir="$(mktemp -d)"
    remote="${tmpdir}/remote.json"
    local_config="${tmpdir}/local.json"
    printf 'not-json\n' > "${remote}"
    printf '{"tools":{"local":true}}\n' > "${local_config}"

    output="$(
        REMOTE="${remote}" LOCAL_CONFIG="${local_config}" MERGE_LIB="${REPO_ROOT}/shared/lib/merge-openclaw-config.sh" \
            bash -e -c '
                source "${MERGE_LIB}"
                if ! merge_openclaw_config "${REMOTE}" "${LOCAL_CONFIG}"; then
                    printf "fallback-continues\n"
                fi
            '
    )"

    [ "${output}" = "fallback-continues" ] ||
        fail "merge failure terminated an errexit-enabled fallback"
    jq -e '.tools.local == true' "${local_config}" >/dev/null ||
        fail "merge failure modified the local config"

    rm -rf "${tmpdir}"
}

test_merges_manager_fields_with_jq_1_7
test_invalid_remote_config_is_recoverable_under_errexit

echo "PASS: merge-openclaw-config"
