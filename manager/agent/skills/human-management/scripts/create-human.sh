#!/bin/bash
# create-human.sh - Create a Human CR through the Controller API.

set -euo pipefail

MATRIX_ID=""
DISPLAY_NAME=""
LEVEL=""
TEAMS_CSV=""
WORKERS_CSV=""
EMAIL=""
NOTE=""

while [ $# -gt 0 ]; do
    case "$1" in
        --matrix-id) MATRIX_ID="$2"; shift 2 ;;
        --name) DISPLAY_NAME="$2"; shift 2 ;;
        --level) LEVEL="$2"; shift 2 ;;
        --teams) TEAMS_CSV="$2"; shift 2 ;;
        --workers) WORKERS_CSV="$2"; shift 2 ;;
        --email) EMAIL="$2"; shift 2 ;;
        --note) NOTE="$2"; shift 2 ;;
        *) echo "Unknown option: $1" >&2; exit 2 ;;
    esac
done

if [ -z "${MATRIX_ID}" ] || [ -z "${DISPLAY_NAME}" ] || [ -z "${LEVEL}" ]; then
    echo "Usage: $0 --matrix-id @user:domain --name DISPLAY_NAME --level 1|2|3 [--teams t1,t2] [--workers w1,w2]" >&2
    exit 2
fi

username=$(echo "${MATRIX_ID}" | sed 's/^@//' | cut -d: -f1)
args=(create human --name "${username}" --display-name "${DISPLAY_NAME}" --permission-level "${LEVEL}")
[ -z "${TEAMS_CSV}" ] || args+=(--accessible-teams "${TEAMS_CSV}")
[ -z "${WORKERS_CSV}" ] || args+=(--accessible-workers "${WORKERS_CSV}")
[ -z "${EMAIL}" ] || args+=(--email "${EMAIL}")
[ -z "${NOTE}" ] || args+=(--note "${NOTE}")

agt "${args[@]}"
agt get humans "${username}" -o json
