#!/bin/bash
# test-register-provider.sh
#
# Static (offline) tests for the provider-management skill's
# register-provider.sh (plan v2.3 Phase 2b step 4, decision #7;
# docs/implementation-milestone-3.md Step 5). Shims curl on PATH to record
# every Higress Console API call the script makes (no network/binary
# needed), then asserts:
#
#   1. Fresh name -> POST source+provider+route with the 5c body shapes and
#      route name hiclaw-<name>-route.
#   2. Existing name -> PUTs (idempotent upsert), same shapes.
#   3. Name with a slash, or missing key, is a hard error before any call.
#   4. Stale cookie (shim returns the session-expired HTML page) -> exactly
#      one re-login, then the call retried successfully.
#   5. --delete reverses all three (route, provider, service-source DELETEs).
#   6. The call log never contains default-ai-route.
#   7. The key never appears in stdout.
#
# Usage: bash manager/tests/test-register-provider.sh
# (intended to run inside an alpine container with `bash` + `jq` installed;
#  CRLF is stripped below so it also runs unmodified from a checkout with
#  autocrlf=true — see docs/implementation-milestone-3.md Toolchain section.)

set -uo pipefail

PASS=0
FAIL=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
TARGET_SCRIPT="${PROJECT_ROOT}/manager/agent/skills/provider-management/scripts/register-provider.sh"

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; echo "       expected: $2"; echo "       got:      $3"; FAIL=$((FAIL + 1)); }

assert_eq() {
    local desc="$1" expected="$2" actual="$3"
    if [ "${expected}" = "${actual}" ]; then
        pass "${desc}"
    else
        fail "${desc}" "${expected}" "${actual}"
    fi
}

assert_contains() {
    local desc="$1" needle="$2" haystack="$3"
    if printf '%s' "${haystack}" | grep -qF -- "${needle}"; then
        pass "${desc}"
    else
        fail "${desc}" "contains '${needle}'" "not found: ${haystack}"
    fi
}

assert_not_contains() {
    local desc="$1" needle="$2" haystack="$3"
    if ! printf '%s' "${haystack}" | grep -qF -- "${needle}"; then
        pass "${desc}"
    else
        fail "${desc}" "should NOT contain '${needle}'" "found it"
    fi
}

TMPDIR_ROOT=$(mktemp -d)
trap 'rm -rf "${TMPDIR_ROOT}"' EXIT

# ── Fake /opt/hiclaw tree the script sources from ────────────────────────────
FAKE_ROOT="${TMPDIR_ROOT}/fake-opt-hiclaw"
mkdir -p "${FAKE_ROOT}/scripts/lib"
cp "${PROJECT_ROOT}/manager/scripts/lib/base.sh" "${FAKE_ROOT}/scripts/lib/base.sh"
sed -i 's/\r$//' "${FAKE_ROOT}/scripts/lib/base.sh"
cp "${PROJECT_ROOT}/manager/scripts/lib/gateway-api.sh" "${FAKE_ROOT}/scripts/lib/gateway-api.sh"
sed -i 's/\r$//' "${FAKE_ROOT}/scripts/lib/gateway-api.sh"

# Copy the target script under test, strip CRLF (checkout is autocrlf=true),
# and rewrite the two hardcoded source paths so they resolve inside FAKE_ROOT.
RUN_SCRIPT="${TMPDIR_ROOT}/register-provider.sh"
sed 's/\r$//' "${TARGET_SCRIPT}" > "${RUN_SCRIPT}"
sed -i "s#source /opt/hiclaw/scripts/lib/base.sh#source ${FAKE_ROOT}/scripts/lib/base.sh#" "${RUN_SCRIPT}"
sed -i "s#source /opt/hiclaw/scripts/lib/gateway-api.sh#source ${FAKE_ROOT}/scripts/lib/gateway-api.sh#" "${RUN_SCRIPT}"
chmod +x "${RUN_SCRIPT}"

# bash -n sanity check up front (also covered at the milestone level, but
# cheap to assert here too since we already have the stripped copy).
if bash -n "${RUN_SCRIPT}" 2>/tmp/bashn.err; then
    pass "bash -n: register-provider.sh has valid syntax"
else
    fail "bash -n: register-provider.sh has valid syntax" "no syntax errors" "$(cat /tmp/bashn.err)"
fi

# ── PATH shims ────────────────────────────────────────────────────────────
BIN_DIR="${TMPDIR_ROOT}/bin"
mkdir -p "${BIN_DIR}"
export CALL_LOG="${TMPDIR_ROOT}/calls.log"
# Controls how many times, from the start, the curl shim answers a GET with
# the HTML session-expired page (simulating a stale cookie). Consumed by
# call index across the whole run (GETs and non-GETs both count) — tests
# that need this set it low and deliberately only rely on the first GET.
export STALE_GET_COUNT="${STALE_GET_COUNT:-0}"

