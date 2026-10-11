#!/bin/bash
# enable-worker-console.sh - Console management (retired with the CoPaw runtime)
#
# This script used to toggle the CoPaw web console by recreating the worker
# container with/without AGENTTEAMS_CONSOLE_PORT. It was CoPaw-worker only
# (the container image had to be a CoPaw image and the recreate body used
# runtime "copaw").
#
# The CoPaw runtime has been removed from AgentTeams (EOL; replaced by the
# QwenPaw runtime), so on-demand console toggling is no longer supported.
# This entry point is kept so callers (skill docs, admin requests) get a
# clear, actionable failure instead of a dangling script.
#
# Usage:
#   enable-worker-console.sh --name <worker> [--action enable|disable] [--port <PORT>]

set -uo pipefail

WORKER_NAME=""
ACTION="enable"
CONSOLE_PORT="8088"

while [ $# -gt 0 ]; do
    case "$1" in
        --name)   WORKER_NAME="$2"; shift 2 ;;
        --action) ACTION="$2"; shift 2 ;;
        --port)   CONSOLE_PORT="$2"; shift 2 ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

if [ -z "${WORKER_NAME}" ]; then
    echo "Usage: enable-worker-console.sh --name <NAME> [--action enable|disable] [--port <PORT>]"
    exit 1
fi

echo "[enable-console $(date '+%Y-%m-%d %H:%M:%S')] ERROR: console toggling was CoPaw-worker only and was retired with the CoPaw runtime (worker: ${WORKER_NAME})." >&2
jq -n --arg name "${WORKER_NAME}" '{
    "error": "console_unsupported",
    "message": ("On-demand console toggling is no longer supported: it was CoPaw-worker only and the CoPaw runtime has been removed (worker " + $name + "). Use the QwenPaw runtime; console management for QwenPaw workers is not yet automated in this script.")
}'
exit 1
