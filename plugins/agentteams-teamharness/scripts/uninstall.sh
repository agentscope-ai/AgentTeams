#!/usr/bin/env bash
set -euo pipefail

# TeamHarness package uninstaller — the documented default entrypoint.
#
# Usage: uninstall.sh [--runtime auto|qwenpaw|openclaw|hermes]... [--dry-run]
#
# Detection is artifact-driven (default, --runtime auto), so residue is
# removed even after the runtime binary is gone:
#
#   qwenpaw   qwenpaw on PATH
#   openclaw  the effective config file ($OPENCLAW_CONFIG_PATH,
#             $OPENCLAW_CONFIG, or ~/.openclaw/openclaw.json — a symlink
#             counts) or the staged plugin root
#             <config dir>/plugins/agentteams-teamharness exists, where
#             <config dir> is the config's directory as resolved through
#             the symlink (the canonical directory)
#   hermes    $HERMES_HOME is set, or ~/.hermes/config.yaml, or
#             ~/.hermes/plugins/agentteams-teamharness exists
#
# The adapters are idempotent, so selecting a runtime with nothing
# installed is a successful no-op; nothing detected is a clean exit 0.
# Failures are collected per runtime; the exit code is 1 when a selected
# adapter failed. --runtime and --dry-run behave as in install.sh.
#
# With TEAMHARNESS_INSTALL_LOG set, one JSON line is appended per run that
# uninstalled at least one runtime:
#   {"event":"uninstall","runtimes":["..."],"pluginDir":"..."}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PLUGIN_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
export AGENTTEAMS_PLUGIN_DIR="${AGENTTEAMS_PLUGIN_DIR:-$PLUGIN_DIR}"

SUPPORTED_RUNTIMES=(qwenpaw openclaw hermes)

usage() {
  cat <<'EOF'
Usage: uninstall.sh [--runtime auto|qwenpaw|openclaw|hermes]... [--dry-run]

Removes the TeamHarness installation from the supported local runtimes
(qwenpaw, openclaw, hermes). Detection is artifact-driven (config files
and staged plugin roots, not just the runtime binary), so residue is
removed even after the binary is gone; --runtime selects runtimes
explicitly (repeatable, bypasses detection); --dry-run previews through
every adapter without writing.
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

# The directory that actually holds the config: follow a symlink chain on
# the config path itself (the Worker links ~/.openclaw/openclaw.json to its
# canonical $HOME/openclaw.json, and install stages the plugin root next to
# the canonical file). One readlink per hop keeps this portable to the
# system shell (bash 3.2 on macOS has no realpath); a hop cap guards
# against link cycles. Parent directories are taken as given — every
# supported layout keeps them real.
canonical_config_dir() {
  local path="$1" hops=0 target
  while [ -L "$path" ] && [ "$hops" -lt 10 ]; do
    target="$(readlink "$path")"
    case "$target" in
      /*) path="$target" ;;
      *)  path="$(dirname "$path")/$target" ;;
    esac
    hops=$((hops + 1))
  done
  printf '%s' "$(dirname "$path")"
}

detect_qwenpaw() {
  command -v qwenpaw >/dev/null 2>&1
}

detect_openclaw() {
  config="$(openclaw_config_file)"
  [ -e "$config" ] \
    || [ -d "$(canonical_config_dir "$config")/plugins/agentteams-teamharness" ]
}

detect_hermes() {
  [ -n "${HERMES_HOME:-}" ] \
    || [ -f "$HOME/.hermes/config.yaml" ] \
    || [ -d "$HOME/.hermes/plugins/agentteams-teamharness" ]
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
  script="${PLUGIN_DIR}/adapters/${runtime}/uninstall.sh"
  rc=0
  bash "$script" ${ADAPTER_ARGS[@]+"${ADAPTER_ARGS[@]}"} || rc=$?
  if [ "$rc" -eq 0 ]; then
    if [ "$DRY_RUN" -eq 1 ]; then
      set_status "$runtime" "uninstalled (dry-run)"
    else
      set_status "$runtime" "uninstalled"
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
  echo "no TeamHarness installation found (supported runtimes: ${SUPPORTED_RUNTIMES[*]}); nothing to uninstall"
  exit 0
fi

if [ "$OVERALL_RC" -ne 0 ]; then
  failed=()
  for runtime in "${SUPPORTED_RUNTIMES[@]}"; do
    case "$(get_status "$runtime")" in
      "failed (exit"*) failed+=("$runtime") ;;
    esac
  done
  {
    echo "ERROR: TeamHarness uninstall failed for: ${failed[*]+"${failed[*]}"}."
    echo "Supported runtimes: ${SUPPORTED_RUNTIMES[*]}. Select explicitly: uninstall.sh --runtime <qwenpaw|openclaw|hermes>"
  } >&2
  exit 1
fi

LOG_FILE="${TEAMHARNESS_INSTALL_LOG:-}"
if [ -n "$LOG_FILE" ] && [ "$DRY_RUN" -eq 0 ]; then
  runtimes_json=""
  for runtime in "${SUPPORTED_RUNTIMES[@]}"; do
    if [ "$(get_status "$runtime")" = "uninstalled" ]; then
      if [ -n "$runtimes_json" ]; then
        runtimes_json="${runtimes_json},"
      fi
      runtimes_json="${runtimes_json}\"${runtime}\""
    fi
  done
  if [ -n "$runtimes_json" ]; then
    mkdir -p "$(dirname "$LOG_FILE")"
    printf '{"event":"uninstall","runtimes":[%s],"pluginDir":"%s"}\n' \
      "$runtimes_json" "$AGENTTEAMS_PLUGIN_DIR" >> "$LOG_FILE"
  fi
fi
