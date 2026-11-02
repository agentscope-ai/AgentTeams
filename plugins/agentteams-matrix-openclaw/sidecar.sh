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
#   sidecar.pid           '<pid> <starttime>' of the background process;
#                         every command re-verifies that identity before
#                         reporting "running" or sending a signal, so a
#                         stale or reused PID is never treated as the
#                         sidecar
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

# Field 22 of /proc/<pid>/stat (starttime, in clock ticks since boot),
# or nothing when it cannot be read. The "pid (comm)" prefix is stripped
# at the LAST ") " because comm may itself contain spaces or
# parentheses; starttime is then the 20th remaining field.
proc_starttime() {
  local pid="$1" stat rest fields=()
  stat="$(cat "/proc/$pid/stat" 2>/dev/null)" || return 0
  rest="${stat##*)}"
  read -r -a fields <<< "$rest"
  if [ "${#fields[@]}" -ge 20 ]; then
    printf '%s' "${fields[19]}"
  fi
  return 0
}

# Does /proc/<pid>/cmdline look like the sidecar's own command line?
# The NUL-separated token sequence must contain "-m" immediately
# followed by "sidecar" AND "--config" immediately followed by
# $CONFIG_FILE (or the one-token form "--config=$CONFIG_FILE"). When
# the config file exists, its resolved real path is accepted too.
# Comparison is literal.
cmdline_is_sidecar() {
  local pid="$1"
  local cmdline
  cmdline="$(tr '\0' '\n' < "/proc/$pid/cmdline" 2>/dev/null)" || return 1
  [ -n "$cmdline" ] || return 1
  local -a tokens=()
  mapfile -t tokens <<< "$cmdline"
  local alt_config=""
  if [ -f "$CONFIG_FILE" ] && command -v realpath >/dev/null 2>&1; then
    alt_config="$(realpath "$CONFIG_FILE" 2>/dev/null || true)"
  fi
  local i n found_module=0 found_config=0
  n=${#tokens[@]}
  for ((i = 0; i < n; i++)); do
    if [ "${tokens[i]}" = "-m" ] && [ $((i + 1)) -lt "$n" ] \
       && [ "${tokens[i + 1]}" = "sidecar" ]; then
      found_module=1
    fi
    if [ "${tokens[i]}" = "--config" ] && [ $((i + 1)) -lt "$n" ]; then
      if [ "${tokens[i + 1]}" = "$CONFIG_FILE" ] \
         || { [ -n "$alt_config" ] && [ "${tokens[i + 1]}" = "$alt_config" ]; }; then
        found_config=1
      fi
    elif [ "${tokens[i]}" = "--config=$CONFIG_FILE" ] \
         || { [ -n "$alt_config" ] && [ "${tokens[i]}" = "--config=$alt_config" ]; }; then
      found_config=1
    fi
  done
  [ "$found_module" -eq 1 ] && [ "$found_config" -eq 1 ]
}

# The pid token recorded in the pidfile, whatever it is (for operator
# messages); prints nothing when the file is absent or empty.
pidfile_pid() {
  local line
  line="$(head -n 1 -- "$PID_FILE" 2>/dev/null || true)"
  printf '%s' "${line%%[[:space:]]*}"
}

# Prints the pid when the pidfile names the live sidecar process
# itself; prints nothing (and exits 0) otherwise. All of these must
# hold:
#   1. the recorded pid is a number > 1 and not this script's own pid;
#   2. the process is alive (kill -0);
#   3. start identity: when the pidfile records a starttime (anything
#      but "-"), it must match field 22 of /proc/<pid>/stat verbatim —
#      the kernel assigns a fresh starttime to every new process, so a
#      PID handed to an unrelated process fails this check. Old-format
#      pidfiles (a bare pid, or "-") skip it but must pass (4);
#   4. the command line is the sidecar's own: "python3 -m sidecar
#      --config <CONFIG_FILE>" (or --config=<CONFIG_FILE>).
# A stale or reused PID therefore never reports "running" and never
# receives a signal from this script.
verified_sidecar_pid() {
  [ -f "$PID_FILE" ] || return 0
  local line pid recorded fields=()
  line="$(head -n 1 -- "$PID_FILE" 2>/dev/null || true)"
  read -r -a fields <<< "$line"
  [ "${#fields[@]}" -ge 1 ] || return 0
  pid="${fields[0]}"
  recorded="${fields[1]:-}"
  case "$pid" in
    '' | *[!0-9]*) return 0 ;;
  esac
  [ "$pid" -gt 1 ] 2>/dev/null || return 0
  [ "$pid" != "$$" ] || return 0
  kill -0 "$pid" 2>/dev/null || return 0
  if [ -n "$recorded" ] && [ "$recorded" != "-" ]; then
    local current
    current="$(proc_starttime "$pid")"
    [ -n "$current" ] || return 0
    [ "$current" = "$recorded" ] || return 0
  fi
  cmdline_is_sidecar "$pid" || return 0
  echo "$pid"
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
  pid="$(verified_sidecar_pid)"
  if [ -n "$pid" ]; then
    echo "sidecar already running (pid $pid)"
    return 0
  fi
  if [ -f "$PID_FILE" ]; then
    local stale
    stale="$(pidfile_pid)"
    rm -f "$PID_FILE"
    echo "ignoring stale pid file (pid ${stale:--} is not this sidecar)"
  fi
  if [ ! -d "$STATE_DIR" ]; then
    mkdir -p "$STATE_DIR"
    chmod 700 "$STATE_DIR"
  fi
  PYTHONPATH="$PLUGIN_ROOT" nohup python3 -m sidecar --config "$CONFIG_FILE" >> "$LOG_FILE" 2>&1 &
  local new_pid=$!
  # Record the process's start identity (field 22 of /proc/<pid>/stat)
  # alongside the pid so a later PID reuse can be detected; "-" when the
  # kernel does not tell us (the command-line check still applies).
  local start_id
  start_id="$(proc_starttime "$new_pid")"
  printf '%s %s\n' "$new_pid" "${start_id:--}" > "$PID_FILE"
  # Give it a moment to fail fast (config error, locked state dir, ...).
  sleep 1
  if [ "$(verified_sidecar_pid)" != "$new_pid" ]; then
    rm -f "$PID_FILE"
    echo "sidecar exited during startup; last log lines:" >&2
    tail -n 20 "$LOG_FILE" >&2
    exit 1
  fi
  echo "sidecar started (pid $new_pid, log: $LOG_FILE)"
}

