#!/bin/bash
# Detect available AI coding CLI tools and their config/auth surfaces
# Output: JSON with available tools and details
# Usage: bash detect-available-cli.sh
#
# A CLI is "available" when its binary is on PATH AND at least one
# config/auth surface exists:
#   claude/gemini/qodercli : $HOME/.claude / $HOME/.gemini / $HOME/.qoder directory
#   qwen                   : $HOME/.qwen directory, OR env-based auth
#   opencode                : $HOME/.config/opencode directory,
#                             $HOME/.local/share/opencode/auth.json file
#                             (credentials from `opencode auth login`),
#                             OR env-based auth
#
# Env-based auth: only the presence of env var NAMES is checked, by listing
# current variable names with `printenv | cut -d= -f1` and comparing names.
# Values are never read into the output, echoed, or stored.
#
# Env var names checked (names only, never values):
#   qwen (verified against qwen 0.25.0):
#     DASHSCOPE_API_KEY        Qwen Cloud provider key; referenced by this
#                              repo's installer (install/agentteams-install.sh)
#     QWEN_OAUTH_DYNAMIC_TOKEN OAuth dynamic-token form; found in the qwen
#                              0.25.0 CLI bundle
#     QWEN_API_KEY             provider key for the qwen endpoint; found in
#                              the qwen 0.25.0 CLI bundle
#     OPENAI_API_KEY           provider key for `qwen --auth-type openai`
#     ANTHROPIC_API_KEY        provider key for `qwen --auth-type anthropic`
#     GEMINI_API_KEY           provider key for `qwen --auth-type gemini`
#     GOOGLE_API_KEY           alternate Gemini provider key
#   opencode (per opencode.ai/docs/providers):
#     OPENAI_API_KEY, ANTHROPIC_API_KEY, OPENROUTER_API_KEY,
#     GEMINI_API_KEY, XAI_API_KEY
#
# `details` records which surface(s) matched: config_source.dir,
# config_source.share_file (opencode auth.json), config_source.env (array of
# matched env var NAMES). It never contains any credential value.

result='{"available":[],"details":{}}'

# Names of the env vars currently set (names only; values are discarded by cut).
current_env_names="$(printenv | cut -d= -f1)"

# env_names_set <space-separated names> -> the subset that is currently set
env_names_set() {
    local wanted="$1" name found=""
    for name in $wanted; do
        if printf '%s\n' "$current_env_names" | grep -qx -- "$name"; then
            found="${found} ${name}"
        fi
    done
    printf '%s' "$found"
}

# to_json_array <space-separated names> -> JSON array literal of the names
to_json_array() {
    local name arr=""
    for name in $1; do
        arr="${arr}${arr:+, }\"$name\""
    done
    printf '[%s]' "$arr"
}

QWEN_ENV_NAMES="DASHSCOPE_API_KEY QWEN_OAUTH_DYNAMIC_TOKEN QWEN_API_KEY OPENAI_API_KEY ANTHROPIC_API_KEY GEMINI_API_KEY GOOGLE_API_KEY"
OPENCODE_ENV_NAMES="OPENAI_API_KEY ANTHROPIC_API_KEY OPENROUTER_API_KEY GEMINI_API_KEY XAI_API_KEY"

for cli in claude gemini qodercli qwen opencode; do
    binary_ok=false
    config_ok=false
    cfg_dir=false
    cfg_share_file=false
    env_found=""

    if command -v "$cli" &>/dev/null; then
        binary_ok=true
    fi

    case "$cli" in
        claude)   [ -d "$HOME/.claude" ]  && { config_ok=true; cfg_dir=true; } ;;
        gemini)   [ -d "$HOME/.gemini" ]  && { config_ok=true; cfg_dir=true; } ;;
        qodercli) [ -d "$HOME/.qoder" ]   && { config_ok=true; cfg_dir=true; } ;;
        qwen)
            [ -d "$HOME/.qwen" ] && { config_ok=true; cfg_dir=true; }
            env_found="$(env_names_set "$QWEN_ENV_NAMES")"
            [ -n "$env_found" ] && config_ok=true
            ;;
        opencode)
            [ -d "$HOME/.config/opencode" ] && { config_ok=true; cfg_dir=true; }
            [ -f "$HOME/.local/share/opencode/auth.json" ] && { config_ok=true; cfg_share_file=true; }
            env_found="$(env_names_set "$OPENCODE_ENV_NAMES")"
            [ -n "$env_found" ] && config_ok=true
            ;;
    esac

    if $binary_ok && $config_ok; then
        result=$(echo "$result" | jq --arg c "$cli" '.available += [$c]')
    fi

    # Uniform details schema for all runners: config_source reports which
    # surface(s) matched (dir / share_file / env), so `config: true` is
    # never left unexplained.
    env_array=$(to_json_array "$env_found")
    result=$(echo "$result" | jq \
        --arg c "$cli" \
        --argjson b "$binary_ok" \
        --argjson co "$config_ok" \
        --argjson dir "$cfg_dir" \
        --argjson sf "$cfg_share_file" \
        --argjson env "$env_array" \
        '.details[$c] = {binary: $b, config: $co, config_source: {dir: $dir, share_file: $sf, env: $env}}')
done

echo "$result"
