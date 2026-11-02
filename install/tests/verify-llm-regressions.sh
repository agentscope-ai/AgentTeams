#!/bin/bash
# verify-llm-regressions.sh — executable regression suite for the LLM
# channel smoke (check #8) in install/agentteams-verify.sh.
#
# Pure bash + coreutils; no network and no real docker/curl — each scenario
# runs agentteams-verify.sh against a sandbox whose PATH is prefixed with
# stub `docker` and stub `curl`. Safe to run in CI.
#
# Design
#   Every scenario gets a fresh mktemp sandbox:
#     $TMP/bin  stub `docker` and stub `curl` (PATH-prepended)
#     $TMP/env  canned env files: the container env served by
#               `docker exec <c> printenv`, plus the dashboard env file
#     $TMP/rec  where stub `curl` appends every /v1/chat/completions request line
#
#   Baseline: with the baseline env files, checks #1-#7 of
#   agentteams-verify.sh all PASS — #1 via stub `docker ps` listing
#   agentteams-manager, #2/#3/#6 via stub `docker exec <c> curl` answering
#   200 (MinIO / Matrix / QwenPaw health), #4/#5 via stub host `curl`
#   answering 200, and #7 via a stubbed dashboard (enabled env file,
#   `docker ps` listing agentteams-dashboard, `docker port` mapping,
#   200 probe) — so the suite's exit code is driven entirely by check #8.
#
# Scenarios (stub curl behaviour $LLM_SCENARIO -> expected outcome)
#   1. reject_max_tokens (core regression): 400 when the request line
#      carries max_tokens, 200 otherwise.
#      Expect: exit 0, [PASS] LLM channel smoke, and the recorded request
#      contains NO max_tokens — the controller probe contract (model +
#      messages only, one-word prompt; see llmAuthProbePromptTemplate in
#      agentteams-controller/internal/service/provisioner.go).
#   2. ok:          200        -> exit 0, [PASS] LLM channel smoke
#   3. fail500:     500        -> exit 1, [FAIL] LLM channel smoke
#   4. missing_key: container env lacks AGENTTEAMS_MANAGER_GATEWAY_KEY
#                   -> exit 0, [SKIP] LLM channel smoke
#   5. skip_flag:   AGENTTEAMS_VERIFY_SKIP_LLM=1
#                   -> exit 0, [SKIP] LLM channel smoke
#   6. url_override: AGENTTEAMS_VERIFY_LLM_URL=http://127.0.0.1:59999/v1/chat/completions
#                   -> exit 0, and the recorded URL is exactly the override
#
# Usage:  bash install/tests/verify-llm-regressions.sh
# Exit:   0 if every scenario passes, 1 otherwise.

set -u

# Locate the repo root from this file's own path (install/tests/ -> ../..);
# never depend on the caller's cwd.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
cd "${REPO_ROOT}" || exit 1

VERIFY_SCRIPT="${REPO_ROOT}/install/agentteams-verify.sh"
if [ ! -f "${VERIFY_SCRIPT}" ]; then
    echo "FAIL setup (install/agentteams-verify.sh not found: ${VERIFY_SCRIPT})"
    exit 1
fi

CLEANUP_DIRS=""

cleanup() {
    # Remove only the mktemp sandboxes created by this run.
    for d in ${CLEANUP_DIRS}; do
        if [ -n "${d}" ] && [ -d "${d}" ]; then
            rm -rf "${d}"
        fi
    done
}
trap cleanup EXIT

write_stubs() {
    local bin="$1"

    cat > "${bin}/docker" <<'DOCKER_STUB'
#!/bin/bash
# Stub `docker` for install/tests/verify-llm-regressions.sh.
# Serves the baseline that lets checks #1-#7 of agentteams-verify.sh PASS:
#   docker version                     -> exit 0 (take the docker branch)
#   docker ps --format '{{.Names}}'    -> lists manager + dashboard
#   docker exec <c> printenv           -> cat $STUB_ENV_FILE
#   docker exec <c> curl ... <url>     -> 200 (internal probes #2/#3/#6)
#   docker exec <c> openclaw ...       -> {"ok":true} (runtime fallback)
#   docker port <c> 3000/tcp           -> 0.0.0.0:13000 (check #7)
#   anything else                      -> exit 0, silent
case "${1:-}" in
  version)
    exit 0
    ;;
  ps)
    echo "agentteams-dashboard"
    echo "agentteams-manager"
    exit 0
    ;;
  port)
    echo "0.0.0.0:13000"
    exit 0
    ;;
  exec)
    case "${3:-}" in
      printenv)
        cat "${STUB_ENV_FILE}" 2>/dev/null || true
        exit 0
        ;;
      curl)
        echo "200"
        exit 0
        ;;
      openclaw)
        echo '{"ok":true}'
        exit 0
        ;;
      *)
        exit 0
        ;;
    esac
    ;;
  *)
    exit 0
    ;;
esac
DOCKER_STUB

    cat > "${bin}/curl" <<'CURL_STUB'
#!/bin/bash
# Stub `curl` for install/tests/verify-llm-regressions.sh.
# Ignores all options; inspects the URL argument only. For
# */v1/chat/completions it appends the full request line (URL + headers +
# payload) to $STUB_REC_DIR/llm-requests.log, then answers per $LLM_SCENARIO:
#   reject_max_tokens -> 400 if the request line contains max_tokens, else 200
#   fail500           -> 500
#   ok / unset        -> 200
# Any other URL (host-side probes #4/#5, dashboard probe #7) -> 200.
url=""
for arg in "$@"; do
    case "${arg}" in
        http://*|https://*) url="${arg}" ;;
    esac
