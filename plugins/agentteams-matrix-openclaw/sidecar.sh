#!/usr/bin/env bash
# Manage the AgentTeams Matrix sidecar (production entry) as a process.
#
#   sidecar.sh start     start in the background (idempotent)
#   sidecar.sh stop      SIGTERM, wait up to 10s, then SIGKILL
#   sidecar.sh restart   stop + start
#   sidecar.sh status    report pid + socket presence (exit 0 = running)
#   sidecar.sh run       foreground (for systemd: ExecStart=.../sidecar.sh run)
#
# State (default $HOME/.agentteams/openclaw-matrix, the installer's
# state directory; override with AGENTTEAMS_OPENCLAW_STATE_DIR):
#   sidecar-config.json   credentials + paths (written by install.sh, 0600)
#   sidecar.pid           pid of the background process
#   sidecar.log           combined stdout/stderr
#
# The sidecar needs MATRIX_CORE_DIR to point at the agentteams-matrix
# core checkout (the directory that contains core/).

set -euo pipefail

PLUGIN_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
STATE_DIR="${AGENTTEAMS_OPENCLAW_STATE_DIR:-$HOME/.agentteams/openclaw-matrix}"
CONFIG_FILE="${AGENTTEAMS_MATRIX_SIDECAR_CONFIG:-$STATE_DIR/sidecar-config.json}"
PID_FILE="$STATE_DIR/sidecar.pid"
LOG_FILE="$STATE_DIR/sidecar.log"
STOP_TIMEOUT=10

die() { echo "sidecar.sh: $*" >&2; exit 1; }

usage() {
  cat <<'EOF'
Usage: sidecar.sh {start|stop|restart|status|run}
  start     start in the background (idempotent)
  stop      SIGTERM, wait up to 10s, then SIGKILL
  restart   stop + start
  status    report pid + socket presence (exit 0 = running)
  run       foreground (for systemd: ExecStart=.../sidecar.sh run)

State directory: $AGENTTEAMS_OPENCLAW_STATE_DIR (default
~/.agentteams/openclaw-matrix). Config: $AGENTTEAMS_MATRIX_SIDECAR_CONFIG
(default <state dir>/sidecar-config.json). The sidecar needs
MATRIX_CORE_DIR to point at the agentteams-matrix core checkout.
EOF
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required but not found in PATH"
}

require_config() {
  [ -f "$CONFIG_FILE" ] || die "sidecar config not found at $CONFIG_FILE — run install.sh with AGENTTEAMS_MATRIX_URL/USER/TOKEN set, or write it by hand (see README.md, 'Running the sidecar')"
}

require_core_dir() {
  [ -n "${MATRIX_CORE_DIR:-}" ] || die "MATRIX_CORE_DIR is not set — point it at the agentteams-matrix core checkout (the directory that contains core/)"
}

# Prints the pid when the pidfile names a live process; prints nothing
# (and exits 0) otherwise. A stale pidfile is simply not "running".
read_live_pid() {
  [ -f "$PID_FILE" ] || return 0
  local pid
  pid="$(cat "$PID_FILE" 2>/dev/null || true)"
  case "$pid" in
    '' | *[!0-9]*) return 0 ;;
  esac
  if kill -0 "$pid" 2>/dev/null; then
    echo "$pid"
  fi
  return 0
}

# The socket the sidecar will listen on: from the config file when it
# can be read (jq is a hard dependency of the lifecycle scripts), else
# the documented default.
socket_path_from_config() {
  if [ -f "$CONFIG_FILE" ] && command -v jq >/dev/null 2>&1; then
    local path
    path="$(jq -r '.socket_path // empty' "$CONFIG_FILE" 2>/dev/null || true)"
    if [ -n "$path" ]; then
      echo "$path"
      return 0
    fi
  fi
  echo "${AGENTTEAMS_MATRIX_STATE_DIR:-$HOME/.agentteams/matrix}/sidecar.sock"
}

cmd_run() {
  require_cmd python3
  require_config
  require_core_dir
  # exec: SIGTERM from systemd reaches the python process directly.
  exec env PYTHONPATH="$PLUGIN_ROOT" python3 -m sidecar --config "$CONFIG_FILE"
}

cmd_start() {
  require_cmd python3
  require_config
  require_core_dir
  local pid
  pid="$(read_live_pid)"
  if [ -n "$pid" ]; then
    echo "sidecar already running (pid $pid)"
    return 0
  fi
  if [ ! -d "$STATE_DIR" ]; then
    mkdir -p "$STATE_DIR"
    chmod 700 "$STATE_DIR"
  fi
  PYTHONPATH="$PLUGIN_ROOT" nohup python3 -m sidecar --config "$CONFIG_FILE" >> "$LOG_FILE" 2>&1 &
  local new_pid=$!
  echo "$new_pid" > "$PID_FILE"
  # Give it a moment to fail fast (config error, locked state dir, ...).
  sleep 1
  if ! kill -0 "$new_pid" 2>/dev/null; then
    rm -f "$PID_FILE"
    echo "sidecar exited during startup; last log lines:" >&2
    tail -n 20 "$LOG_FILE" >&2
    exit 1
  fi
  echo "sidecar started (pid $new_pid, log: $LOG_FILE)"
}

cmd_stop() {
  local pid
  pid="$(read_live_pid)"
  if [ -z "$pid" ]; then
    rm -f "$PID_FILE" 2>/dev/null || true
    echo "sidecar not running"
    return 0
  fi
  echo "stopping sidecar (pid $pid, SIGTERM)"
  kill -TERM "$pid" 2>/dev/null || true
  local waited=0
  while kill -0 "$pid" 2>/dev/null && [ "$waited" -lt "$STOP_TIMEOUT" ]; do
    sleep 1
    waited=$((waited + 1))
  done
  if kill -0 "$pid" 2>/dev/null; then
    echo "sidecar did not stop within ${STOP_TIMEOUT}s; sending SIGKILL" >&2
    kill -KILL "$pid" 2>/dev/null || true
  fi
  rm -f "$PID_FILE"
  echo "sidecar stopped"
}

cmd_status() {
  local pid sock
  pid="$(read_live_pid)"
  sock="$(socket_path_from_config)"
  if [ -n "$pid" ]; then
    echo "sidecar running (pid $pid)"
  else
    echo "sidecar not running"
  fi
  if [ -S "$sock" ]; then
    echo "socket present: $sock"
  else
    echo "socket absent: $sock"
  fi
  if [ -n "$pid" ]; then
    return 0
  fi
  return 1
}

case "${1:-}" in
  start) cmd_start ;;
  stop) cmd_stop ;;
  restart) cmd_stop; cmd_start ;;
  status) cmd_status ;;
  run) cmd_run ;;
  -h | --help | help) usage ;;
  *) usage >&2; exit 2 ;;
esac
