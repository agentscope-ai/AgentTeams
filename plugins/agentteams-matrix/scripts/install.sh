#!/usr/bin/env bash
# Bundle installer: installs the agentteams-matrix bundle onto an
# AgentTeams runtime via that runtime's adapter (OpenClaw projects
# the contract onto openclaw.json).
#
# Usage:
#   install.sh [--adapter <openclaw>] [adapter options]
#
# Without --adapter, the first detected runtime is used (search order:
# openclaw). A runtime whose binary is on PATH is
# detected as before; OpenClaw additionally counts as present when its
# effective config exists — $OPENCLAW_CONFIG_PATH (exported by the
# Worker entrypoint), then the legacy $OPENCLAW_CONFIG, then
# $HOME/.openclaw/openclaw.json — so a Worker whose only signal is the
# config path is detected without the binary on PATH. Adapter options
# given after the adapter name (e.g. --config, --dry-run for the
# openclaw adapter) are forwarded to the selected adapter's
# install.sh.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUNDLE_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
export AGENTTEAMS_BUNDLE_DIR="${AGENTTEAMS_BUNDLE_DIR:-$BUNDLE_DIR}"
export AGENTTEAMS_PLUGIN_DIR="${AGENTTEAMS_PLUGIN_DIR:-$BUNDLE_DIR}"

ADAPTER=""
FORWARD=()

usage() {
  cat <<'EOF'
Usage: install.sh [--adapter <openclaw>] [adapter options]

Adapters:
  openclaw   project channels.matrix into ~/.openclaw/openclaw.json
             (options: --config, --dry-run, --with-credentials)

Without --adapter, the first detected runtime (search order:
openclaw) is installed. OpenClaw counts as present when its
effective config exists ($OPENCLAW_CONFIG_PATH, then $OPENCLAW_CONFIG,
then ~/.openclaw/openclaw.json), even without the binary on PATH.
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --adapter)
      if [ $# -lt 2 ]; then
        echo "ERROR: --adapter requires a value (openclaw)" >&2
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
    openclaw)
      # -e follows symlinks, so a link to an existing canonical file
      # counts; a dangling link does not.
      command -v openclaw >/dev/null 2>&1 || [ -e "$(openclaw_effective_config)" ]
      ;;
    *) return 1 ;;
  esac
}

install_adapter() {
  local adapter="$1"
  if [ "${#FORWARD[@]}" -gt 0 ]; then
    bash "${BUNDLE_DIR}/adapters/${adapter}/install.sh" "${FORWARD[@]}"
  else
    bash "${BUNDLE_DIR}/adapters/${adapter}/install.sh"
  fi
}

if [ -n "${ADAPTER}" ]; then
  case "${ADAPTER}" in
    openclaw) ;;
    *)
      echo "ERROR: unknown adapter: ${ADAPTER} (expected openclaw)" >&2
      usage >&2
      exit 2
      ;;
  esac
  if ! runtime_present "${ADAPTER}"; then
    echo "WARNING: ${ADAPTER} binary not found on PATH; the adapter still installs its files" >&2
  fi
  install_adapter "${ADAPTER}"
  exit 0
fi

ran=0
for candidate in openclaw; do
  if runtime_present "${candidate}"; then
    install_adapter "${candidate}"
    ran=1
  fi
done

if [ "${ran}" -eq 0 ]; then
  echo "ERROR: no supported Matrix channel runtime found; expected openclaw" >&2
  exit 1
fi
