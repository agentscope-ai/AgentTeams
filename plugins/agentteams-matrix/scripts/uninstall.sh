#!/usr/bin/env bash
# Bundle uninstaller: removes the agentteams-matrix install from an
# AgentTeams runtime via that runtime's adapter. Mirrors install.sh.
#
# Usage:
#   uninstall.sh [--adapter <qwenpaw|openclaw|hermes>] [adapter options]
#
# Without --adapter, every detected runtime is uninstalled (search
# order: qwenpaw, openclaw, hermes). A runtime whose binary is on PATH
# is detected as before; OpenClaw additionally counts as present when
# its effective config exists — $OPENCLAW_CONFIG_PATH (exported by the
# Worker entrypoint), then the legacy $OPENCLAW_CONFIG, then
# $HOME/.openclaw/openclaw.json — so a Worker whose only signal is the
# config path is detected without the binary on PATH. Adapter options
# given after the adapter name (e.g. --config, --dry-run for the
# openclaw adapter) are forwarded to the selected adapter's
# uninstall.sh; the qwenpaw and hermes adapters take no options.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUNDLE_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
export AGENTTEAMS_BUNDLE_DIR="${AGENTTEAMS_BUNDLE_DIR:-$BUNDLE_DIR}"

ADAPTER=""
FORWARD=()

usage() {
  cat <<'EOF'
Usage: uninstall.sh [--adapter <qwenpaw|openclaw|hermes>] [adapter options]

Adapters:
  qwenpaw    remove the matrix channel plugin from the QwenPaw working dir
             (takes no options)
  openclaw   remove the channels.matrix projection from
             ~/.openclaw/openclaw.json (options: --config, --dry-run)
  hermes     remove the staged matrix platform plugin from $HERMES_HOME
             (default ~/.hermes): the payload under
             $HERMES_HOME/plugins/agentteams-matrix plus the
             ownership marker (takes no options)

Without --adapter, every detected runtime (search order: qwenpaw,
openclaw, hermes) is uninstalled. OpenClaw counts as present when its
effective config exists ($OPENCLAW_CONFIG_PATH, then $OPENCLAW_CONFIG,
then ~/.openclaw/openclaw.json), even without the binary on PATH.
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --adapter)
      if [ $# -lt 2 ]; then
        echo "ERROR: --adapter requires a value (qwenpaw, openclaw, or hermes)" >&2
        exit 2
      fi
      ADAPTER="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      FORWARD+=("$1")
      shift
      ;;
  esac
done

# Effective OpenClaw config path, same resolution order the adapter
# uses: the Worker's config-path environment first, then the legacy
# variable, then the normal default.
openclaw_effective_config() {
  if [ -n "${OPENCLAW_CONFIG_PATH:-}" ]; then
    printf '%s' "${OPENCLAW_CONFIG_PATH}"
  elif [ -n "${OPENCLAW_CONFIG:-}" ]; then
    printf '%s' "${OPENCLAW_CONFIG}"
  else
    printf '%s' "${HOME}/.openclaw/openclaw.json"
  fi
}

runtime_present() {
  case "$1" in
    qwenpaw) command -v qwenpaw >/dev/null 2>&1 ;;
    openclaw)
      # -e follows symlinks, so a link to an existing canonical file
      # counts; a dangling link does not.
      command -v openclaw >/dev/null 2>&1 || [ -e "$(openclaw_effective_config)" ]
      ;;
    hermes) command -v hermes >/dev/null 2>&1 ;;
    *) return 1 ;;
  esac
}

uninstall_adapter() {
  local adapter="$1"
  if [ "${#FORWARD[@]}" -gt 0 ]; then
    if [ "${adapter}" = "qwenpaw" ]; then
      echo "ERROR: the qwenpaw adapter takes no options; drop: ${FORWARD[*]}" >&2
      exit 2
    fi
    if [ "${adapter}" = "hermes" ]; then
      echo "ERROR: the hermes adapter takes no options; drop: ${FORWARD[*]}" >&2
      exit 2
    fi
    bash "${BUNDLE_DIR}/adapters/${adapter}/uninstall.sh" "${FORWARD[@]}"
  else
    bash "${BUNDLE_DIR}/adapters/${adapter}/uninstall.sh"
  fi
}

if [ -n "${ADAPTER}" ]; then
  case "${ADAPTER}" in
    qwenpaw|openclaw|hermes) ;;
    *)
      echo "ERROR: unknown adapter: ${ADAPTER} (expected qwenpaw, openclaw, or hermes)" >&2
      usage >&2
      exit 2
      ;;
  esac
  if ! runtime_present "${ADAPTER}"; then
    echo "WARNING: ${ADAPTER} binary not found on PATH; the adapter still removes its files" >&2
  fi
  uninstall_adapter "${ADAPTER}"
  exit 0
fi

ran=0
for candidate in qwenpaw openclaw hermes; do
  if runtime_present "${candidate}"; then
    uninstall_adapter "${candidate}"
    ran=1
  fi
done

if [ "${ran}" -eq 0 ]; then
  echo "ERROR: no supported Matrix channel runtime found; expected qwenpaw, openclaw, or hermes" >&2
  exit 1
fi
