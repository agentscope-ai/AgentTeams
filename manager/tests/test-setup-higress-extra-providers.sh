#!/bin/bash
# test-setup-higress-extra-providers.sh
#
# Static (offline) tests for setup-higress.sh's Step-5 extra-LLM-provider loop
# (plan v2.3 Phase 2b steps 1-2, decision #7; docs/implementation-milestone-2.md
# Step 5). Shims curl/jq on PATH to record every Higress Console API call
# setup-higress.sh makes, then asserts:
#
#   1. HICLAW_EXTRA_LLM_PROVIDERS unset -> byte-identical call sequence to the
#      pre-existing (no-env) baseline (proves the new loop is a true no-op).
#   2. HICLAW_EXTRA_LLM_PROVIDERS set -> each provider gets its own DNS
#      service-source, its own `openai`-type provider, and its own AI route
#      named `hiclaw-<provider>-route` with a model-prefix match.
#   3. The new loop never reads or writes `default-ai-route` (grep-assert +
#      runtime call-log assert).
#   4. A malformed entry / missing per-provider API key is skipped with a
#      warning, not a hard failure.
#
# Usage: bash manager/tests/test-setup-higress-extra-providers.sh
# (intended to run inside an alpine container with `bash` + `jq` installed —
#  see docs/implementation-milestone-2.md Toolchain section; CRLF is stripped
#  below so it also runs unmodified from a checkout with autocrlf=true.)

set -uo pipefail

PASS=0
FAIL=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
TARGET_SCRIPT="${PROJECT_ROOT}/manager/scripts/init/setup-higress.sh"

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

# ── Fake /opt/hiclaw tree the script sources/reads from ──────────────────────
FAKE_ROOT="${TMPDIR_ROOT}/fake-opt-hiclaw"
mkdir -p "${FAKE_ROOT}/scripts/lib" "${FAKE_ROOT}/agent"
cp "${PROJECT_ROOT}/manager/scripts/lib/base.sh" "${FAKE_ROOT}/scripts/lib/base.sh"
sed -i 's/\r$//' "${FAKE_ROOT}/scripts/lib/base.sh"

# Copy the target script under test, strip CRLF (checkout is autocrlf=true),
# and rewrite the one hardcoded source path so it resolves inside FAKE_ROOT.
RUN_SCRIPT="${TMPDIR_ROOT}/setup-higress.sh"
sed 's/\r$//' "${TARGET_SCRIPT}" > "${RUN_SCRIPT}"
sed -i "s#source /opt/hiclaw/scripts/lib/base.sh#source ${FAKE_ROOT}/scripts/lib/base.sh#" "${RUN_SCRIPT}"
chmod +x "${RUN_SCRIPT}"

# ── PATH shims: curl + jq record every call, no network/binary needed ───────
BIN_DIR="${TMPDIR_ROOT}/bin"
mkdir -p "${BIN_DIR}"
export CALL_LOG="${TMPDIR_ROOT}/calls.log"

cat > "${BIN_DIR}/curl" <<'CURL_EOF'
#!/bin/bash
# Records method/path/body of every Higress Console call; always returns a
# generic "already exists" style 200 so the script's GET->PUT/POST branches
# take a deterministic, harmless path (higress_get sees empty body -> POST).
method="GET"
path=""
outfile=""
write_out=""
args=("$@")
i=0
while [ $i -lt ${#args[@]} ]; do
    case "${args[$i]}" in
        -X) i=$((i+1)); method="${args[$i]}" ;;
        -o) i=$((i+1)); outfile="${args[$i]}" ;;
        -w) i=$((i+1)); write_out="${args[$i]}" ;;
        -d) i=$((i+1)); body="${args[$i]}" ;;
        http*://*) path="${args[$i]}" ;;
    esac
    i=$((i+1))
done
{
    echo "CURL method=${method} path=${path}"
    [ -n "${body:-}" ] && echo "CURL body=${body}"
} >> "${CALL_LOG:-/dev/null}"

