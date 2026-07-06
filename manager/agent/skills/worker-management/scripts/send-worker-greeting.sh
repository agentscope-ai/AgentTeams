#!/bin/bash
# send-worker-greeting.sh - Send the post-creation greeting in a Worker's Matrix Room
#
# Hides all shell-escape / flag-name / mention-format details behind a single
# command so the Manager never burns a turn fighting bash quoting.
#
# Usage:
#   send-worker-greeting.sh --worker <NAME> --room <ROOM_ID> [--text <CUSTOM_TEXT>]
#
# Runtime behavior (auto-detected from $AGENTTEAMS_MANAGER_RUNTIME):
#   - copaw:    delegates to `copaw channels send` with the correct flags
#               (--text, --target-user, --target-session) and the proper
#               "@<name>:${AGENTTEAMS_MATRIX_DOMAIN}" mention format.
#   - openclaw: prints the greeting text + target room and exits 2, so the
#               Manager can deliver it via its native message channel.
#
# Output (success, CoPaw): passthrough from `copaw channels send`.
# Output (OpenClaw): instructional text on stdout, non-zero exit to make the
#                    branch decision obvious to the caller.

set -euo pipefail

WORKER=""
ROOM=""
CUSTOM_TEXT=""

while [ $# -gt 0 ]; do
    case "$1" in
        --worker) WORKER="$2"; shift 2 ;;
        --room)   ROOM="$2"; shift 2 ;;
        --text)   CUSTOM_TEXT="$2"; shift 2 ;;
        -h|--help)
            grep '^#' "$0" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *)
            echo "Unknown option: $1" >&2
            echo "Usage: send-worker-greeting.sh --worker <NAME> --room <ROOM_ID> [--text <CUSTOM_TEXT>]" >&2
            exit 1
            ;;
    esac
done

if [ -z "${WORKER}" ] || [ -z "${ROOM}" ]; then
    echo "Usage: send-worker-greeting.sh --worker <NAME> --room <ROOM_ID> [--text <CUSTOM_TEXT>]" >&2
    exit 1
fi

DOMAIN="${AGENTTEAMS_MATRIX_DOMAIN:-matrix-local.agentteams.io:18080}"
RUNTIME="${AGENTTEAMS_MANAGER_RUNTIME:-openclaw}"

MENTION="@${WORKER}:${DOMAIN}"
if [ -n "${CUSTOM_TEXT}" ]; then
    TEXT="${CUSTOM_TEXT}"
else
    TEXT="${MENTION} You're all set! Please introduce yourself to everyone in this room."
fi

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
This helper only runs the shell flow for CoPaw. For OpenClaw, send the
greeting via your native message channel:

  Target room:  ${ROOM}
  Target user:  ${MENTION}
  Message text: ${TEXT}

Exit code 2 is expected in this branch.
EOF
        exit 2
        ;;
esac
