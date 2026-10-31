#!/usr/bin/env bash
set -euo pipefail

# TeamHarness package installer — the documented default entrypoint.
#
# Usage: install.sh [--runtime auto|qwenpaw|openclaw|hermes]... [--dry-run]
#
# Detects the supported local runtimes and dispatches to the matching
# adapters. Detection (default, --runtime auto): every detected runtime is
# installed, so coexisting runtimes are all covered:
#
#   qwenpaw   qwenpaw on PATH
#   openclaw  openclaw on PATH, $OPENCLAW_CONFIG_PATH set, $OPENCLAW_CONFIG
#             set, or the effective config path (default
#             ~/.openclaw/openclaw.json) present — a symlink counts
#   hermes    hermes on PATH, $HERMES_HOME set, or
#             ~/.hermes/config.yaml present
#
# --runtime NAME (repeatable) selects that runtime explicitly and bypasses
# detection for it. --dry-run is forwarded to every adapter and writes
# nothing. Failures are collected per runtime; the exit code is 1 when no
# runtime was selected or a selected adapter failed.
#
# The adapters read $OPENCLAW_CONFIG_PATH, $OPENCLAW_CONFIG,
# $OPENCLAW_WORKSPACE, $HERMES_HOME and $HERMES_PLUGIN_HOME themselves; this
# script never overrides them. Detection and the adapters resolve the
# effective OpenClaw config the same way: $OPENCLAW_CONFIG_PATH (the Worker
# export), then $OPENCLAW_CONFIG (legacy), then ~/.openclaw/openclaw.json.
#
# With TEAMHARNESS_INSTALL_LOG set, one JSON line is appended per run that
# installed at least one runtime:
#   {"event":"install","runtimes":["..."],"pluginDir":"..."}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PLUGIN_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
export AGENTTEAMS_BUNDLE_DIR="${AGENTTEAMS_BUNDLE_DIR:-$PLUGIN_DIR}"
export AGENTTEAMS_PLUGIN_DIR="${AGENTTEAMS_PLUGIN_DIR:-$PLUGIN_DIR}"

SUPPORTED_RUNTIMES=(qwenpaw openclaw hermes)

usage() {
  cat <<'EOF'
Usage: install.sh [--runtime auto|qwenpaw|openclaw|hermes]... [--dry-run]

Installs the TeamHarness bundle into the supported local runtimes
(qwenpaw, openclaw, hermes). By default every detected runtime is
installed; --runtime selects runtimes explicitly (repeatable, bypasses
detection); --dry-run previews through every adapter without writing.
EOF
}

DRY_RUN=0
RUNTIME_SELECTIONS=()
while [ $# -gt 0 ]; do
  case "$1" in
    --runtime)
      value="${2:?--runtime requires a value}"
      case "$value" in
        auto|qwenpaw|openclaw|hermes) ;;
        *)
          echo "ERROR: unknown runtime: $value (supported: auto, qwenpaw, openclaw, hermes)" >&2
          exit 2
          ;;
      esac
      RUNTIME_SELECTIONS+=("$value")
      shift 2
      ;;
    --dry-run)
      DRY_RUN=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "ERROR: unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

# --runtime selections bypass detection for their runtime; "auto" (also the
# default when no --runtime was given) adds every detected runtime.
AUTO=1
if [ "${#RUNTIME_SELECTIONS[@]}" -gt 0 ]; then
  AUTO=0
  for value in "${RUNTIME_SELECTIONS[@]}"; do
    if [ "$value" = "auto" ]; then
      AUTO=1
    fi
  done
fi

detect_qwenpaw() {
  command -v qwenpaw >/dev/null 2>&1
}

# The effective OpenClaw config path, same priority the adapters use:
# $OPENCLAW_CONFIG_PATH (the Worker export), then $OPENCLAW_CONFIG (legacy),
# then the runtime default.
openclaw_config_file() {
  if [ -n "${OPENCLAW_CONFIG_PATH:-}" ]; then
    printf '%s' "$OPENCLAW_CONFIG_PATH"
  elif [ -n "${OPENCLAW_CONFIG:-}" ]; then
    printf '%s' "$OPENCLAW_CONFIG"
  else
    printf '%s' "$HOME/.openclaw/openclaw.json"
  fi
}

detect_openclaw() {
  command -v openclaw >/dev/null 2>&1 \
    || [ -n "${OPENCLAW_CONFIG_PATH:-}" ] \
    || [ -n "${OPENCLAW_CONFIG:-}" ] \
    || [ -e "$(openclaw_config_file)" ]
}

detect_hermes() {
  command -v hermes >/dev/null 2>&1 \
    || [ -n "${HERMES_HOME:-}" ] \
    || [ -f "$HOME/.hermes/config.yaml" ]
}