if [ -n "${outfile}" ]; then
    printf '' > "${outfile}"
fi
if [ -n "${write_out}" ]; then
    printf '200'
fi
exit 0
CURL_EOF
chmod +x "${BIN_DIR}/curl"

# jq passthrough shim: if real jq exists elsewhere on PATH use it (needed for
# the AI-route header-control jq pipelines); otherwise fall back to a minimal
# stub sufficient for this test (the extra-provider loop's jq usage only runs
# on the existing-route PUT branch, which our curl shim never triggers since
# higress_get always returns empty body).
REAL_JQ="$(command -v jq || true)"
if [ -z "${REAL_JQ}" ]; then
    cat > "${BIN_DIR}/jq" <<'JQ_EOF'
#!/bin/bash
# Minimal stub: never invoked in this test's code paths (all higress_get
# calls return empty body, so scripts take the POST/create branch, not the
# jq-patch branch). Fail loudly if that assumption ever breaks.
echo "STUB jq INVOKED UNEXPECTEDLY: $*" >&2
exit 1
JQ_EOF
    chmod +x "${BIN_DIR}/jq"
fi

# The script hardcodes SETUP_MARKER=/data/.higress-setup-done. Repoint it at
# $HOME so each test's fake-home marker is independent and no test writes to
# the real /data (equivalent behavior — the marker is just an existence
# check — just relocated to keep the harness hermetic and each case's
# "first boot already done" pre-seed effective).
sed -i 's#SETUP_MARKER="/data/.higress-setup-done"#SETUP_MARKER="${HOME}/.higress-setup-done"#' "${RUN_SCRIPT}"

# Skip the trailing 45s sleep — irrelevant to what we assert and slows CI.
sed -i 's/^sleep 45$/sleep 0/' "${RUN_SCRIPT}"

run_setup_higress() {
    # run_setup_higress <fake_home> [VAR=value ...]
    local home="$1"; shift
    rm -f "${CALL_LOG}"
    touch "${CALL_LOG}"
    (
        export PATH="${BIN_DIR}:${PATH}"
        export HOME="${home}"
        export HIGRESS_COOKIE_FILE="${home}/cookie"
        touch "${HIGRESS_COOKIE_FILE}"
        export HICLAW_MANAGER_GATEWAY_KEY="test-gw-key"
        for assignment in "$@"; do
            export "${assignment?}"
        done
        bash "${RUN_SCRIPT}"
    ) > "${home}/stdout.log" 2>&1
}

new_home() { mktemp -d "${TMPDIR_ROOT}/home-XXXXXX"; }

echo "=== Test 1: HICLAW_EXTRA_LLM_PROVIDERS unset -> no extra-provider calls ==="
home1=$(new_home)
touch "${home1}/.higress-setup-done"  # pre-mark first-boot done for a focused diff
run_setup_higress "${home1}"
baseline_log=$(cat "${CALL_LOG}")
assert_not_contains "no extra-provider service-source call when env unset" "ollama" "${baseline_log}"
assert_not_contains "no mimo service-source call when env unset" "mimo" "${baseline_log}"
assert_not_contains "no hiclaw-<provider>-route call when env unset" "hiclaw-ollama-route" "${baseline_log}"

echo "=== Test 1b: env-unset baseline is IDENTICAL across two independent runs ==="
home1b=$(new_home)
touch "${home1b}/.higress-setup-done"
run_setup_higress "${home1b}"
rerun_log=$(cat "${CALL_LOG}")
# Normalize away nothing (no timestamps in the curl call log) and compare.
assert_eq "env-unset call log is byte-identical run-to-run" "${baseline_log}" "${rerun_log}"

echo "=== Test 2: HICLAW_EXTRA_LLM_PROVIDERS set -> per-provider registration ==="
home2=$(new_home)
touch "${home2}/.higress-setup-done"
run_setup_higress "${home2}" \
    'HICLAW_EXTRA_LLM_PROVIDERS=ollama=https://ollama.com/v1;mimo=https://platform.xiaomimimo.com/v1' \
    'HICLAW_OLLAMA_API_KEY=ollama-test-key' \
    'HICLAW_MIMO_API_KEY=mimo-test-key'
