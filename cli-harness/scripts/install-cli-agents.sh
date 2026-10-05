#!/bin/bash
# install-cli-agents.sh - install headless CLI coding agents into the image.
#
# Each CLI is installed best-effort: a failing channel must not break the
# image build for the other runtimes. The cli-harness worker fails fast at
# container start when the selected runtime's binary is missing, so a broken
# upstream channel surfaces as a clear worker error instead of a red build.
#
# Escape hatches at runtime:
#   AGENTTEAMS_CLI_EXTRA_ARGS - extra argv appended to every CLI invocation
#   AGENTTEAMS_CLI_ADAPTER    - adapter override (see entrypoint)
set -u

NPM_REGISTRY="${NPM_REGISTRY:-https://registry.npmjs.org/}"
PIP_INDEX_URL="${PIP_INDEX_URL:-https://pypi.org/simple/}"

log() { echo "[install-cli-agents] $1"; }

install_npm() {
    local pkg="$1"
    if npm install -g --registry "${NPM_REGISTRY}" "${pkg}"; then
        log "npm: ${pkg} installed"
    else
        log "WARN: npm install failed for ${pkg} (skipped)"
    fi
}

install_pip() {
    local pkg="$1"
    if pip install --no-cache-dir --index-url "${PIP_INDEX_URL}" "${pkg}"; then
        log "pip: ${pkg} installed"
    else
        log "WARN: pip install failed for ${pkg} (skipped)"
    fi
}

# claude-code  -> claude   (Anthropic)
install_npm "@anthropic-ai/claude-code"

# codex        -> codex    (OpenAI)
install_npm "@openai/codex"

# kimi-code    -> kimi     (Moonshot)
install_npm "@moonshot-ai/kimi-code"

# pi           -> pi       (pi-coding-agent)
install_npm "@mariozechner/pi-coding-agent"

# atomcode     -> atomcode (install channel varies by release; try npm first)
install_npm "atomcode" || true

# dsh          -> dsh      (DeepSeek Harness; install channel varies by release)
install_npm "@deepseek/dsh" || true
install_pip "deepseek-harness" || true

log "Available CLI binaries:"
for bin in claude codex kimi pi atomcode dsh; do
    if command -v "${bin}" >/dev/null 2>&1; then
        log "  ${bin}: $(command -v "${bin}")"
    else
        log "  ${bin}: MISSING"
    fi
done