cat > "${BIN_DIR}/curl" <<'CURL_EOF'
#!/bin/bash
# Records method/path/body of every Higress Console call.
# GET behavior:
#   - if a counter file (STALE_COUNTER_FILE) has remaining "stale" credits,
#     return the session-expired HTML page (simulates a stale cookie) and
#     decrement the credit.
#   - otherwise return 200 with an empty JSON body (so higress_get's
#     GET->PUT/POST branch in the caller takes the POST/create path, unless
#     a test pre-seeds a canned response via CANNED_GET_BODY/CANNED_GET_PATH).
# DELETE behavior:
#   - also honors STALE_COUNTER_FILE (stale-cookie shim covers the delete
#     path's session-aware higress_delete helper too), decrementing the same
#     shared credit as GET.
#   - otherwise 200 with an empty body ("deleted"/"absent", either way a
#     success from higress_delete's point of view).
# POST/PUT always "succeed" with success:true so the script's branch logic
# proceeds; the point of this harness is the call shape, not real Higress
# semantics.
method="GET"
path=""
outfile=""
write_out=""
body=""
args=("$@")
i=0
while [ $i -lt ${#args[@]} ]; do
    case "${args[$i]}" in
        -X) i=$((i+1)); method="${args[$i]}" ;;
        -o) i=$((i+1)); outfile="${args[$i]}" ;;
        -w) i=$((i+1)); write_out="${args[$i]}" ;;
        -d|--data) i=$((i+1)); if [ "${args[$i]}" = "@-" ]; then body=$(cat); else body="${args[$i]}"; fi ;;
        http*://*) path="${args[$i]}" ;;
    esac
    i=$((i+1))
done
{
    echo "CURL method=${method} path=${path}"
    [ -n "${body:-}" ] && echo "CURL body=${body}"
} >> "${CALL_LOG:-/dev/null}"

resp_body=""
http_code="200"

if [ "${method}" = "GET" ] || [ "${method}" = "DELETE" ]; then
    remaining=0
    if [ -n "${STALE_COUNTER_FILE:-}" ] && [ -f "${STALE_COUNTER_FILE}" ]; then
        remaining=$(cat "${STALE_COUNTER_FILE}")
    fi
    if [ "${remaining}" -gt 0 ] 2>/dev/null; then
        resp_body='<!DOCTYPE html><html><body>login</body></html>'
        remaining=$((remaining - 1))
        echo "${remaining}" > "${STALE_COUNTER_FILE}"
    elif [ "${method}" = "GET" ] && [ "${path}" = "${CANNED_GET_PATH:-__none__}" ]; then
        resp_body="${CANNED_GET_BODY:-}"
    elif [ "${method}" = "GET" ]; then
        resp_body=""
        http_code="404"
    else
        # DELETE, no stale credit remaining: succeed with an empty 200 body.
        resp_body=""
        http_code="200"
    fi
elif [ "${method}" = "POST" ] && [[ "${path}" == *"/session/login"* ]]; then
    resp_body=""
    http_code="200"
else
    resp_body='{"success":true}'
    http_code="200"
fi

if [ -n "${outfile}" ]; then
    printf '%s' "${resp_body}" > "${outfile}"
fi
if [ -n "${write_out}" ]; then
    printf '%s' "${http_code}"
fi
exit 0
CURL_EOF
chmod +x "${BIN_DIR}/curl"

# jq passthrough: use the real jq if present (needed for the existing-route
# PUT-patch branch); this harness's tests never hit that branch (higress_get
# always sees an empty/404 body unless CANNED_GET_PATH targets the route),
# so a real jq isn't required, but prefer it if available for realism.
REAL_JQ="$(command -v jq || true)"
if [ -z "${REAL_JQ}" ]; then
    cat > "${BIN_DIR}/jq" <<'JQ_EOF'
#!/bin/bash
echo "STUB jq INVOKED UNEXPECTEDLY: $*" >&2
exit 1
JQ_EOF
    chmod +x "${BIN_DIR}/jq"
fi

