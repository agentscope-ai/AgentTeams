#!/bin/bash
# Unit checks for shared/lib/agentteams-env.sh sandbox worker env loading.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

run_env_source() {
    env -i \
        PATH="${PATH}" \
        AGENTTEAMS_WORKER_ENV_MOUNT_REQUIRED=1 \
        AGENTTEAMS_WORKER_ENV_MOUNT_DIR="$1" \
        AGENTTEAMS_WORKER_ENV_MOUNT_TIMEOUT_SECONDS="$2" \
        bash -c ". '${REPO_ROOT}/shared/lib/agentteams-env.sh'; printf 'worker=%s token=%s\n' \"\${AGENTTEAMS_WORKER_NAME:-}\" \"\${AGENTTEAMS_AUTH_TOKEN_FILE:-}\""
}

test_waits_for_required_worker_env_values() {
    local tmpdir mount token output updater_pid
    tmpdir="$(mktemp -d)"
    mount="${tmpdir}/env"
    token="${tmpdir}/token"
    mkdir -p "${mount}"
    printf 'token\n' > "${token}"
    printf "export AGENTTEAMS_AUTH_TOKEN_FILE='%s'\n" "${token}" > "${mount}/env"

    (
        sleep 1
        {
            printf "export AGENTTEAMS_AUTH_TOKEN_FILE='%s'\n" "${token}"
            printf "export AGENTTEAMS_WORKER_NAME='alice'\n"
        } > "${mount}/env"
    ) &
    updater_pid=$!

    output="$(run_env_source "${mount}" 4)"
    wait "${updater_pid}"

    rm -rf "${tmpdir}"
    [ "${output}" = "worker=alice token=${token}" ] || fail "unexpected delayed env output: ${output}"
}

test_times_out_when_required_worker_env_values_never_arrive() {
    local tmpdir mount token output rc
    tmpdir="$(mktemp -d)"
    mount="${tmpdir}/env"
    token="${tmpdir}/token"
    mkdir -p "${mount}"
    printf 'token\n' > "${token}"
    printf "export AGENTTEAMS_AUTH_TOKEN_FILE='%s'\n" "${token}" > "${mount}/env"

    set +e
    output="$(run_env_source "${mount}" 1 2>&1)"
    rc=$?
    set -e

    rm -rf "${tmpdir}"
    [ "${rc}" -ne 0 ] || fail "expected timeout failure"
    grep -q "\[agentteams-env\] ERROR" <<<"${output}" || fail "timeout did not include agentteams-env error: ${output}"
    grep -q "AGENTTEAMS_WORKER_NAME" <<<"${output}" || fail "timeout did not name missing worker env: ${output}"
}

test_legacy_hiclaw_env_maps_to_agentteams_contract() {
    local output
    output="$(
        env -i \
            PATH="${PATH}" \
            AGENTTEAMS_WORKER_NAME=legacy-worker \
            AGENTTEAMS_AUTH_TOKEN_FILE=/var/run/secrets/hiclaw/token \
            AGENTTEAMS_CONTROLLER_URL=http://controller:8090 \
            AGENTTEAMS_FS_BUCKET=agentteams-storage \
            AGENTTEAMS_STORAGE_PREFIX=agentteams/agentteams-storage \
            bash -c ". '${REPO_ROOT}/shared/lib/agentteams-env.sh'; printf 'worker=%s token=%s controller=%s alias=%s prefix=%s\n' \"\${AGENTTEAMS_WORKER_NAME:-}\" \"\${AGENTTEAMS_AUTH_TOKEN_FILE:-}\" \"\${AGENTTEAMS_CONTROLLER_URL:-}\" \"\${AGENTTEAMS_STORAGE_ALIAS:-}\" \"\${AGENTTEAMS_STORAGE_PREFIX:-}\""
    )"

    [ "${output}" = "worker=legacy-worker token=/var/run/secrets/hiclaw/token controller=http://controller:8090 alias=agentteams prefix=agentteams/agentteams-storage" ] || \
        fail "legacy HICLAW env did not map to AgentTeams contract: ${output}"
}

test_waits_for_required_worker_env_values
test_times_out_when_required_worker_env_values_never_arrive
test_legacy_hiclaw_env_maps_to_agentteams_contract

echo "PASS: agentteams-env sandbox worker env loading"