cmd_stop() {
  local pid
  pid="$(verified_sidecar_pid)"
  if [ -z "$pid" ]; then
    if [ -f "$PID_FILE" ]; then
      rm -f "$PID_FILE"
      echo "sidecar not running (stale pid file ignored)"
    else
      echo "sidecar not running"
    fi
    return 0
  fi
  echo "stopping sidecar (pid $pid, SIGTERM)"
  kill -TERM "$pid" 2>/dev/null || true
  # Re-verify the start identity on every poll, not a bare kill -0: once
  # the sidecar dies the kernel may hand its PID to an unrelated process,
  # which a liveness-only check would keep waiting on (and later KILL).
  local waited=0
  while [ "$(verified_sidecar_pid)" = "$pid" ] && [ "$waited" -lt "$STOP_TIMEOUT" ]; do
    sleep 1
    waited=$((waited + 1))
  done
  if [ "$(verified_sidecar_pid)" = "$pid" ]; then
    echo "sidecar did not stop within ${STOP_TIMEOUT}s; sending SIGKILL" >&2
    kill -KILL "$pid" 2>/dev/null || true
  fi
  rm -f "$PID_FILE"
  echo "sidecar stopped"
}

cmd_status() {
  local pid sock stale
  pid="$(verified_sidecar_pid)"
  sock="$(socket_path_from_config)"
  if [ -n "$pid" ]; then
    echo "sidecar running (pid $pid)"
  elif [ -f "$PID_FILE" ]; then
    stale="$(pidfile_pid)"
    echo "sidecar not running (stale pid file names pid ${stale:--}, which is not this sidecar)"
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
