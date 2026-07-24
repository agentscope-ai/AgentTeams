#!/bin/bash
# create-team.sh - Create a Team CR that references existing Worker CRs.

set -euo pipefail

TEAM_NAME=""
LEADER_NAME=""
WORKERS_CSV=""
TEAM_ADMIN=""
TEAM_ADMIN_MATRIX_ID=""
PEER_MENTIONS=true
HEARTBEAT_EVERY=""
DESCRIPTION=""

while [ $# -gt 0 ]; do
    case "$1" in
        --name) TEAM_NAME="$2"; shift 2 ;;
        --leader) LEADER_NAME="$2"; shift 2 ;;
        --workers) WORKERS_CSV="$2"; shift 2 ;;
        --team-admin) TEAM_ADMIN="$2"; shift 2 ;;
        --team-admin-matrix-id) TEAM_ADMIN_MATRIX_ID="$2"; shift 2 ;;
        --peer-mentions) PEER_MENTIONS="$2"; shift 2 ;;
        --heartbeat-every) HEARTBEAT_EVERY="$2"; shift 2 ;;
        --description) DESCRIPTION="$2"; shift 2 ;;
        *) echo "Unknown option: $1" >&2; exit 2 ;;
    esac
done

if [ -z "${TEAM_NAME}" ] || [ -z "${LEADER_NAME}" ]; then
    echo "Usage: $0 --name TEAM --leader WORKER [--workers w1,w2] [--team-admin HUMAN] [--peer-mentions true|false]" >&2
    exit 2
fi

agt get workers "${LEADER_NAME}" -o json >/dev/null
IFS=',' read -ra workers <<< "${WORKERS_CSV}"
for worker in "${workers[@]}"; do
    [ -n "${worker}" ] || continue
    agt get workers "${worker}" -o json >/dev/null
done
[ -z "${TEAM_ADMIN}" ] || agt get humans "${TEAM_ADMIN}" -o json >/dev/null

args=(create team --name "${TEAM_NAME}" --leader-name "${LEADER_NAME}" --peer-mentions="${PEER_MENTIONS}")
[ -z "${WORKERS_CSV}" ] || args+=(--workers "${WORKERS_CSV}")
[ -z "${TEAM_ADMIN}" ] || args+=(--admin "${TEAM_ADMIN}")
[ -z "${TEAM_ADMIN_MATRIX_ID}" ] || args+=(--admin-matrix-id "${TEAM_ADMIN_MATRIX_ID}")
[ -z "${HEARTBEAT_EVERY}" ] || args+=(--leader-heartbeat-every "${HEARTBEAT_EVERY}")
[ -z "${DESCRIPTION}" ] || args+=(--description "${DESCRIPTION}")

agt "${args[@]}"
agt get teams "${TEAM_NAME}" -o json