run_register_provider() {
    # run_register_provider <fake_home> <stale_get_count> -- <script args...>
    local home="$1"; shift
    local stale_count="$1"; shift
    if [ "${1:-}" = "--" ]; then shift; fi

    rm -f "${CALL_LOG}"
    touch "${CALL_LOG}"
    (
        export PATH="${BIN_DIR}:${PATH}"
        export HOME="${home}"
        export HIGRESS_COOKIE_FILE="${home}/cookie"
        touch "${HIGRESS_COOKIE_FILE}"
        export AGENTTEAMS_ADMIN_USER="admin"
        export AGENTTEAMS_ADMIN_PASSWORD="${TEST_ADMIN_PASSWORD:-admin-pass}"
        export STALE_COUNTER_FILE="${home}/stale-counter"
        echo "${stale_count}" > "${STALE_COUNTER_FILE}"
        bash "${RUN_SCRIPT}" "$@"
    ) > "${home}/stdout.log" 2> "${home}/stderr.log"
    echo $?
}

new_home() { mktemp -d "${TMPDIR_ROOT}/home-XXXXXX"; }

echo "=== Test 1: fresh name -> POST source+provider+route, 5c body shapes ==="
home1=$(new_home)
rc=$(run_register_provider "${home1}" 0 -- ollama --url https://ollama.com/v1 --key ollama-test-key)
log1=$(cat "${CALL_LOG}")
assert_eq "fresh name exits 0" "0" "${rc}"
assert_contains "service-source POSTed" "path=http://127.0.0.1:8001/v1/service-sources" "${log1}"
assert_contains "service-source body has domain ollama.com" '"domain":"ollama.com"' "${log1}"
assert_contains "service-source body is DNS type" '"type":"dns","name":"ollama"' "${log1}"
assert_contains "provider POSTed" "path=http://127.0.0.1:8001/v1/ai/providers" "${log1}"
assert_contains "provider body is openai type" '"type":"openai","name":"ollama"' "${log1}"
assert_contains "provider body carries the key" '"tokens":["ollama-test-key"]' "${log1}"
assert_contains "provider body carries openaiCustomUrl" '"openaiCustomUrl":"https://ollama.com/v1"' "${log1}"
assert_contains "route POSTed" "path=http://127.0.0.1:8001/v1/ai/routes" "${log1}"
assert_contains "route uses hiclaw-<name>-route naming" '"name":"hiclaw-ollama-route"' "${log1}"
assert_contains "route matches model prefix name/" '"modelPredicate":{"matchType":"PRE","matchValue":"ollama/"}' "${log1}"
assert_contains "route consumers start manager-only" '"allowedConsumers":["manager"]' "${log1}"

echo "=== Test 2: existing name -> PUTs (idempotent upsert) ==="
home2=$(new_home)
export CANNED_GET_PATH="http://127.0.0.1:8001/v1/service-sources/ollama"
export CANNED_GET_BODY='{"type":"dns","name":"ollama"}'
rc=$(run_register_provider "${home2}" 0 -- ollama --url https://ollama.com/v1 --key ollama-test-key)
log2=$(cat "${CALL_LOG}")
unset CANNED_GET_PATH CANNED_GET_BODY
assert_eq "existing name exits 0" "0" "${rc}"
assert_contains "existing service-source -> PUT not POST-only path" "method=PUT path=http://127.0.0.1:8001/v1/service-sources/ollama" "${log2}"

echo "=== Test 3a: name with a slash -> hard error before any call ==="
home3a=$(new_home)
rc=$(run_register_provider "${home3a}" 0 -- "bad/name" --url https://x.example.com/v1 --key some-key)
log3a=$(cat "${CALL_LOG}")
stderr3a=$(cat "${home3a}/stdout.log")
assert_eq "slash-in-name exits non-zero" "1" "${rc}"
assert_eq "slash-in-name makes zero calls" "" "${log3a}"
assert_contains "slash-in-name logs an error" "must not contain" "${stderr3a}"

echo "=== Test 3b: missing key -> hard error before any call ==="
home3b=$(new_home)
rc=$(run_register_provider "${home3b}" 0 -- mimo --url https://platform.xiaomimimo.com/v1)
log3b=$(cat "${CALL_LOG}")
stdout3b=$(cat "${home3b}/stdout.log")
assert_eq "missing key exits non-zero" "1" "${rc}"
assert_eq "missing key makes zero calls" "" "${log3b}"
assert_contains "missing key logs an error" "--key or --key-env is required" "${stdout3b}"

echo "=== Test 4: stale cookie -> exactly one re-login then retry succeeds ==="
home4=$(new_home)
rc=$(run_register_provider "${home4}" 1 -- ollama --url https://ollama.com/v1 --key ollama-test-key)
log4=$(cat "${CALL_LOG}")
relogin_count=$(printf '%s\n' "${log4}" | grep -c 'path=http://127.0.0.1:8001/session/login' || true)
assert_eq "stale cookie run still exits 0" "0" "${rc}"
assert_eq "exactly one re-login call" "1" "${relogin_count}"
assert_contains "after re-login, service-source call still lands" "path=http://127.0.0.1:8001/v1/service-sources" "${log4}"

echo "=== Test 5: --delete reverses all three (route, provider, service-source) ==="
home5=$(new_home)
rc=$(run_register_provider "${home5}" 0 -- ollama --delete)
log5=$(cat "${CALL_LOG}")
assert_eq "--delete exits 0" "0" "${rc}"
assert_contains "delete removes the route" "method=DELETE path=http://127.0.0.1:8001/v1/ai/routes/hiclaw-ollama-route" "${log5}"
assert_contains "delete removes the provider" "method=DELETE path=http://127.0.0.1:8001/v1/ai/providers/ollama" "${log5}"
assert_contains "delete removes the service-source" "method=DELETE path=http://127.0.0.1:8001/v1/service-sources/ollama" "${log5}"

echo "=== Test 5b: --delete against a stale cookie -> exactly one re-login, then succeeds ==="
home5b=$(new_home)
# STALE_COUNTER_FILE is shared across GET and DELETE in the shim; a credit of
# 1 makes only the FIRST call (the route DELETE) come back as the expired-
# session HTML page, forcing exactly one re-login before the retried DELETE
# (and the two subsequent deletes) succeed.
rc=$(run_register_provider "${home5b}" 1 -- ollama --delete)
log5b=$(cat "${CALL_LOG}")
stdout5b=$(cat "${home5b}/stdout.log")
relogin_count5b=$(printf '%s\n' "${log5b}" | grep -c 'path=http://127.0.0.1:8001/session/login' || true)
assert_eq "--delete with stale cookie still exits 0" "0" "${rc}"
assert_eq "--delete with stale cookie does exactly one re-login" "1" "${relogin_count5b}"
assert_contains "delete removes the route after re-login" "method=DELETE path=http://127.0.0.1:8001/v1/ai/routes/hiclaw-ollama-route" "${log5b}"
assert_contains "delete removes the provider after re-login" "method=DELETE path=http://127.0.0.1:8001/v1/ai/providers/ollama" "${log5b}"
assert_contains "delete removes the service-source after re-login" "method=DELETE path=http://127.0.0.1:8001/v1/service-sources/ollama" "${log5b}"
assert_contains "--delete with stale cookie reports final success" "Provider 'ollama' removed." "${stdout5b}"

echo "=== Test 5c: creds with a double-quote produce valid JSON in the re-login body ==="
home5c=$(new_home)
# Force exactly one HTML/stale response so _higress_relogin fires, then capture the
# login POST body from the call log and confirm it's valid, correctly-valued
# JSON even though the password (AGENTTEAMS_ADMIN_PASSWORD, sourced from an
# already-exported env var same as production) contains an embedded double
# quote.
export TEST_ADMIN_PASSWORD='p"a"ss'
rc=$(run_register_provider "${home5c}" 1 -- ollama --url https://ollama.com/v1 --key ollama-test-key)
unset TEST_ADMIN_PASSWORD
log5c=$(cat "${CALL_LOG}")
login_body=$(printf '%s\n' "${log5c}" | grep 'path=http://127.0.0.1:8001/session/login' -A1 | grep '^CURL body=' | sed 's/^CURL body=//' | head -n1)
assert_eq "creds-with-quote run still exits 0" "0" "${rc}"
parsed_ok="no"
if printf '%s' "${login_body}" | jq -e . >/dev/null 2>&1; then
    parsed_pw=$(printf '%s' "${login_body}" | jq -r '.password')
    [ "${parsed_pw}" = 'p"a"ss' ] && parsed_ok="yes"
fi
assert_eq "login body with embedded quote is valid JSON with the exact password" "yes" "${parsed_ok}"

echo "=== Test 6: call log never contains default-ai-route (any test's log) ==="
combined_log="${log1}
${log2}
${log4}
${log5}
${log5b}"
assert_not_contains "no test's call log references default-ai-route" "default-ai-route" "${combined_log}"

echo "=== Test 6b: grep-assert — script CODE never references default-ai-route ==="
code_only=$(grep -v '^[[:space:]]*#' "${TARGET_SCRIPT}")
assert_not_contains "register-provider.sh code lines never reference default-ai-route" "default-ai-route" "${code_only}"

echo "=== Test 7: the key never appears in stdout ==="
stdout1=$(cat "${home1}/stdout.log")
assert_not_contains "key not echoed to stdout on registration" "ollama-test-key" "${stdout1}"

echo ""
echo "=== Results: ${PASS} passed, ${FAIL} failed ==="
if [ "${FAIL}" -gt 0 ]; then
    exit 1
fi
exit 0
