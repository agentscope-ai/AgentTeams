#!/bin/bash
# Regression tests for shared/lib/oss-credentials.sh.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
TMPDIR_ROOT="$(mktemp -d)"
trap 'rm -rf "${TMPDIR_ROOT}"' EXIT

pass() { echo "  PASS: $1"; }

assert_contains() {
    local desc="$1" needle="$2" haystack="$3"
    if printf '%s' "${haystack}" | grep -qF -- "${needle}"; then
        pass "${desc}"
    else
        echo "  FAIL: ${desc}" >&2
        echo "       expected to contain: ${needle}" >&2
        echo "       got: ${haystack}" >&2
        exit 1
    fi
}

assert_not_contains() {
    local desc="$1" needle="$2" haystack="$3"
    if ! printf '%s' "${haystack}" | grep -qF -- "${needle}"; then
        pass "${desc}"
    else
        echo "  FAIL: ${desc}" >&2
        echo "       expected not to contain: ${needle}" >&2
        echo "       got: ${haystack}" >&2
        exit 1
    fi
}

create_mock_tools() {
    local mockbin="$1"
    mkdir -p "${mockbin}"

    cat > "${mockbin}/curl" <<'MOCK_CURL'
#!/bin/sh
set -eu
printf '%s\n' "$*" >> "${TEST_CURL_LOG:?}"
cat <<'OUT'
{"access_key_id":"test-ak","access_key_secret":"test-sk","security_token":"test-token","oss_endpoint":"oss.example.test"}
200
OUT
MOCK_CURL
    chmod +x "${mockbin}/curl"

    cat > "${mockbin}/jq" <<'MOCK_JQ'
#!/bin/sh
set -eu
if [ "${1:-}" = "-r" ]; then
    shift
fi
cat >/dev/null
case "${1:-}" in
    .access_key_id) echo "test-ak" ;;
    .access_key_secret) echo "test-sk" ;;
    .security_token) echo "test-token" ;;
    .oss_endpoint) echo "oss.example.test" ;;
    *) echo "null" ;;
esac
MOCK_JQ
    chmod +x "${mockbin}/jq"
}

run_refresh() {
    local case_name="$1"
    local mockbin="${TMPDIR_ROOT}/${case_name}-bin"
    local curl_log="${TMPDIR_ROOT}/${case_name}-curl.log"
    create_mock_tools "${mockbin}"

    (
        . "${PROJECT_ROOT}/shared/lib/oss-credentials.sh"
        _OSS_CRED_FILE="${TMPDIR_ROOT}/${case_name}-mc.env"
        export PATH="${mockbin}:${PATH}"
        export TEST_CURL_LOG="${curl_log}"
        export AGENTTEAMS_CONTROLLER_URL="http://controller:8090"
        export AGENTTEAMS_AUTH_TOKEN="controller-token"
        _oss_refresh_sts_via_controller >/dev/null
    )

    cat "${curl_log}"
}

echo ""
echo "=== oss credentials STS controller request ==="

request="$(run_refresh sts-request)"
assert_not_contains "cluster header should not be sent" "X-AgentTeams-Cluster-ID:" "${request}"
assert_contains "bearer token should be sent" "Authorization: Bearer controller-token" "${request}"

echo "All oss-credentials tests passed"
