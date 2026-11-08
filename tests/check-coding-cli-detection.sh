#!/bin/bash
# check-coding-cli-detection.sh — regression tests for
# manager/agent/skills/coding-cli-management/scripts/detect-available-cli.sh
#
# Pure bash + coreutils + jq; no network and no real CLI binaries — each
# scenario runs detect-available-cli.sh against a sandbox whose HOME is a
# fresh mktemp dir and whose PATH is prefixed with stub `qwen`/`opencode`
# binaries (and scrubbed of any real CLIs). Safe to run in CI.
#
# Design
#   Every scenario gets a fresh mktemp sandbox:
#     $SANDBOX/bin   stub CLIs (PATH-prepended)
#     $SANDBOX/home  fake HOME: config dirs / auth files placed per scenario
#   detect-available-cli.sh is invoked under `env -i` with only HOME and a
#   scrubbed PATH, so results never depend on the runner's real CLIs or
#   credentials.
#
# Scenarios (sandbox setup -> expected outcome)
#   1. qwen_binary_and_home_dir   : stub qwen + $HOME/.qwen dir
#                                   -> available; config_source.dir
#   2. qwen_binary_env_auth       : stub qwen + no dirs + DASHSCOPE_API_KEY
#                                   (sentinel value)
#                                   -> available; config_source.env
#   3. opencode_binary_share_file : stub opencode +
#                                   $HOME/.local/share/opencode/auth.json
#                                   -> available; config_source.share_file
#   4. opencode_binary_config_dir : stub opencode + $HOME/.config/opencode
#                                   -> available; config_source.dir
#   5. binary_without_auth        : stub qwen, no auth surface
#                                   -> NOT available (config: false)
#   6. auth_without_binary        : $HOME/.qwen dir, no qwen on PATH
#                                   -> NOT available (binary: false)
#   7. sentinel_never_leaks       : stub qwen + opencode with sentinel env
#                                   values set -> output contains no sentinel
#   8. claude_binary_and_home_dir : stub claude + $HOME/.claude dir
#                                   -> available; config_source.dir
#   9. gemini_binary_and_home_dir : stub gemini + $HOME/.gemini dir
#                                   -> available; config_source.dir
#  10. qodercli_binary_and_home_dir: stub qodercli + $HOME/.qoder dir
#                                   -> available; config_source.dir
#
# Usage:  bash tests/check-coding-cli-detection.sh
# Exit:   0 if every scenario passes, 1 otherwise.

set -u

# Locate the repo root from this file's own path (tests/ -> ..); never
# depend on the caller's cwd.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${REPO_ROOT}" || exit 1

DETECT_SCRIPT="${REPO_ROOT}/manager/agent/skills/coding-cli-management/scripts/detect-available-cli.sh"
if [ ! -f "${DETECT_SCRIPT}" ]; then
    echo "FAIL setup (detect-available-cli.sh not found: ${DETECT_SCRIPT})"
    exit 1
fi

if ! command -v jq >/dev/null 2>&1; then
    echo "[SKIP] check-coding-cli-detection (jq not found)"
    exit 0
fi

SANDBOX=""
RUN_OUT=""
RUN_RC=0

cleanup() {
    if [ -n "${SANDBOX}" ] && [ -d "${SANDBOX}" ]; then
        rm -rf "${SANDBOX}"
    fi
}
trap cleanup EXIT

# write_stubs DIR CLI... -> stub binaries that answer `--version`
write_stubs() {
    local bin="$1"; shift
    local cli
    for cli in "$@"; do
        cat > "${bin}/${cli}" <<STUB_EOF
#!/bin/bash
# Stub \`${cli}\` for tests/check-coding-cli-detection.sh.
case "\${1:-}" in
  --version) echo "0.0.0-stub" ;;
  *) exit 0 ;;
esac
STUB_EOF
        chmod +x "${bin}/${cli}"
    done
}

new_sandbox() {
    SANDBOX="$(mktemp -d)"
    mkdir -p "${SANDBOX}/bin" "${SANDBOX}/home"
}

