#!/usr/bin/env bash
# Shared helpers for the agentteams-matrix OpenClaw plugin lifecycle scripts
# (install.sh, upgrade.sh, rollback.sh, uninstall.sh). Source, do not execute.
#
# Conventions
#   - All plugin state lives under STATE_DIR (default
#     ~/.agentteams/openclaw-matrix):
#       releases/<name>  self-contained release directories (source + built
#                        dist + node_modules), so rollback never rebuilds
#       active           absolute path of the installed release
#       previous         absolute path of the release before the last
#                        install/upgrade (rollback target)
#   - The channel config is written to the OpenClaw config (env
#     OPENCLAW_CONFIG_PATH, default ~/.openclaw/openclaw.json) under
#     channels.agentteams-matrix with enabled/socketPath/agent. Those three
#     keys are managed by the scripts (env-driven); any other keys already
#     present in the section are preserved.
#
# Environment
#   AGENTTEAMS_OPENCLAW_STATE_DIR  plugin state dir (default ~/.agentteams/openclaw-matrix)
#   OPENCLAW_BIN                   openclaw CLI (default: openclaw on PATH)
#   OPENCLAW_CONFIG_PATH           openclaw.json (default ~/.openclaw/openclaw.json)
#   AGENTTEAMS_MATRIX_SOCKET       sidecar socket path written to the config
#                                  (default ${AGENTTEAMS_MATRIX_STATE_DIR:-$HOME/.agentteams/matrix}/sidecar.sock)
#   AGENTTEAMS_MATRIX_AGENT        agent name written to the config (default: agentteams)

PLUGIN_ID="agentteams-matrix"

LIFECYCLE_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PLUGIN_ROOT="$(cd "${LIFECYCLE_SCRIPT_DIR}/.." && pwd)"

STATE_DIR="${AGENTTEAMS_OPENCLAW_STATE_DIR:-$HOME/.agentteams/openclaw-matrix}"
RELEASES_DIR="${STATE_DIR}/releases"
ACTIVE_FILE="${STATE_DIR}/active"
PREVIOUS_FILE="${STATE_DIR}/previous"

OPENCLAW_BIN="${OPENCLAW_BIN:-openclaw}"
OPENCLAW_CONFIG="${OPENCLAW_CONFIG_PATH:-$HOME/.openclaw/openclaw.json}"
SOCKET_PATH="${AGENTTEAMS_MATRIX_SOCKET:-${AGENTTEAMS_MATRIX_STATE_DIR:-$HOME/.agentteams/matrix}/sidecar.sock}"
AGENT_NAME="${AGENTTEAMS_MATRIX_AGENT:-agentteams}"

lifecycle_log() { printf '[agentteams-matrix] %s\n' "$*" >&2; }
lifecycle_fail() { printf '[agentteams-matrix] ERROR: %s\n' "$*" >&2; exit 1; }

lifecycle_require_cmd() {
  command -v "$1" >/dev/null 2>&1 || lifecycle_fail "required command not found: $1"
}

lifecycle_plugin_version() {
  jq -r '.version' "${PLUGIN_ROOT}/package.json"
}

# Print the marker file's content, or nothing when the marker is absent.
lifecycle_read_marker() {
  if [ -f "$1" ]; then cat "$1"; fi
}

# Write a marker (absolute path) or remove it when the value is empty.
lifecycle_write_marker() {
  if [ -n "$2" ]; then
    mkdir -p "$(dirname "$1")"
    printf '%s\n' "$2" > "$1"
  else
    rm -f "$1"
  fi
}

