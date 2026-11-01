#!/usr/bin/env bash
# Bootstrap the isolated OpenClaw home for the e2e run.
#
# Order matters (#1470): the CLI validates the WHOLE config on every command,
# and a `channels.<id>` section for a plugin that is not registered yet is
# rejected with `unknown channel id: <id>`. The e2e used to write the channel
# first and then install, so a fresh home failed before the Gateway or the
# Agent ever ran.
#
#   1. `openclaw plugins install` — registers the channel id (config valid,
#      no channel section yet)
#   2. write `channels.<id>` through the shared lifecycle helper
#   3. `openclaw plugins enable`
#   4. `openclaw config validate --json` — fail the run on an invalid config
#
# Environment:
#   OPENCLAW_CONFIG_PATH          the isolated config (required)
#   E2E_RELEASE_DIR               plugin directory to install (required)
#   AGENTTEAMS_MATRIX_SOCKET      sidecar socket the channel should use
#   AGENTTEAMS_MATRIX_AGENT       agent name the channel should use
#   OPENCLAW_BIN                  openclaw binary (default: openclaw)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib-lifecycle.sh
source "${SCRIPT_DIR}/lib-lifecycle.sh"

: "${OPENCLAW_CONFIG_PATH:?OPENCLAW_CONFIG_PATH must point at the isolated openclaw.json}"
: "${E2E_RELEASE_DIR:?E2E_RELEASE_DIR must point at the plugin directory to install}"

e2e_bootstrap_die() { echo "e2e: $*" >&2; exit 1; }

lifecycle_require_cmd jq

[ -f "$OPENCLAW_CONFIG_PATH" ] \
  || e2e_bootstrap_die "config missing (write the base config first): $OPENCLAW_CONFIG_PATH"
[ -f "${E2E_RELEASE_DIR}/openclaw.plugin.json" ] \
  || e2e_bootstrap_die "release has no openclaw.plugin.json: $E2E_RELEASE_DIR"

# The base config must not carry our channel yet: installing first keeps the
# config valid and makes a failed install leave it untouched.
if jq -e --arg id "$PLUGIN_ID" '.channels[$id] // empty' "$OPENCLAW_CONFIG_PATH" >/dev/null 2>&1; then
  e2e_bootstrap_die "the base config already contains channels.${PLUGIN_ID}; write the base config without it"
fi

echo "e2e: installing the plugin into the isolated home (registers the channel id)"
"$OPENCLAW_BIN" plugins install --force --accept-capabilities "$E2E_RELEASE_DIR" \
  || e2e_bootstrap_die "openclaw plugins install failed"

echo "e2e: writing channels.${PLUGIN_ID}"
lifecycle_apply_channel_config

echo "e2e: enabling ${PLUGIN_ID}"
"$OPENCLAW_BIN" plugins enable "$PLUGIN_ID" \
  || e2e_bootstrap_die "openclaw plugins enable failed"

echo "e2e: validating the isolated config"
"$OPENCLAW_BIN" config validate --json >/dev/null \
  || e2e_bootstrap_die "openclaw config validate rejected the config after the channel write"

echo "e2e: bootstrap ok"