# run_detect [NAME=value]... : run the detect script hermetically.
# The PATH keeps only the sandbox bin plus the directories of the tools the
# detect script itself needs (jq/coreutils), so a real CLI on the runner
# can neither satisfy nor break a scenario.
run_detect() {
    local -a extra=("$@")
    local toolpath
    toolpath="$(dirname "$(command -v jq)")"
    RUN_OUT="$(env -i \
        HOME="${SANDBOX}/home" \
        PATH="${SANDBOX}/bin:${toolpath}:/usr/bin:/bin" \
        ${extra[@]+"${extra[@]}"} \
        bash "${DETECT_SCRIPT}" 2>&1)"
    RUN_RC=$?
}

SUITE_FAIL=0

# finish NAME PROBLEMS (PROBLEMS empty => PASS)
finish() {
    if [ -z "$2" ]; then
        echo "[PASS] $1"
    else
        echo "[FAIL] $1 ($2)"
        SUITE_FAIL=1
        printf '%s\n' "${RUN_OUT}" | sed 's/^/    | /'
    fi
}

json_get() {
    printf '%s' "${RUN_OUT}" | jq -r "$1" 2>/dev/null
}

# --- Scenario 1: qwen binary + $HOME/.qwen dir -> available via dir
new_sandbox
write_stubs "${SANDBOX}/bin" qwen
mkdir -p "${SANDBOX}/home/.qwen"
run_detect
problems=""
[ "${RUN_RC}" = "0" ] || problems="${problems}rc=${RUN_RC}; "
[ "$(json_get '.available | index("qwen")')" != "null" ] || problems="${problems}qwen not available; "
[ "$(json_get '.details.qwen.config_source.dir')" = "true" ] || problems="${problems}config_source.dir != true; "
[ "$(json_get '.details.qwen.config_source.env | length')" = "0" ] || problems="${problems}env should be empty; "
finish "qwen_binary_and_home_dir" "${problems}"

# --- Scenario 2: qwen binary, no dirs, env-based auth -> available via env
new_sandbox
write_stubs "${SANDBOX}/bin" qwen
run_detect DASHSCOPE_API_KEY="SENTINEL-QWEN-KEY-DO-NOT-LEAK-0001"
problems=""
[ "${RUN_RC}" = "0" ] || problems="${problems}rc=${RUN_RC}; "
[ "$(json_get '.available | index("qwen")')" != "null" ] || problems="${problems}qwen not available; "
[ "$(json_get '.details.qwen.config_source.dir')" = "false" ] || problems="${problems}config_source.dir should be false; "
[ "$(json_get '.details.qwen.config_source.env | index("DASHSCOPE_API_KEY")')" != "null" ] \
    || problems="${problems}env should list DASHSCOPE_API_KEY; "
case "${RUN_OUT}" in
    *SENTINEL-QWEN-KEY-DO-NOT-LEAK-0001*) problems="${problems}sentinel value leaked; " ;;
esac
finish "qwen_binary_env_auth" "${problems}"

# --- Scenario 3: opencode binary + auth.json -> available via share_file
new_sandbox
write_stubs "${SANDBOX}/bin" opencode
mkdir -p "${SANDBOX}/home/.local/share/opencode"
echo '{"stub": true}' > "${SANDBOX}/home/.local/share/opencode/auth.json"
run_detect
problems=""
[ "${RUN_RC}" = "0" ] || problems="${problems}rc=${RUN_RC}; "
[ "$(json_get '.available | index("opencode")')" != "null" ] || problems="${problems}opencode not available; "
[ "$(json_get '.details.opencode.config_source.share_file')" = "true" ] || problems="${problems}config_source.share_file != true; "
[ "$(json_get '.details.opencode.config_source.dir')" = "false" ] || problems="${problems}config_source.dir should be false; "
finish "opencode_binary_share_file" "${problems}"

# --- Scenario 4: opencode binary + ~/.config/opencode dir -> available via dir
new_sandbox
write_stubs "${SANDBOX}/bin" opencode
mkdir -p "${SANDBOX}/home/.config/opencode"
run_detect
problems=""
[ "${RUN_RC}" = "0" ] || problems="${problems}rc=${RUN_RC}; "
[ "$(json_get '.available | index("opencode")')" != "null" ] || problems="${problems}opencode not available; "
[ "$(json_get '.details.opencode.config_source.dir')" = "true" ] || problems="${problems}config_source.dir != true; "
[ "$(json_get '.details.opencode.config_source.share_file')" = "false" ] || problems="${problems}config_source.share_file should be false; "
finish "opencode_binary_config_dir" "${problems}"