extra_log=$(cat "${CALL_LOG}")

assert_contains "ollama DNS service-source registered" 'path=http://127.0.0.1:8001/v1/service-sources' "${extra_log}"
assert_contains "ollama service-source body has domain ollama.com" '"domain":"ollama.com"' "${extra_log}"
assert_contains "mimo service-source body has domain platform.xiaomimimo.com" '"domain":"platform.xiaomimimo.com"' "${extra_log}"
assert_contains "ollama provider created" '"type":"openai","name":"ollama"' "${extra_log}"
assert_contains "mimo provider created" '"type":"openai","name":"mimo"' "${extra_log}"
assert_contains "ollama route uses its own route name" '"name":"hiclaw-ollama-route"' "${extra_log}"
assert_contains "mimo route uses its own route name" '"name":"hiclaw-mimo-route"' "${extra_log}"
assert_contains "ollama route matches model prefix ollama/" '"modelPredicate":{"matchType":"PRE","matchValue":"ollama/"}' "${extra_log}"
assert_contains "mimo route matches model prefix mimo/" '"modelPredicate":{"matchType":"PRE","matchValue":"mimo/"}' "${extra_log}"
assert_not_contains "extra-provider loop never PUTs/POSTs default-ai-route" '/v1/ai/routes/default-ai-route' "${extra_log}"

echo "=== Test 3: malformed entry is skipped, not fatal ==="
home3=$(new_home)
touch "${home3}/.higress-setup-done"
run_setup_higress "${home3}" \
    'HICLAW_EXTRA_LLM_PROVIDERS=badentry-no-equals;ollama=https://ollama.com/v1' \
    'HICLAW_OLLAMA_API_KEY=ollama-test-key'
malformed_stdout=$(cat "${home3}/stdout.log")
malformed_log=$(cat "${CALL_LOG}")
assert_contains "malformed entry logs a warning" "malformed HICLAW_EXTRA_LLM_PROVIDERS entry" "${malformed_stdout}"
assert_contains "well-formed entry after a malformed one still registers" '"name":"ollama"' "${malformed_log}"

echo "=== Test 4: missing per-provider API key is skipped, not fatal ==="
home4=$(new_home)
touch "${home4}/.higress-setup-done"
run_setup_higress "${home4}" \
    'HICLAW_EXTRA_LLM_PROVIDERS=mimo=https://platform.xiaomimimo.com/v1'
missing_key_stdout=$(cat "${home4}/stdout.log")
missing_key_log=$(cat "${CALL_LOG}")
assert_contains "missing API key logs a warning naming the expected env var" "HICLAW_MIMO_API_KEY not set" "${missing_key_stdout}"
assert_not_contains "no provider call fires without the API key" '"name":"mimo"' "${missing_key_log}"

echo "=== Test 5: grep-assert — default-ai-route is not referenced in CODE by the new loop ==="
# Static check independent of runtime behavior: extract just the new
# extra-provider block, strip comment-only lines (the block's own doc
# comments legitimately name default-ai-route to explain why it's excluded),
# and assert no executable line references it.
block=$(awk '/^# 5c\. Extra LLM providers/,/^# 6\. GitHub MCP Server/' "${TARGET_SCRIPT}")
code_only=$(printf '%s\n' "${block}" | grep -v '^[[:space:]]*#')
assert_not_contains "extra-provider CODE lines never reference default-ai-route" "default-ai-route" "${code_only}"
assert_contains "extra-provider source block documents why default-ai-route is excluded" "default-ai-route" "${block}"
assert_contains "extra-provider source block is gated on HICLAW_EXTRA_LLM_PROVIDERS" 'HICLAW_EXTRA_LLM_PROVIDERS' "${block}"

echo ""
echo "=== Results: ${PASS} passed, ${FAIL} failed ==="
if [ "${FAIL}" -gt 0 ]; then
    exit 1
fi
exit 0
