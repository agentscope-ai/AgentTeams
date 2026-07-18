#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
VERIFY_SCRIPT="${ROOT_DIR}/install/hiclaw-verify.sh"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/agentteams-verify-routing.XXXXXX")"
trap 'rm -rf "${TEST_ROOT}"' EXIT

FAKE_BIN="${TEST_ROOT}/bin"
CAPTURE="${TEST_ROOT}/exec-calls"
mkdir -p "${FAKE_BIN}"

cat > "${FAKE_BIN}/docker" <<'EOF'
#!/usr/bin/env bash
set -e

mode="${VERIFY_FAKE_MODE:?}"
case "${1:-}" in
    version)
        exit 0
        ;;
    ps)
        if [ "${mode}" = "embedded" ]; then
            printf '%s\n' agentteams-controller agentteams-manager
        else
            printf '%s\n' agentteams-manager
        fi
        exit 0
        ;;
    port)
        container="${2:-}"
        container_port="${3:-}"
        printf 'port %s %s\n' "${container}" "${container_port}" >> "${VERIFY_FAKE_CAPTURE}"
        case "${container_port}" in
            8080/tcp) printf '127.0.0.1:19080\n' ;;
            8001/tcp) printf '127.0.0.1:19001\n' ;;
            *) exit 1 ;;
        esac
        exit 0
        ;;
    exec)
        container="${2:-}"
        command="${3:-}"
        case "${command}" in
            printenv)
                if [ "${container}" = "agentteams-manager" ]; then
                    printf 'AGENTTEAMS_MANAGER_RUNTIME=copaw\n'
                    if [ "${mode}" = "legacy" ]; then
                        printf 'AGENTTEAMS_PORT_GATEWAY=18080\n'
                        printf 'AGENTTEAMS_PORT_CONSOLE=18001\n'
                    fi
                elif [ "${container}" = "agentteams-controller" ]; then
                    :
                fi
                exit 0
                ;;
            curl)
                url="${!#}"
                printf '%s %s\n' "${container}" "${url}" >> "${VERIFY_FAKE_CAPTURE}"
                case "${url}" in
                    *:9000/minio/health/live|*:6167/_matrix/client/versions)
                        if { [ "${mode}" = "embedded" ] && [ "${container}" = "agentteams-controller" ]; } || \
                            { [ "${mode}" = "legacy" ] && [ "${container}" = "agentteams-manager" ]; }; then
                            printf '200'
                        else
                            printf '000'
                        fi
                        ;;
                    *:18799/health)
                        if [ "${container}" = "agentteams-manager" ]; then
                            printf '200'
                        else
                            printf '000'
                        fi
                        ;;
                    *)
                        printf '000'
                        ;;
                esac
                exit 0
                ;;
        esac
        ;;
esac

printf 'unexpected docker invocation: %s\n' "$*" >&2
exit 1
EOF

cat > "${FAKE_BIN}/curl" <<'EOF'
#!/usr/bin/env bash
printf 'host %s\n' "${!#}" >> "${VERIFY_FAKE_CAPTURE}"
printf '200'
EOF

chmod +x "${FAKE_BIN}/docker" "${FAKE_BIN}/curl"

PASS=0
FAIL=0
RUN_OUTPUT=""
RUN_RC=0

pass() {
    printf 'PASS: %s\n' "$1"
    PASS=$((PASS + 1))
}

fail() {
    printf 'FAIL: %s\n' "$1" >&2
    FAIL=$((FAIL + 1))
}

run_verify() {
    local mode="$1"
    rm -f "${CAPTURE}"
    set +e
    RUN_OUTPUT="$(
        env \
            PATH="${FAKE_BIN}:${PATH}" \
            VERIFY_FAKE_MODE="${mode}" \
            VERIFY_FAKE_CAPTURE="${CAPTURE}" \
            AGENTTEAMS_VERIFY_INFRA_CONTAINER=agentteams-controller \
            bash "${VERIFY_SCRIPT}" 2>&1
    )"
    RUN_RC=$?
    set -e
}

run_verify embedded
if [ "${RUN_RC}" -eq 0 ] \
    && [[ "${RUN_OUTPUT}" == *"Result: 6/6 passed"* ]] \
    && grep -Fq 'agentteams-controller http://127.0.0.1:9000/minio/health/live' "${CAPTURE}" \
    && grep -Fq 'agentteams-controller http://127.0.0.1:6167/_matrix/client/versions' "${CAPTURE}" \
    && grep -Fq 'agentteams-manager http://127.0.0.1:18799/health' "${CAPTURE}" \
    && grep -Fq 'host http://127.0.0.1:19080/' "${CAPTURE}" \
    && grep -Fq 'host http://127.0.0.1:19001/' "${CAPTURE}"; then
    pass "embedded verification routes infrastructure and Agent checks correctly"
else
    fail "embedded verification routing: rc=${RUN_RC}, output=${RUN_OUTPUT}"
fi

run_verify legacy
if [ "${RUN_RC}" -eq 0 ] \
    && [[ "${RUN_OUTPUT}" == *"Result: 6/6 passed"* ]] \
    && grep -Fq 'agentteams-manager http://127.0.0.1:9000/minio/health/live' "${CAPTURE}" \
    && grep -Fq 'agentteams-manager http://127.0.0.1:6167/_matrix/client/versions' "${CAPTURE}" \
    && ! grep -Fq 'agentteams-controller ' "${CAPTURE}"; then
    pass "legacy verification keeps infrastructure checks in the Manager container"
else
    fail "legacy verification routing: rc=${RUN_RC}, output=${RUN_OUTPUT}"
fi

printf '\nResults: %d passed, %d failed\n' "${PASS}" "${FAIL}"
if [ "${FAIL}" -ne 0 ]; then
    exit 1
fi