# --- Scenario 5: binary present, no auth surface -> NOT available
new_sandbox
write_stubs "${SANDBOX}/bin" qwen
run_detect
problems=""
[ "${RUN_RC}" = "0" ] || problems="${problems}rc=${RUN_RC}; "
[ "$(json_get '.available | index("qwen")')" = "null" ] || problems="${problems}qwen should NOT be available; "
[ "$(json_get '.details.qwen.binary')" = "true" ] || problems="${problems}details.qwen.binary should be true; "
[ "$(json_get '.details.qwen.config')" = "false" ] || problems="${problems}details.qwen.config should be false; "
finish "binary_without_auth" "${problems}"

# --- Scenario 6: auth dir present, no binary -> NOT available
new_sandbox
mkdir -p "${SANDBOX}/home/.qwen"
run_detect
problems=""
[ "${RUN_RC}" = "0" ] || problems="${problems}rc=${RUN_RC}; "
[ "$(json_get '.available | index("qwen")')" = "null" ] || problems="${problems}qwen should NOT be available; "
[ "$(json_get '.details.qwen.binary')" = "false" ] || problems="${problems}details.qwen.binary should be false; "
[ "$(json_get '.details.qwen.config')" = "true" ] || problems="${problems}details.qwen.config should be true; "
finish "auth_without_binary" "${problems}"

# --- Scenario 7: sentinel env values must never appear in the output
new_sandbox
write_stubs "${SANDBOX}/bin" qwen opencode
mkdir -p "${SANDBOX}/home/.qwen" "${SANDBOX}/home/.config/opencode"
run_detect \
    DASHSCOPE_API_KEY="SENTINEL-QWEN-KEY-DO-NOT-LEAK-0001" \
    OPENAI_API_KEY="SENTINEL-OPENAI-KEY-DO-NOT-LEAK-0002" \
    ANTHROPIC_API_KEY="SENTINEL-ANTHROPIC-KEY-DO-NOT-LEAK-0003" \
    OPENROUTER_API_KEY="SENTINEL-OPENROUTER-KEY-DO-NOT-LEAK-0004" \
    QWEN_OAUTH_DYNAMIC_TOKEN="SENTINEL-QWEN-OAUTH-TOKEN-DO-NOT-LEAK-0005"
problems=""
[ "${RUN_RC}" = "0" ] || problems="${problems}rc=${RUN_RC}; "
[ "$(json_get '.available | index("qwen")')" != "null" ] || problems="${problems}qwen not available; "
[ "$(json_get '.available | index("opencode")')" != "null" ] || problems="${problems}opencode not available; "
sent=0
for s in SENTINEL-QWEN-KEY-DO-NOT-LEAK-0001 SENTINEL-OPENAI-KEY-DO-NOT-LEAK-0002 \
         SENTINEL-ANTHROPIC-KEY-DO-NOT-LEAK-0003 SENTINEL-OPENROUTER-KEY-DO-NOT-LEAK-0004 \
         SENTINEL-QWEN-OAUTH-TOKEN-DO-NOT-LEAK-0005; do
    case "${RUN_OUT}" in
        *"${s}"*) problems="${problems}sentinel value leaked; " sent=1 ;;
    esac
done
[ "${sent}" = "0" ] || problems="${problems}credential value appeared in output; "
# names (not values) are allowed and expected in config_source.env
[ "$(json_get '.details.qwen.config_source.env | index("DASHSCOPE_API_KEY")')" != "null" ] \
    || problems="${problems}env should list DASHSCOPE_API_KEY name; "
finish "sentinel_never_leaks" "${problems}"

