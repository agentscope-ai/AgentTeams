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
#   AGENTTEAMS_MATRIX_URL / _USER / _TOKEN
#                                  when all three are set, install.sh also
#                                  writes the sidecar config
#                                  (STATE_DIR/sidecar-config.json, 0600) that
#                                  the production sidecar (sidecar.sh) reads

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

  mkdir -p "$RELEASES_DIR"
  if [ "$mode" = "fresh" ] || [ -e "$release_dir" ]; then
    release_dir="$(mktemp -d "${RELEASES_DIR}/${version}.XXXXXX")"
  else
    mkdir "$release_dir"
  fi

  lifecycle_log "Materializing release directory: ${release_dir}"
  cp -a "${PLUGIN_ROOT}/package.json" "$release_dir/"
  cp -a "${PLUGIN_ROOT}/openclaw.plugin.json" "$release_dir/"
  cp -a "${PLUGIN_ROOT}/README.md" "$release_dir/"
  cp -a "${PLUGIN_ROOT}/dist" "$release_dir/"
  cp -a "${PLUGIN_ROOT}/src" "$release_dir/"
  cp -a "${PLUGIN_ROOT}/scripts" "$release_dir/"
  # The production Python sidecar and its operator entry ship with the
  # release (so a rollback can also restore them). The source tree may lack
  # them (e.g. a capture that only copied the node files), so copy only what
  # exists; the entry is made executable in the materialized release.
  if [ -d "${PLUGIN_ROOT}/sidecar" ]; then
    cp -a "${PLUGIN_ROOT}/sidecar" "$release_dir/"
  fi
  if [ -f "${PLUGIN_ROOT}/sidecar.sh" ]; then
    cp -a "${PLUGIN_ROOT}/sidecar.sh" "$release_dir/"
    chmod +x "$release_dir/sidecar.sh"
  fi
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

# Write the sidecar config (credentials + paths) to
# STATE_DIR/sidecar-config.json (0600). This is the production sidecar's
# config; it is separate from the OpenClaw channel config and is left alone
# by upgrade/rollback (it holds the operator's credentials).
#
# The access token reaches the writer only through the environment — never on
# an argv line, in the script, or in the log. When any of the three
# credentials is missing this logs a warning and writes nothing (the
# installer continues; the sidecar must then be configured by hand).
lifecycle_write_sidecar_config() {
  local url="${AGENTTEAMS_MATRIX_URL:-}"
  local user="${AGENTTEAMS_MATRIX_USER:-}"
  local token="${AGENTTEAMS_MATRIX_TOKEN:-}"
  local target="${STATE_DIR}/sidecar-config.json"

  if [ -z "$url" ] || [ -z "$user" ] || [ -z "$token" ]; then
    lifecycle_log "WARN: sidecar config not written — set AGENTTEAMS_MATRIX_URL, AGENTTEAMS_MATRIX_USER and AGENTTEAMS_MATRIX_TOKEN to have install.sh write ${target} (or write it by hand; see README.md, 'Running the sidecar'). The sidecar cannot start until it exists."
    return 0
  fi

  lifecycle_require_cmd python3
  mkdir -p "$STATE_DIR"
  chmod 700 "$STATE_DIR" 2>/dev/null || true

  local tmp
  tmp="$(mktemp "${STATE_DIR}/.sidecar-config.XXXXXX")"
  if ! AGENTTEAMS_MATRIX_URL="$url" \
    AGENTTEAMS_MATRIX_USER="$user" \
    AGENTTEAMS_MATRIX_TOKEN="$token" \
    AGENTTEAMS_MATRIX_SOCKET="$SOCKET_PATH" \
    AGENTTEAMS_MATRIX_AGENT="$AGENT_NAME" \
    python3 - > "$tmp" <<'PY'
import json, os, sys

data = {
    "homeserver_url": os.environ["AGENTTEAMS_MATRIX_URL"],
    "agent_user": os.environ["AGENTTEAMS_MATRIX_USER"],
    "agent_access_token": os.environ["AGENTTEAMS_MATRIX_TOKEN"],
    "socket_path": os.environ["AGENTTEAMS_MATRIX_SOCKET"],
    "agent_name": os.environ["AGENTTEAMS_MATRIX_AGENT"],
}
sys.stdout.write(json.dumps(data, indent=2) + "\n")
PY
  then
    rm -f "$tmp"
    # Return, do not exit: a sidecar-config write failure must not fail an
    # otherwise successful plugin install (the caller downgrades it to a WARN).
    lifecycle_log "ERROR: failed to write the sidecar config (${target})"
    return 1
  fi
  chmod 600 "$tmp"
  mv "$tmp" "$target"
  lifecycle_log "Wrote sidecar config (0600) at ${target}; start the sidecar with: ${PLUGIN_ROOT}/sidecar.sh start"
}