# Build the plugin and materialize a self-contained release directory under
# RELEASES_DIR; prints its absolute path.
#   $1: "reuse" (install.sh) — reuse an existing complete release for the
#         current version; "fresh" (upgrade.sh) — always create a new one.
lifecycle_build_release() {
  local mode="${1:-reuse}"
  local version release_dir

  lifecycle_require_cmd node
  lifecycle_require_cmd npm
  lifecycle_require_cmd jq

  version="$(lifecycle_plugin_version)"
  release_dir="${RELEASES_DIR}/${version}"

  if [ "$mode" = "reuse" ] && [ -d "$release_dir" ] && [ -f "${release_dir}/dist/src/index.js" ]; then
    lifecycle_log "Reusing existing release directory: ${release_dir}"
    printf '%s\n' "$release_dir"
    return 0
  fi

  lifecycle_log "Installing dependencies and building (this may take a while)..."
  # stdout of this function is the release path consumed by
  # `release_dir="$(lifecycle_build_release ...)"`; npm/tsc write their own
  # progress to stdout, which would glue itself to that path (Pi round: the
  # installer then reported `release directory missing:` with a multi-line
  # argument). Keep the build logs on stderr.
  (cd "$PLUGIN_ROOT" && npm install --no-audit --no-fund && npm run build) >&2 \
    || lifecycle_fail "npm install/build failed in ${PLUGIN_ROOT}"

  if [ -d "$release_dir" ]; then
    release_dir="${RELEASES_DIR}/${version}.$(date -u +%Y%m%dT%H%M%SZ)"
  fi

  lifecycle_log "Materializing release directory: ${release_dir}"
  rm -rf "$release_dir"
  mkdir -p "$release_dir"
  cp -a "${PLUGIN_ROOT}/package.json" "$release_dir/"
  cp -a "${PLUGIN_ROOT}/openclaw.plugin.json" "$release_dir/"
  cp -a "${PLUGIN_ROOT}/README.md" "$release_dir/"
  cp -a "${PLUGIN_ROOT}/dist" "$release_dir/"
  cp -a "${PLUGIN_ROOT}/src" "$release_dir/"
  cp -a "${PLUGIN_ROOT}/scripts" "$release_dir/"
  # Full node_modules copy keeps the release self-contained: the managed
  # install (and any later rollback to it) resolves openclaw/plugin-sdk/*
  # without the source checkout.
  cp -a "${PLUGIN_ROOT}/node_modules" "$release_dir/"

  printf '%s\n' "$release_dir"
}

