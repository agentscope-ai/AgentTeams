#!/bin/bash
# send-task-message.sh - Dispatch a task-related Matrix message to a Worker room
#
# Hides runtime branching so the Manager never looks up runtime via hiclaw API.
# Mirrors send-worker-greeting.sh behavior.
#
# Usage:
#   send-task-message.sh --room <ROOM_ID> --worker <NAME> --text <BODY>
#
# Runtime behavior (from $AGENTTEAMS_MANAGER_RUNTIME):
#   copaw:    runs `copaw channels send` with correct flags and mention format
#   openclaw: prints target + body and exits 2 for native message tool delivery

set -euo pipefail

ROOM=""
WORKER=""
TEXT=""

while [ $# -gt 0 ]; do
    case "$1" in
        --room)   ROOM="$2"; shift 2 ;;
        --worker) WORKER="$2"; shift 2 ;;
        --text)   TEXT="$2"; shift 2 ;;
        -h|--help)
            grep '^#' "$0" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *)
            echo "Unknown option: $1" >&2
            exit 1
            ;;
    esac
done

if [ -z "${ROOM}" ] || [ -z "${WORKER}" ] || [ -z "${TEXT}" ]; then
    echo "Usage: send-task-message.sh --room <ROOM_ID> --worker <NAME> --text <BODY>" >&2
    exit 1
fi

DOMAIN="${AGENTTEAMS_MATRIX_DOMAIN:-matrix-local.agentteams.io:18080}"
RUNTIME="${AGENTTEAMS_MANAGER_RUNTIME:-openclaw}"
MENTION="@${WORKER}:${DOMAIN}"

case "${RUNTIME}" in
    copaw)
        exec copaw channels send \
            --agent-id default \
            --channel matrix \
            --target-user "${MENTION}" \
            --target-session "${ROOM}" \
            --text "${TEXT}"
        ;;
    openclaw|*)
        cat <<EOF
OpenClaw Manager runtime detected (AGENTTEAMS_MANAGER_RUNTIME="${RUNTIME}").
Deliver this task message via your native message channel:

  Target room:  ${ROOM}
  Target user:  ${MENTION}
  Message text: ${TEXT}

Exit code 2 is expected in this branch.
EOF
        exit 2
        ;;
esac
