#!/usr/bin/env bash
set -euo pipefail

PLUGIN_NAME="teamharness"
PACKAGE_PATH="${TEAMHARNESS_PLUGIN_PACKAGE:-}"
PROJECT_DIR="${AGENTTEAMS_PROJECT_DIR:-${AGENTTEAMS_WORKER_HOME:-$(pwd)}}"
API_BASE="${QWENPAW_API_BASE:-${COPAW_API_BASE:-http://127.0.0.1:${AGENTTEAMS_CONSOLE_PORT:-8088}}}"
RELOAD_PATH="${TEAMHARNESS_RELOAD_PATH:-/api/teamharness/sync}"
INSTALLER="auto"
SKIP_RELOAD=0
CLEANUP_DIR=""

cleanup() {
  [ -z "$CLEANUP_DIR" ] || rm -rf "$CLEANUP_DIR"
}
trap cleanup EXIT

usage() {
  cat <<'EOF'
Usage:
  qwenpaw-teamharness-plugin-reload.sh [--package PATH] [options]

Installs or updates the TeamHarness plugin for a running QwenPaw worker, then
triggers the TeamHarness sync endpoint so prompts, skills, and MCP config are
re-applied to the active agent workspace.

Options:
  --package PATH       Plugin package or directory. Defaults to
                       $TEAMHARNESS_PLUGIN_PACKAGE, /opt/agentteams/plugins/teamharness.tar.gz,
                       then /opt/agentteams/plugins/teamharness-qwenpaw.zip.
  --project-dir DIR    agentteams state directory parent. Defaults to
                       $AGENTTEAMS_PROJECT_DIR, $AGENTTEAMS_WORKER_HOME, or cwd.
  --api-base URL       QwenPaw API base. Defaults to $QWENPAW_API_BASE or
                       http://127.0.0.1:$AGENTTEAMS_CONSOLE_PORT.
  --reload-path PATH   Reload endpoint path. Defaults to /api/teamharness/sync.
  --use-agentteams     Force agentteams plugin install/update.
  --use-qwenpaw        Force qwenpaw plugin install --force.
  --skip-reload        Install only.
  -h, --help           Show this help.
EOF
}

log() {
  printf '[teamharness-plugin-reload] %s\n' "$*" >&2
}

fail() {
  log "ERROR: $*"
  exit 1
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --package)
      [ "$#" -ge 2 ] || fail "--package requires a value"
      PACKAGE_PATH="$2"
      shift 2
      ;;
    --project-dir)
      [ "$#" -ge 2 ] || fail "--project-dir requires a value"
      PROJECT_DIR="$2"
      shift 2
      ;;
    --api-base)
      [ "$#" -ge 2 ] || fail "--api-base requires a value"
      API_BASE="$2"
      shift 2
      ;;
    --reload-path)
      [ "$#" -ge 2 ] || fail "--reload-path requires a value"
      RELOAD_PATH="$2"
      shift 2
      ;;
    --use-agentteams)
      INSTALLER="agentteams"
      shift
      ;;
    --use-qwenpaw)
      INSTALLER="qwenpaw"
      shift
      ;;
    --skip-reload)
      SKIP_RELOAD=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      fail "unknown argument: $1"
      ;;
  esac
done

if [ -z "$PACKAGE_PATH" ]; then
  if [ -e /opt/agentteams/plugins/teamharness.tar.gz ]; then
    PACKAGE_PATH="/opt/agentteams/plugins/teamharness.tar.gz"
  elif [ -e /opt/agentteams/plugins/teamharness-qwenpaw.zip ]; then
    PACKAGE_PATH="/opt/agentteams/plugins/teamharness-qwenpaw.zip"
  fi
fi

[ -n "$PACKAGE_PATH" ] || fail "plugin package is required"
[ -e "$PACKAGE_PATH" ] || fail "plugin package not found: $PACKAGE_PATH"
PACKAGE_PATH="$(python3 - "$PACKAGE_PATH" <<'PY'
from pathlib import Path
import sys

print(Path(sys.argv[1]).expanduser().resolve())
PY
)"

is_qwenpaw_native_package() {
  local package="$1"
  if [ -d "$package" ]; then
    [ -f "$package/plugin.json" ]
    return
  fi
  case "$package" in
    *.zip) return 0 ;;
    *) return 1 ;;
  esac
}

extract_qwenpaw_package() {
  local zip_path="$1"
  local target_dir="$2"
  python3 - "$zip_path" "$target_dir" <<'PY'
from pathlib import Path
import sys
import zipfile

zip_path = Path(sys.argv[1])
target_dir = Path(sys.argv[2])
target_root = target_dir.resolve()

with zipfile.ZipFile(zip_path) as archive:
    for name in archive.namelist():
        resolved = (target_dir / name).resolve()
        try:
            resolved.relative_to(target_root)
        except ValueError:
            raise SystemExit(f"unsafe qwenpaw plugin package path: {name}")
    archive.extractall(target_dir)

packages = [
    path for path in target_dir.iterdir()
    if path.is_dir() and (path / "plugin.json").is_file()
]
if len(packages) != 1:
    raise SystemExit(f"expected one qwenpaw plugin package in {zip_path}")
print(packages[0])
PY
}

install_with_qwenpaw() {
  command -v qwenpaw >/dev/null 2>&1 || fail "qwenpaw command not found"

  local package="$1"
  local package_dir="$package"
  local tmp_dir=""

  if [ -f "$package" ]; then
    tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/teamharness-qwenpaw-reload.XXXXXX")"
    CLEANUP_DIR="$tmp_dir"
    package_dir="$(extract_qwenpaw_package "$package" "$tmp_dir")"
  fi

  log "installing QwenPaw plugin package: $package_dir"
  qwenpaw plugin install "$package_dir" --force
}

install_with_agentteams() {
  command -v agentteams >/dev/null 2>&1 || fail "agentteams command not found"
  mkdir -p "$PROJECT_DIR"

  local action="install"
  if [ -f "$PROJECT_DIR/.agentteams/plugins/$PLUGIN_NAME/manifest.json" ]; then
    action="update"
  fi

  log "running agentteams plugin $action in $PROJECT_DIR"
  (
    cd "$PROJECT_DIR"
    agentteams plugin "$action" "$PLUGIN_NAME" --package "$PACKAGE_PATH"
  )
}

install_plugin() {
  case "$INSTALLER" in
    qwenpaw)
      install_with_qwenpaw "$PACKAGE_PATH"
      ;;
    agentteams)
      install_with_agentteams
      ;;
    auto)
      if is_qwenpaw_native_package "$PACKAGE_PATH"; then
        install_with_qwenpaw "$PACKAGE_PATH"
      elif command -v agentteams >/dev/null 2>&1; then
        install_with_agentteams
      else
        fail "agentteams is unavailable and package is not a QwenPaw-native plugin: $PACKAGE_PATH"
      fi
      ;;
    *)
      fail "unknown installer: $INSTALLER"
      ;;
  esac
}

post_reload() {
  local url="${API_BASE%/}/${RELOAD_PATH#/}"
  log "triggering agent reload: $url"
  if command -v curl >/dev/null 2>&1; then
    curl -fsS -X POST "$url" >/dev/null
    return
  fi
  python3 - "$url" <<'PY'
import sys
import urllib.request

request = urllib.request.Request(sys.argv[1], data=b"", method="POST")
with urllib.request.urlopen(request, timeout=30) as response:
    response.read()
PY
}

install_plugin

if [ "$SKIP_RELOAD" -eq 0 ]; then
  post_reload
fi

log "done"
