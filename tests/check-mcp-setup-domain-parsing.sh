#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="${ROOT_DIR}/manager/agent/skills/mcp-server-management/scripts/setup-mcp-server.sh"
PROXY_SCRIPT="${ROOT_DIR}/manager/agent/skills/mcp-server-management/scripts/setup-mcp-proxy.sh"
DOC="${ROOT_DIR}/manager/agent/skills/mcp-server-management/references/api-commands.md"

# Regression for issue #1284: a scheme-less --api-domain with an explicit
# port (e.g. host:8080) used to register silently as https, so plain-HTTP
# internal gateways failed with 503s far from the root cause. The resolved
# protocol must now be visible in the NOTE and in the service-source log.

bash -n "${SCRIPT}"
bash -n "${PROXY_SCRIPT}"

# Both registration paths must log the resolved protocol next to host:port.
grep -q 'Registering ${SVC_SOURCE_NAME} DNS service source (${URL_PROTO}://${API_DOMAIN}:${URL_PORT})' "${SCRIPT}" || {
    echo "FAIL: setup-mcp-server.sh must log the resolved protocol for the service source" >&2
    exit 1
}
grep -q 'Registering ${SVC_SOURCE_NAME} DNS service source (${URL_PROTO}://${API_DOMAIN}:${URL_PORT})' "${PROXY_SCRIPT}" || {
    echo "FAIL: setup-mcp-proxy.sh must log the resolved protocol for the service source" >&2
    exit 1
}
grep -q 'NOTE: no scheme given' "${SCRIPT}" || {
    echo "FAIL: setup-mcp-server.sh must warn when a scheme-less --api-domain carries an explicit port" >&2
    exit 1
}

# The list → PUT data-loss footgun must be documented for manual edits.
grep -q 'rawConfigurations' "${DOC}" || {
    echo "FAIL: api-commands.md must document the rawConfigurations list-to-PUT footgun" >&2
    exit 1
}

# ── Functional test of the --api-domain parsing block ──────────────────────

workdir="$(mktemp -d)"
trap 'rm -rf "${workdir}"' EXIT
RESULT_FILE="${workdir}/result"
NOTE_FILE="${workdir}/note"

# The parsing block exits on invalid input and only the NOTE is logged, so
# run it in a subshell: `exit 1` then only terminates the subshell, and the
# parsed triple / NOTE survive via files instead of (sub)shell variables.
log() { echo "$*" >> "${NOTE_FILE}"; }

run_parse() {
    local input="$1"
    : > "${NOTE_FILE}"
    (
        EXPLICIT_API_DOMAIN="${input}"
        API_DOMAIN=""
        URL_PROTO="https"
        URL_PORT=443
        eval "$(awk '
            /^API_DOMAIN=""$/ {flag=1}
            /^if \[ -z/ {flag=0}
            flag
        ' "${SCRIPT}")"
        printf '%s:%s:%s\n' "${URL_PROTO}" "${URL_PORT}" "${API_DOMAIN}" > "${RESULT_FILE}"
    )
}

assert_parse() {
    local input="$1" want_proto="$2" want_port="$3" want_domain="$4"
    local note_expected="$5"
    local parsed=""
    if run_parse "${input}"; then
        parsed="$(cat "${RESULT_FILE}")"
        if [ "${parsed}" != "${want_proto}:${want_port}:${want_domain}" ]; then
            echo "FAIL: '${input}' parsed to ${parsed}, want ${want_proto}:${want_port}:${want_domain}" >&2
            exit 1
        fi
        if [ "${note_expected}" = "yes" ] && ! grep -q "NOTE: no scheme given" "${NOTE_FILE}"; then
            echo "FAIL: '${input}' must print the scheme-less NOTE" >&2
            exit 1
        fi
        if [ "${note_expected}" = "no" ] && grep -q "NOTE: no scheme given" "${NOTE_FILE}"; then
            echo "FAIL: '${input}' must not print the scheme-less NOTE" >&2
            exit 1
        fi
    else
        if [ "${want_proto}" != "error" ]; then
            echo "FAIL: '${input}' unexpectedly failed" >&2
            exit 1
        fi
    fi
}

# Scheme-less with explicit port: https by default, with the NOTE.
assert_parse "tools.example.local:8080" "https" "8080" "tools.example.local" "yes"
# Explicit scheme wins, port included or default.
assert_parse "http://tools.example.local:8080" "http" "8080" "tools.example.local" "no"
assert_parse "http://tools.example.local" "http" "80" "tools.example.local" "no"
assert_parse "https://tools.example.local" "https" "443" "tools.example.local" "no"
# Single-label and malformed inputs must fail with an actionable error.
assert_parse "singlelabel" "error" "" "" ""
assert_parse "bad host/path" "error" "" "" ""
# Leading-zero ports are still numeric (bash 10# coercion).
assert_parse "tools.example.local:0080" "https" "80" "tools.example.local" "yes"

echo "PASS: --api-domain parsing surfaces the resolved protocol (#1284)"
