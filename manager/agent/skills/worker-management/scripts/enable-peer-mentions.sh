#!/bin/bash
# enable-peer-mentions.sh - Enable peer mentions on the Team containing Workers.

set -euo pipefail

WORKERS_CSV=""
while [ $# -gt 0 ]; do
    case "$1" in
        --workers) WORKERS_CSV="$2"; shift 2 ;;
        *) echo "Unknown option: $1" >&2; exit 2 ;;
    esac
done
[ -n "${WORKERS_CSV}" ] || { echo "Usage: $0 --workers w1,w2" >&2; exit 2; }

team=""
IFS=',' read -ra workers <<< "${WORKERS_CSV}"
for worker in "${workers[@]}"; do
    [ -n "${worker}" ] || continue
    worker_team=$(agt get workers "${worker}" -o json | jq -r '.team // empty')
    [ -n "${worker_team}" ] || { echo "Worker ${worker} is not a Team member" >&2; exit 1; }
    if [ -z "${team}" ]; then
        team="${worker_team}"
    elif [ "${team}" != "${worker_team}" ]; then
        echo "Workers must belong to the same Team" >&2
        exit 1
    fi
done

agt update team --name "${team}" --peer-mentions=true
agt get teams "${team}" -o json