# --- Scenario 8: claude binary + $HOME/.claude dir -> available via dir
new_sandbox
write_stubs "${SANDBOX}/bin" claude
mkdir -p "${SANDBOX}/home/.claude"
run_detect
problems=""
[ "${RUN_RC}" = "0" ] || problems="${problems}rc=${RUN_RC}; "
[ "$(json_get '.available | index("claude")')" != "null" ] || problems="${problems}claude not available; "
[ "$(json_get '.details.claude.config_source.dir')" = "true" ] || problems="${problems}config_source.dir != true; "
[ "$(json_get '.details.claude.config_source.share_file')" = "false" ] || problems="${problems}config_source.share_file should be false; "
finish "claude_binary_and_home_dir" "${problems}"

# --- Scenario 9: gemini binary + $HOME/.gemini dir -> available via dir
new_sandbox
write_stubs "${SANDBOX}/bin" gemini
mkdir -p "${SANDBOX}/home/.gemini"
run_detect
problems=""
[ "${RUN_RC}" = "0" ] || problems="${problems}rc=${RUN_RC}; "
[ "$(json_get '.available | index("gemini")')" != "null" ] || problems="${problems}gemini not available; "
[ "$(json_get '.details.gemini.config_source.dir')" = "true" ] || problems="${problems}config_source.dir != true; "
[ "$(json_get '.details.gemini.config_source.share_file')" = "false" ] || problems="${problems}config_source.share_file should be false; "
finish "gemini_binary_and_home_dir" "${problems}"

# --- Scenario 10: qodercli binary + $HOME/.qoder dir -> available via dir
new_sandbox
write_stubs "${SANDBOX}/bin" qodercli
mkdir -p "${SANDBOX}/home/.qoder"
run_detect
problems=""
[ "${RUN_RC}" = "0" ] || problems="${problems}rc=${RUN_RC}; "
[ "$(json_get '.available | index("qodercli")')" != "null" ] || problems="${problems}qodercli not available; "
[ "$(json_get '.details.qodercli.config_source.dir')" = "true" ] || problems="${problems}config_source.dir != true; "
[ "$(json_get '.details.qodercli.config_source.share_file')" = "false" ] || problems="${problems}config_source.share_file should be false; "
finish "qodercli_binary_and_home_dir" "${problems}"

# --- Skill placement & reference path chain
# The manager image mounts manager/agent/ at /opt/agentteams/agent/ and the
# runtime loads skills only from the official layers (skills/, worker-skills/):
# skills-alpha/ is a dead zone at runtime (no upgrade-builtins.sh sync, no
# push-worker-skills.sh source entry, no start-manager-agent.sh render).
# This section pins placement and the script-reference chain.
PATH_PROBLEMS=""
MGMT_SKILL="manager/agent/skills/coding-cli-management/SKILL.md"
WORKER_SKILL="manager/agent/worker-skills/coding-cli/SKILL.md"
[ -f "${MGMT_SKILL}" ] || PATH_PROBLEMS="${PATH_PROBLEMS}management SKILL.md missing from official skills/ layer; "
[ -f "${WORKER_SKILL}" ] || PATH_PROBLEMS="${PATH_PROBLEMS}worker SKILL.md missing from official worker-skills/ layer; "
[ -d "manager/agent/skills-alpha/coding-cli-management" ] && PATH_PROBLEMS="${PATH_PROBLEMS}management SKILL still present in skills-alpha dead zone; "
if [ -f "${MGMT_SKILL}" ]; then
    while IFS= read -r ref; do
        rel="${ref#/opt/agentteams/agent/}"
        [ -f "manager/agent/${rel}" ] || PATH_PROBLEMS="${PATH_PROBLEMS}unresolved script ref ${rel}; "
    done < <(grep -oE '/opt/agentteams/agent/[A-Za-z0-9._/-]+\.sh' "${MGMT_SKILL}" | sort -u)
fi
if [ -z "${PATH_PROBLEMS}" ]; then
    echo "[PASS] skill_placement_and_path_chain"
else
    echo "[FAIL] skill_placement_and_path_chain (${PATH_PROBLEMS})"
    SUITE_FAIL=1
fi

echo ""
if [ "${SUITE_FAIL}" = "0" ]; then
    echo "All scenarios + path-chain check passed."
    exit 0
else
    echo "One or more scenarios FAILED."
    exit 1
fi