# A sidecar started before a release switch keeps executing the code of the
# release it was started from. The lifecycle scripts never manage the
# sidecar process — they only tell the operator to restart it ($1 names the
# release the operator should move to).
lifecycle_warn_sidecar_restart() {
  # "Is the sidecar running" is answered by sidecar.sh status, which
  # verifies the pidfile's process identity (a live PID that is not the
  # sidecar — stale or reused — must not trigger the warning). status
  # exits non-zero when the sidecar is not running, so `|| true` keeps
  # the set -e callers (upgrade/rollback) going.
  local status pid
  status="$("${PLUGIN_ROOT}/sidecar.sh" status 2>/dev/null || true)"
  pid=""
  case "$status" in
    *"sidecar running (pid "*)
      pid="${status#*(pid }"
      pid="${pid%%)*}"
      case "$pid" in
        *[!0-9]*) pid="" ;;
      esac
      ;;
  esac
  case "$status" in
    *"sidecar running"*)
      if [ -n "$pid" ]; then
        lifecycle_log "WARN: the sidecar is still running (pid ${pid}); run sidecar.sh restart so it picks up ${1}"
      else
        lifecycle_log "WARN: the sidecar is still running; run sidecar.sh restart so it picks up ${1}"
      fi
      ;;
  esac
  return 0
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

# Inventory state of the plugin in the cold inventory. Returns:
#   0  listed — the plugin id is present in the inventory
#   1  absent — the inventory was read successfully and the plugin is not in it
#   2  unreadable — the inventory command failed, or its output is not the
#      expected inventory JSON. Callers must NOT treat this as "absent": an
#      unreadable inventory says nothing about the install, and acting on it
#      (e.g. clearing the release markers during uninstall) would leave the
#      plugin installed but unmanaged.
lifecycle_is_listed() {
  local json rc
  if ! json="$("$OPENCLAW_BIN" plugins list --json)"; then
    return 2
  fi
  rc=0
  printf '%s' "$json" | jq -e --arg id "$PLUGIN_ID" \
    '[.. | objects | select(.id == $id)] | length > 0' >/dev/null || rc=$?
  case "$rc" in
    0) return 0 ;;
    1) return 1 ;;
    *) return 2 ;;
  esac
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
  # Post-install, both inventory failures are fatal, but they must be
  # reported as different conditions: an unreadable inventory means the
  # install could not be verified, not that the plugin is missing.
  local listed_rc=0
  lifecycle_is_listed || listed_rc=$?
  if [ "$listed_rc" -eq 2 ]; then
    lifecycle_fail "could not read the plugin inventory ('openclaw plugins list --json' failed); the install is not verified"
  elif [ "$listed_rc" -ne 0 ]; then
    lifecycle_fail "plugin ${PLUGIN_ID} not found in 'openclaw plugins list --json'"
  fi

  local enabled_rc=0 enabled_out
  enabled_out="$("$OPENCLAW_BIN" plugins list --enabled 2>&1)" || enabled_rc=$?
  if [ "$enabled_rc" -ne 0 ]; then
    printf '%s\n' "$enabled_out" >&2
    lifecycle_fail "could not read the enabled-plugin list ('openclaw plugins list --enabled' exited ${enabled_rc}); the install is not verified"
  fi
  if printf '%s\n' "$enabled_out" | grep -Fq "$PLUGIN_ID"; then
    lifecycle_log "OK: ${PLUGIN_ID} is installed and enabled"
  else
    lifecycle_fail "plugin ${PLUGIN_ID} is installed but not enabled ('openclaw plugins list --enabled')"
  fi
}
