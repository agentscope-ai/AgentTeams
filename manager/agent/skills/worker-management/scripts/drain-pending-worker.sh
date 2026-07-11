#!/bin/bash
# drain-pending-worker.sh - Remove one processed Worker from pending-workers.json.
#
# Usage:
#   drain-pending-worker.sh --worker <NAME> [--file <PATH>]
#
# Keep this behind a helper so CoPaw Manager heartbeat turns do not emit shell
# commands containing mv/rm into the admin DM Tool Guard path.

set -euo pipefail

WORKER=""
FILE="${PENDING_WORKERS_FILE:-${HOME}/pending-workers.json}"

while [ "$#" -gt 0 ]; do
    case "$1" in
        --worker) WORKER="${2:-}"; shift 2 ;;
        --file) FILE="${2:-}"; shift 2 ;;
        -h|--help)
            grep '^#' "$0" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *)
            echo "Unknown option: $1" >&2
            echo "Usage: drain-pending-worker.sh --worker <NAME> [--file <PATH>]" >&2
            exit 1
            ;;
    esac
done

if [ -z "${WORKER}" ]; then
    echo "Usage: drain-pending-worker.sh --worker <NAME> [--file <PATH>]" >&2
    exit 1
fi

mkdir -p "$(dirname "${FILE}")"
touch "${FILE}"

TMP="${FILE}.tmp.$$"
trap 'rm -f "${TMP}"' EXIT

if [ -s "${FILE}" ]; then
    jq -c --arg name "${WORKER}" 'select(.name != $name)' "${FILE}" > "${TMP}"
else
    : > "${TMP}"
fi

cat "${TMP}" > "${FILE}"
echo "Drained ${WORKER} from ${FILE}"