SELECTED_RUNTIMES=()
for runtime in "${SUPPORTED_RUNTIMES[@]}"; do
  explicit=0
  for value in ${RUNTIME_SELECTIONS[@]+"${RUNTIME_SELECTIONS[@]}"}; do
    if [ "$value" = "$runtime" ]; then
      explicit=1
    fi
  done
  if [ "$explicit" -eq 1 ]; then
    case " ${SELECTED_RUNTIMES[*]-} " in
      *" $runtime "*) : ;;
      *) SELECTED_RUNTIMES+=("$runtime") ;;
    esac
    continue
  fi
  if [ "$AUTO" -ne 1 ]; then
    continue
  fi
  detected=0
  case "$runtime" in
    qwenpaw) detect_qwenpaw && detected=1 ;;
    openclaw) detect_openclaw && detected=1 ;;
    hermes) detect_hermes && detected=1 ;;
  esac
  if [ "$detected" -eq 1 ]; then
    SELECTED_RUNTIMES+=("$runtime")
  fi
done

# Per-runtime status as plain variables (this entrypoint must run on the
# system shell, bash 3.2 on macOS; the portability test pins that).
STATUS_QWENPAW="skipped (not detected)"
STATUS_OPENCLAW="skipped (not detected)"
STATUS_HERMES="skipped (not detected)"

set_status() {  # $1=runtime $2=status
  case "$1" in
    qwenpaw)  STATUS_QWENPAW="$2" ;;
    openclaw) STATUS_OPENCLAW="$2" ;;
    hermes)   STATUS_HERMES="$2" ;;
  esac
}

get_status() {  # $1=runtime
  case "$1" in
    qwenpaw)  printf '%s' "$STATUS_QWENPAW" ;;
    openclaw) printf '%s' "$STATUS_OPENCLAW" ;;
    hermes)   printf '%s' "$STATUS_HERMES" ;;
  esac
}

ADAPTER_ARGS=()
if [ "$DRY_RUN" -eq 1 ]; then
  ADAPTER_ARGS=(--dry-run)
fi

OVERALL_RC=0
for runtime in ${SELECTED_RUNTIMES[@]+"${SELECTED_RUNTIMES[@]}"}; do
  script="${PLUGIN_DIR}/adapters/${runtime}/install.sh"
  rc=0
  bash "$script" ${ADAPTER_ARGS[@]+"${ADAPTER_ARGS[@]}"} || rc=$?
  if [ "$rc" -eq 0 ]; then
    if [ "$DRY_RUN" -eq 1 ]; then
      set_status "$runtime" "installed (dry-run)"
    else
      set_status "$runtime" "installed"
    fi
  else
    set_status "$runtime" "failed (exit $rc)"
    OVERALL_RC=1
  fi
done

for runtime in "${SUPPORTED_RUNTIMES[@]}"; do
  echo "${runtime}: $(get_status "$runtime")"
done

if [ "${#SELECTED_RUNTIMES[@]}" -eq 0 ]; then
  {
    echo "ERROR: no supported TeamHarness local runtime found."
    echo "Supported runtimes: ${SUPPORTED_RUNTIMES[*]}."
    echo "Select one explicitly: install.sh --runtime <qwenpaw|openclaw|hermes>"
  } >&2
  exit 1
fi

if [ "$OVERALL_RC" -ne 0 ]; then
  failed=()
  for runtime in "${SUPPORTED_RUNTIMES[@]}"; do
    case "$(get_status "$runtime")" in
      "failed (exit"*) failed+=("$runtime") ;;
    esac
  done
  {
    echo "ERROR: TeamHarness install failed for: ${failed[*]+"${failed[*]}"}."
    echo "Supported runtimes: ${SUPPORTED_RUNTIMES[*]}. Select explicitly: install.sh --runtime <qwenpaw|openclaw|hermes>"
  } >&2
  exit 1
fi

LOG_FILE="${TEAMHARNESS_INSTALL_LOG:-}"
if [ -n "$LOG_FILE" ] && [ "$DRY_RUN" -eq 0 ]; then
  runtimes_json=""
  for runtime in "${SUPPORTED_RUNTIMES[@]}"; do
    if [ "$(get_status "$runtime")" = "installed" ]; then
      if [ -n "$runtimes_json" ]; then
        runtimes_json="${runtimes_json},"
      fi
      runtimes_json="${runtimes_json}\"${runtime}\""
    fi
  done
  if [ -n "$runtimes_json" ]; then
    mkdir -p "$(dirname "$LOG_FILE")"
    printf '{"event":"install","runtimes":[%s],"pluginDir":"%s"}\n' \
      "$runtimes_json" "$AGENTTEAMS_PLUGIN_DIR" >> "$LOG_FILE"
  fi
fi
