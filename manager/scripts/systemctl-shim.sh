#!/bin/bash
# systemctl shim for openclaw gateway restart support in Docker (no systemd available).
# Intercepts openclaw-gateway service operations and uses SIGUSR1 for in-process restart.
# All other calls are delegated to the real systemctl binary if present.

# Parse subcommand and first unit (non-flag) argument from "$@"
SUBCOMMAND=""
UNIT=""
for arg in "$@"; do
    case "$arg" in
        --*|-*) ;;
        *)
            if [[ -z "$SUBCOMMAND" ]]; then
                SUBCOMMAND="$arg"
            elif [[ -z "$UNIT" ]]; then
                UNIT="$arg"
            fi
            ;;
    esac
done

# Find a running gateway PID via the lock file directory
find_gateway_pid() {
    local lock_dir="/tmp/openclaw-$(id -u)"
    local f pid
    for f in "$lock_dir"/gateway.*.lock; do
        [[ -f "$f" ]] || continue
        pid=$(jq -r '.pid // empty' "$f" 2>/dev/null)
        [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null && echo "$pid" && return 0
    done
    return 1
}

# Handle openclaw-gateway service operations, or bare 'status' (for assertSystemdAvailable)
if [[ "$UNIT" == openclaw-gateway* || ( "$SUBCOMMAND" == "status" && -z "$UNIT" ) ]]; then
    case "$SUBCOMMAND" in
        status)
            if [[ "$UNIT" == openclaw-gateway* ]]; then
                if find_gateway_pid > /dev/null 2>&1; then
                    exit 0
                fi
                echo "openclaw-gateway: not running" >&2
                exit 1
            fi
            exit 0
            ;;
        daemon-reload|enable|disable)
            exit 0
            ;;
        is-enabled)
            if find_gateway_pid > /dev/null 2>&1; then
                echo "enabled"
                exit 0
            fi
            echo "disabled"
            exit 1
            ;;
        restart)
            pid=$(find_gateway_pid)
            if [[ -z "$pid" ]]; then
                echo "systemctl: openclaw gateway is not running" >&2
                exit 1
            fi
            kill -USR1 "$pid"
            echo "Restarted openclaw gateway (pid $pid via SIGUSR1)"
            exit 0
            ;;
        stop)
            pid=$(find_gateway_pid)
            if [[ -n "$pid" ]]; then
                kill -TERM "$pid"
                echo "Stopped openclaw gateway (pid $pid)"
            fi
            exit 0
            ;;
    esac
fi

# Delegate everything else to the real systemctl
for real in /bin/systemctl /usr/bin/systemctl /usr/sbin/systemctl; do
    [[ -x "$real" ]] && exec "$real" "$@"
done
echo "systemctl: not available in this container" >&2
exit 1