done
case "${url}" in
  */v1/chat/completions)
    if [ -n "${STUB_REC_DIR:-}" ]; then
        printf '%s\n' "$*" >> "${STUB_REC_DIR}/llm-requests.log"
    fi
    case "${LLM_SCENARIO:-ok}" in
      reject_max_tokens)
        case "${*}" in
          *max_tokens*) echo "400" ;;
          *)            echo "200" ;;
        esac
        ;;
      fail500)
        echo "500"
        ;;
      *)
        echo "200"
        ;;
    esac
    ;;
  *)
    echo "200"
    ;;
esac
exit 0
CURL_STUB

    chmod +x "${bin}/docker" "${bin}/curl"
}

# Write the canned container env file. $1 = path, $2 = key|nokey.
write_env_file() {
    {
        echo "AGENTTEAMS_PORT_GATEWAY=18080"
        echo "AGENTTEAMS_PORT_CONSOLE=18001"
        echo "AGENTTEAMS_MANAGER_RUNTIME=qwenpaw"
        if [ "$2" = "key" ]; then
            echo "AGENTTEAMS_MANAGER_GATEWAY_KEY=stub-gateway-key"
        fi
        echo "AGENTTEAMS_DEFAULT_MODEL=qwen3.6-plus"
    } > "$1"
}

new_sandbox() {
    SANDBOX="$(mktemp -d)"
    CLEANUP_DIRS="${CLEANUP_DIRS} ${SANDBOX}"
    mkdir -p "${SANDBOX}/bin" "${SANDBOX}/env" "${SANDBOX}/rec"
    write_stubs "${SANDBOX}/bin"
    # Check #7 reads this host-side dashboard env file; enable the
    # dashboard here so the check exercises its PASS path (stub `docker`
    # lists the dashboard container, maps its port, stub `curl` answers 200).
    {
        echo "AGENTTEAMS_DASHBOARD=1"
        echo "AGENTTEAMS_PORT_DASHBOARD=13000"
    } > "${SANDBOX}/env/dashboard.env"
    SANDBOX_DASHENV="${SANDBOX}/env/dashboard.env"
}

SANDBOX=""
RUN_RC=0
RUN_OUT=""

# run_verify LLM_SCENARIO SKIP_FLAG LLM_URL
run_verify() {
    RUN_OUT="$(
        PATH="${SANDBOX}/bin:${PATH}" \
        STUB_ENV_FILE="${SANDBOX}/env/container.env" \
        STUB_REC_DIR="${SANDBOX}/rec" \
        LLM_SCENARIO="${1}" \
        AGENTTEAMS_VERIFY_SKIP_LLM="${2}" \
        AGENTTEAMS_VERIFY_LLM_URL="${3}" \
        AGENTTEAMS_ENV_FILE="${SANDBOX_DASHENV}" \
        bash "${VERIFY_SCRIPT}" agentteams-manager 2>&1
    )"
    RUN_RC=$?
}

SUITE_FAIL=0

# finish NAME PROBLEMS (PROBLEMS empty => PASS)
finish() {
    if [ -z "$2" ]; then
        echo "PASS $1"
    else
        echo "FAIL $1 ($2)"
        SUITE_FAIL=1
        printf '%s\n' "${RUN_OUT}" | sed 's/^/    | /'
    fi
}

# scenario NAME WANT_RC WANT_TAG ENV_VARIANT LLM_SCENARIO SKIP_FLAG LLM_URL [LOG_MODE LOG_ARG]
scenario() {
    local name="$1" want_rc="$2" want_tag="$3" env_variant="$4"
    local llm_scenario="$5" skip_flag="$6" llm_url="$7"
    local log_mode="${8:-none}" log_arg="${9:-}"

    new_sandbox
    write_env_file "${SANDBOX}/env/container.env" "${env_variant}"
    run_verify "${llm_scenario}" "${skip_flag}" "${llm_url}"

    local log="${SANDBOX}/rec/llm-requests.log"
    local problems=""
    [ "${RUN_RC}" = "${want_rc}" ] || problems="${problems}rc=${RUN_RC} (want ${want_rc}); "
    printf '%s\n' "${RUN_OUT}" | grep -q "\[${want_tag}\] LLM channel smoke" \
        || problems="${problems}missing [${want_tag}] LLM channel smoke; "
    case "${log_mode}" in
      no_max_tokens)
        [ -f "${log}" ] || problems="${problems}no recorded LLM request; "
        if [ -f "${log}" ] && grep -q "max_tokens" "${log}"; then
            problems="${problems}recorded request still contains max_tokens; "
        fi
        ;;
      url_is)
        [ -f "${log}" ] || problems="${problems}no recorded LLM request; "
        if [ -f "${log}" ] && ! grep -qF "${log_arg}" "${log}"; then
            problems="${problems}recorded URL is not ${log_arg}; "
        fi
        ;;
    esac
    finish "${name}" "${problems}"
}

# 1. Core regression: a provider that rejects max_tokens with a 400 must
#    still see the probe PASS, and the recorded request must not carry it.
scenario reject_max_tokens 0 PASS key reject_max_tokens 0 "" no_max_tokens
# 2. Healthy provider.
scenario ok 0 PASS key ok 0 ""
# 3. Broken provider.
scenario fail500 1 FAIL key fail500 0 ""
# 4. No gateway key in the container env.
scenario missing_key 0 SKIP nokey ok 0 ""
# 5. Explicit skip flag.
scenario skip_flag 0 SKIP key ok 1 ""
# 6. URL override is honoured verbatim.
scenario url_override 0 PASS key ok 0 "http://127.0.0.1:59999/v1/chat/completions" url_is "http://127.0.0.1:59999/v1/chat/completions"

echo ""
if [ "${SUITE_FAIL}" = "0" ]; then
    echo "All 6 scenarios passed."
    exit 0
else
    echo "One or more scenarios FAILED."
    exit 1
fi