# Write channels.agentteams-matrix into the OpenClaw config (idempotent).
lifecycle_apply_channel_config() {
  lifecycle_require_cmd jq
  mkdir -p "$(dirname "$OPENCLAW_CONFIG")"
  if [ ! -f "$OPENCLAW_CONFIG" ]; then
    lifecycle_log "Creating OpenClaw config at ${OPENCLAW_CONFIG}"
    printf '{}\n' > "$OPENCLAW_CONFIG"
  fi

  local tmp mode
  tmp="$(mktemp)"
  mode="$(stat -c '%a' "$OPENCLAW_CONFIG" 2>/dev/null || true)"
  # The channel section lives at channels.<id> (OpenClaw's config schema
  # rejects a platform key at the root: `Unrecognized key: "agentteams-matrix"`).
  # The merge keeps every other channel and every other key inside our own
  # section: only enabled/socketPath/agent are managed here.
  if ! jq --arg id "$PLUGIN_ID" --arg socket "$SOCKET_PATH" --arg agent "$AGENT_NAME" '
    (.channels //= {})
    | .channels[$id] = ((.channels[$id] // {}) + {
        enabled: true,
        socketPath: $socket,
        agent: $agent
      })
  ' "$OPENCLAW_CONFIG" > "$tmp"; then
    rm -f "$tmp"
    lifecycle_fail "cannot update ${OPENCLAW_CONFIG} (channels.${PLUGIN_ID}); the file was left unchanged"
  fi
  if [ -n "$mode" ]; then chmod "$mode" "$tmp"; fi
  mv "$tmp" "$OPENCLAW_CONFIG"
  lifecycle_log "Wrote channels.${PLUGIN_ID} (socketPath=${SOCKET_PATH}, agent=${AGENT_NAME}) to ${OPENCLAW_CONFIG}"
}

# Remove our owned config sections (uninstall --purge).
lifecycle_purge_channel_config() {
  lifecycle_require_cmd jq
  if [ ! -f "$OPENCLAW_CONFIG" ]; then
    lifecycle_log "No OpenClaw config at ${OPENCLAW_CONFIG}; nothing to purge"
    return 0
  fi
  local tmp mode
  tmp="$(mktemp)"
  mode="$(stat -c '%a' "$OPENCLAW_CONFIG" 2>/dev/null || true)"
  if ! jq --arg id "$PLUGIN_ID" '
    if (.channels[$id]? != null) then del(.channels[$id]) else . end
    | if (.plugins.entries[$id]? != null) then del(.plugins.entries[$id]) else . end
  ' "$OPENCLAW_CONFIG" > "$tmp"; then
    rm -f "$tmp"
    lifecycle_fail "cannot purge from ${OPENCLAW_CONFIG}; the file was left unchanged"
  fi
  if [ -n "$mode" ]; then chmod "$mode" "$tmp"; fi
  mv "$tmp" "$OPENCLAW_CONFIG"
  lifecycle_log "Purged channels.${PLUGIN_ID} (and plugins.entries.${PLUGIN_ID}) from ${OPENCLAW_CONFIG}"
}

# True when the plugin id appears in the cold inventory.
lifecycle_is_listed() {
  local json
  json="$("$OPENCLAW_BIN" plugins list --json)" \
    || { lifecycle_log "WARN: 'openclaw plugins list --json' failed"; return 1; }
  printf '%s' "$json" | jq -e --arg id "$PLUGIN_ID" \
    '[.. | objects | select(.id == $id)] | length > 0' >/dev/null
}

# Managed-install a release directory, write config, enable, reload, verify.
lifecycle_install_release() {
  local release_dir="$1"

  lifecycle_require_cmd jq
  [ -d "$release_dir" ] || lifecycle_fail "release directory missing: $release_dir"
  [ -f "${release_dir}/openclaw.plugin.json" ] \
    || lifecycle_fail "release has no openclaw.plugin.json: $release_dir"
  [ -f "${release_dir}/dist/src/index.js" ] \
    || lifecycle_fail "release has no built entry (dist/src/index.js): $release_dir"

  lifecycle_log "Installing plugin from ${release_dir}"
  # --force: noninteractive local-path install (source trust) and overwrite
  # of an existing install target. --accept-capabilities: this plugin declares
  # no consent-requiring capabilities; the flag keeps the script
  # noninteractive on hosts that prompt for local copies.
  "$OPENCLAW_BIN" plugins install --force --accept-capabilities "$release_dir" \
    || lifecycle_fail "openclaw plugins install failed for $release_dir"

  # The channel section may only be written AFTER the plugin is registered:
  # the CLI validates the whole config on every command, and on a fresh home
  # it rejects the invalid config with `unknown channel id: <id>`, which made
  # `plugins install` fail and left the invalid section behind (round 14).
  # Installing first also means a failed install leaves the config untouched.
  lifecycle_apply_channel_config

  lifecycle_log "Enabling ${PLUGIN_ID}"
  "$OPENCLAW_BIN" plugins enable "$PLUGIN_ID" \
    || lifecycle_fail "openclaw plugins enable failed"

  lifecycle_log "Reloading ${PLUGIN_ID} (best effort: skipped cleanly when no Gateway is running)"
  "$OPENCLAW_BIN" plugins reload "$PLUGIN_ID" --wait \
    || lifecycle_log "WARN: reload failed — the plugin will load on the next Gateway start"

  lifecycle_log "Verifying plugin inventory"
  lifecycle_is_listed \
    || lifecycle_fail "plugin ${PLUGIN_ID} not found in 'openclaw plugins list --json'"
  if "$OPENCLAW_BIN" plugins list --enabled | grep -Fq "$PLUGIN_ID"; then
    lifecycle_log "OK: ${PLUGIN_ID} is installed and enabled"
  else
    lifecycle_fail "plugin ${PLUGIN_ID} is installed but not enabled ('openclaw plugins list --enabled')"
  fi
}
