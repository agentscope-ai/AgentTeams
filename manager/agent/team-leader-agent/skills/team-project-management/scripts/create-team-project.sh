#!/bin/bash
# create-team-project.sh - Create a team project directory structure
#
# Usage:
#   create-team-project.sh --id <PROJECT_ID> --title <TITLE> --workers <w1,w2,...> \
#     [--source manager|team-admin] [--parent-task-id <TASK_ID>] [--requester <@user:domain>]
#
# Prerequisites:
#   - Team storage must be initialized (teams/{team-name}/ exists in MinIO)
#   - TEAM_NAME environment variable or ~/SOUL.md must identify the team

set -euo pipefail
source /opt/hiclaw/scripts/lib/hiclaw-env.sh

# Ensure mc can find its config even when HOME is overridden
export MC_CONFIG_DIR="${MC_CONFIG_DIR:-/root/manager-workspace/.mc}"

PROJECT_ID=""
PROJECT_TITLE=""
WORKERS_CSV=""
SOURCE=""
PARENT_TASK_ID=""
REQUESTER=""

while [ $# -gt 0 ]; do
    case "$1" in
        --id)              PROJECT_ID="$2"; shift 2 ;;
        --title)           PROJECT_TITLE="$2"; shift 2 ;;
        --workers)         WORKERS_CSV="$2"; shift 2 ;;
        --source)          SOURCE="$2"; shift 2 ;;
        --parent-task-id)  PARENT_TASK_ID="$2"; shift 2 ;;
        --requester)       REQUESTER="$2"; shift 2 ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

if [ -z "${PROJECT_ID}" ] || [ -z "${PROJECT_TITLE}" ] || [ -z "${WORKERS_CSV}" ]; then
    echo "Usage: create-team-project.sh --id <ID> --title <TITLE> --workers <w1,w2,...> [--source S] [--parent-task-id T] [--requester R]"
    exit 1
fi

# Resolve team name from SOUL.md or environment
if [ -z "${TEAM_NAME:-}" ]; then
    for _soul in "./SOUL.md" "../SOUL.md" "${HOME}/SOUL.md"; do
        if [ -f "${_soul}" ]; then
            TEAM_NAME=$(grep -oP 'Team: \K\S+' "${_soul}" 2>/dev/null || grep -oP 'Team Leader of \K[^\s]+' "${_soul}" 2>/dev/null || true)
            [ -n "${TEAM_NAME}" ] && break
        fi
    done
fi
if [ -z "${TEAM_NAME:-}" ]; then
    echo "ERROR: Cannot determine TEAM_NAME. Set it via environment or ensure SOUL.md contains team info." >&2
    exit 1
fi

MATRIX_DOMAIN="${HICLAW_MATRIX_DOMAIN:-matrix-local.hiclaw.io:8080}"
NOW=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

# ============================================================
# Step 1: Create project directories and files
# ============================================================
PROJECT_DIR="/root/hiclaw-fs/shared/projects/${PROJECT_ID}"
mkdir -p "${PROJECT_DIR}"

WORKERS_JSON="[$(echo "${WORKERS_CSV}" | tr ',' '\n' | sed 's/.*/"&"/' | tr '\n' ',' | sed 's/,$//')]"

# Build meta.json
META=$(jq -n \
    --arg id "${PROJECT_ID}" \
    --arg title "${PROJECT_TITLE}" \
    --arg status "planning" \
    --arg team "${TEAM_NAME}" \
    --arg source "${SOURCE:-}" \
    --arg parent "${PARENT_TASK_ID:-}" \
    --arg requester "${REQUESTER:-}" \
    --argjson workers "${WORKERS_JSON}" \
    --arg now "${NOW}" \
    '{
        project_id: $id,
        title: $title,
        status: $status,
        team: $team,
        workers: $workers,
        created_at: $now,
        confirmed_at: null
    } + (if $source != "" then {source: $source} else {} end)
      + (if $parent != "" then {parent_task_id: $parent} else {} end)
      + (if $requester != "" then {requester: $requester} else {} end)')

echo "${META}" > "${PROJECT_DIR}/meta.json"

# Build plan.md template
PARENT_LINE=""
if [ -n "${PARENT_TASK_ID}" ]; then
    PARENT_LINE="**Parent Task**: ${PARENT_TASK_ID}"
else
    PARENT_LINE="**Parent Task**: (none — Team Admin initiated)"
fi

cat > "${PROJECT_DIR}/plan.md" << EOF
# Team Project: ${PROJECT_TITLE}

**ID**: ${PROJECT_ID}
${PARENT_LINE}
**Status**: planning
**Team**: ${TEAM_NAME}
**Created**: ${NOW}

## Workers

$(echo "${WORKERS_CSV}" | tr ',' '\n' | while read -r w; do echo "- @${w}:${MATRIX_DOMAIN} — (role TBD)"; done)

## DAG Task Plan

(To be filled in by Team Leader)

## Change Log

- ${NOW}: Project initiated
EOF

echo "  Project files created at ${PROJECT_DIR}"

# ============================================================
# Step 2: Sync to MinIO
# ============================================================
mc cp "${PROJECT_DIR}/meta.json" "${HICLAW_STORAGE_PREFIX}/teams/${TEAM_NAME}/shared/projects/${PROJECT_ID}/meta.json" 2>&1 | tail -1
mc cp "${PROJECT_DIR}/plan.md" "${HICLAW_STORAGE_PREFIX}/teams/${TEAM_NAME}/shared/projects/${PROJECT_ID}/plan.md" 2>&1 | tail -1
mc stat "${HICLAW_STORAGE_PREFIX}/teams/${TEAM_NAME}/shared/projects/${PROJECT_ID}/meta.json" > /dev/null 2>&1 \
    || { echo "ERROR: meta.json not found in MinIO after sync" >&2; exit 1; }
echo "  MinIO sync verified"

# ============================================================
# Step 3: Register in team-state.json
# ============================================================
STATE_ARGS=(--action add-project --project-id "${PROJECT_ID}" --title "${PROJECT_TITLE}")
[ -n "${SOURCE}" ] && STATE_ARGS+=(--source "${SOURCE}")
[ -n "${PARENT_TASK_ID}" ] && STATE_ARGS+=(--parent-task-id "${PARENT_TASK_ID}")
[ -n "${REQUESTER}" ] && STATE_ARGS+=(--requester "${REQUESTER}")

bash ./skills/team-task-management/scripts/manage-team-state.sh "${STATE_ARGS[@]}"

# ============================================================
# Output JSON result
# ============================================================
echo "---RESULT---"
echo "${META}"
