#!/bin/bash
# test-install-storage-env.sh — regression suite for the installer's
# controller storage env (AGENTTEAMS_STORAGE_PROVIDER=minio|s3 + FS_*
# overrides), exercised through the read-only --print-controller-env
# mode (AGENTTEAMS_PRINT_CONTROLLER_ENV=1).
#
# Pure bash + coreutils; no docker, no root, no network.
#
# Usage:  bash install/tests/test-install-storage-env.sh
# Exit:   0 if every scenario passes, 1 otherwise.

set -u

# Locate the repo root from this file's own path (install/tests/ -> ../..);
# never depend on the caller's cwd.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
INSTALLER="${REPO_ROOT}/install/agentteams-install.sh"

if [ ! -f "${INSTALLER}" ]; then
    echo "FAIL setup (install/agentteams-install.sh not found: ${INSTALLER})"
    exit 1
fi

# Run the installer in read-only storage-env preview mode. Storage and
# version vars inherited from the caller are stripped first so every
# scenario is deterministic. Combined stdout+stderr in $RUN_OUT, exit
# code in $RUN_RC.
RUN_RC=0
RUN_OUT=""
run_env() {
    RUN_OUT="$(
        env -u AGENTTEAMS_STORAGE_PROVIDER \
            -u AGENTTEAMS_FS_ENDPOINT \
            -u AGENTTEAMS_FS_BUCKET \
            -u AGENTTEAMS_FS_ACCESS_KEY \
            -u AGENTTEAMS_FS_SECRET_KEY \
            -u AGENTTEAMS_MINIO_USER \
            -u AGENTTEAMS_MINIO_PASSWORD \
            -u AGENTTEAMS_VERSION \
            "$@" \
            AGENTTEAMS_PRINT_CONTROLLER_ENV=1 \
            bash "${INSTALLER}" 2>&1
    )"
    RUN_RC=$?
}

SUITE_FAIL=0
PASS_COUNT=0
problems=""

note_problem() {
    problems="${problems}$1; "
}

# finish NAME — PASS if no problems were noted, FAIL otherwise.
finish() {
    local name="$1"
    if [ -z "${problems}" ]; then
        echo "PASS $name"
        PASS_COUNT=$((PASS_COUNT + 1))
    else
        echo "FAIL $name (${problems})"
        printf '%s\n' "${RUN_OUT}" | sed 's/^/    | /'
        SUITE_FAIL=1
    fi
    problems=""
}

# contains NEEDLE — assert the preview output carries NEEDLE (fixed string).
contains() {
    printf '%s\n' "${RUN_OUT}" | grep -qF "$1" || note_problem "missing $1"
}

# not_contains NEEDLE — assert the preview output does NOT carry NEEDLE.
not_contains() {
    if printf '%s\n' "${RUN_OUT}" | grep -qF "$1"; then
        note_problem "unexpected $1"
    fi
}

# every_line_is_env_arg — every line of the output is one "-e ..." arg.
every_line_is_env_arg() {
    local total env_lines
    total="$(printf '%s\n' "${RUN_OUT}" | wc -l)"
    env_lines="$(printf '%s\n' "${RUN_OUT}" | grep -c '^-e ')"
    [ "${total}" -eq "${env_lines}" ] \
        || note_problem "only ${env_lines} of ${total} lines are -e args"
}

# 1. Default (no storage env): embedded MinIO, modern prefix, rc=0.
run_env
problems=""
[ "${RUN_RC}" = "0" ] || note_problem "rc=${RUN_RC} (want 0)"
contains '-e AGENTTEAMS_STORAGE_PROVIDER=minio'
contains '-e AGENTTEAMS_FS_ENDPOINT=http://127.0.0.1:9000'
contains '-e AGENTTEAMS_FS_BUCKET=agentteams-storage'
finish "default_minio"

# 2. provider=s3, full config: all eight keys, external values mirrored
#    into MINIO_*, and no 127.0.0.1 fallback left behind.
run_env \
    AGENTTEAMS_STORAGE_PROVIDER=s3 \
    AGENTTEAMS_FS_ENDPOINT=http://rustfs:9000 \
    AGENTTEAMS_FS_BUCKET=b \
    AGENTTEAMS_FS_ACCESS_KEY=ak \
    AGENTTEAMS_FS_SECRET_KEY=sk
problems=""
[ "${RUN_RC}" = "0" ] || note_problem "rc=${RUN_RC} (want 0)"
contains '-e AGENTTEAMS_STORAGE_PROVIDER=s3'
contains '-e AGENTTEAMS_MINIO_ENDPOINT=http://rustfs:9000'
contains '-e AGENTTEAMS_MINIO_BUCKET=b'
contains '-e AGENTTEAMS_FS_ENDPOINT=http://rustfs:9000'
contains '-e AGENTTEAMS_FS_BUCKET=b'
contains '-e AGENTTEAMS_FS_ACCESS_KEY=ak'
contains '-e AGENTTEAMS_FS_SECRET_KEY=sk'
contains '-e AGENTTEAMS_STORAGE_PREFIX=agentteams/agentteams-storage'
not_contains 'FS_ENDPOINT=http://127.0.0.1:9000'
finish "s3_full"

# 3. provider=s3 with the secret key missing: non-zero rc, names the var.
run_env \
    AGENTTEAMS_STORAGE_PROVIDER=s3 \
    AGENTTEAMS_FS_ENDPOINT=http://rustfs:9000 \
    AGENTTEAMS_FS_BUCKET=b \
    AGENTTEAMS_FS_ACCESS_KEY=ak
problems=""
[ "${RUN_RC}" != "0" ] || note_problem "rc=0 (want non-zero)"
contains 'AGENTTEAMS_FS_SECRET_KEY'
finish "s3_missing_secret"

# 4. provider=s3 with an endpoint that lacks a scheme: non-zero rc,
#    the hint shows the expected http:// form.
run_env \
    AGENTTEAMS_STORAGE_PROVIDER=s3 \
    AGENTTEAMS_FS_ENDPOINT=rustfs:9000 \
    AGENTTEAMS_FS_BUCKET=b \
    AGENTTEAMS_FS_ACCESS_KEY=ak \
    AGENTTEAMS_FS_SECRET_KEY=sk
problems=""
[ "${RUN_RC}" != "0" ] || note_problem "rc=0 (want non-zero)"
contains 'http://'
finish "s3_endpoint_no_scheme"

# 5. Invalid provider value: non-zero rc.
run_env AGENTTEAMS_STORAGE_PROVIDER=foo
problems=""
[ "${RUN_RC}" != "0" ] || note_problem "rc=0 (want non-zero)"
finish "invalid_provider"

# 6. Output shape of a successful preview: every line is one -e arg
#    (checked for both the default and the s3 configuration).
run_env
problems=""
every_line_is_env_arg
finish "preview_line_format"

run_env \
    AGENTTEAMS_STORAGE_PROVIDER=s3 \
    AGENTTEAMS_FS_ENDPOINT=http://rustfs:9000 \
    AGENTTEAMS_FS_BUCKET=b \
    AGENTTEAMS_FS_ACCESS_KEY=ak \
    AGENTTEAMS_FS_SECRET_KEY=sk
problems=""
every_line_is_env_arg
finish "preview_line_format_s3"

echo ""
if [ "${SUITE_FAIL}" = "0" ]; then
    echo "All ${PASS_COUNT} scenarios passed."
    exit 0
else
    echo "One or more scenarios FAILED."
    exit 1
fi
