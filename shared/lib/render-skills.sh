#!/bin/bash
# render-skills.sh - Replace env var placeholders in agent doc files
# Usage: render-skills.sh <directory> [file1 file2 ...]
#   render-skills.sh /opt/agentteams/agent/skills          — render all .md in dir
#   render-skills.sh /dir AGENTS.md TOOLS.md            — render specific files in dir

DIR="${1:?Usage: render-skills.sh <directory> [files...]}"
[ ! -d "$DIR" ] && exit 0
shift

source /opt/agentteams/scripts/lib/agentteams-env.sh 2>/dev/null || true

# Defaults for variables that may not be set in all environments
export AGENTTEAMS_MATRIX_URL="${AGENTTEAMS_MATRIX_URL:-http://127.0.0.1:6167}"
export AGENTTEAMS_AI_GATEWAY_URL="${AGENTTEAMS_AI_GATEWAY_URL:-http://aigw-local.agentteams.io:8080}"
export AGENTTEAMS_DEFAULT_WORKER_RUNTIME="${AGENTTEAMS_DEFAULT_WORKER_RUNTIME:-openclaw}"
export AGENTTEAMS_SKILLS_API_URL="${AGENTTEAMS_SKILLS_API_URL:-https://skills.sh}"

# Whitelist: only replace these known variables, leave $task_id etc. untouched
VARS='${AGENTTEAMS_STORAGE_PREFIX} ${AGENTTEAMS_MATRIX_DOMAIN} ${AGENTTEAMS_MATRIX_URL}
${AGENTTEAMS_AI_GATEWAY_URL}
${AGENTTEAMS_ADMIN_USER} ${AGENTTEAMS_ADMIN_PASSWORD} ${AGENTTEAMS_REGISTRATION_TOKEN}
${AGENTTEAMS_DEFAULT_MODEL} ${AGENTTEAMS_AI_GATEWAY_DOMAIN} ${AGENTTEAMS_FS_DOMAIN}
${AGENTTEAMS_DEFAULT_WORKER_RUNTIME} ${AGENTTEAMS_WORKER_IMAGE} ${AGENTTEAMS_SKILLS_API_URL}
${AGENTTEAMS_CONTAINER_RUNTIME} ${AGENTTEAMS_GITHUB_TOKEN} ${AGENTTEAMS_WORKER_NAME}
${AGENTTEAMS_YOLO}
${MANAGER_MATRIX_TOKEN} ${MANAGER_TOKEN} ${HIGRESS_COOKIE_FILE}'

if [ $# -gt 0 ]; then
    # Render specific files
    for name in "$@"; do
        f="${DIR}/${name}"
        [ -f "$f" ] || continue
        envsubst "$VARS" < "$f" > "${f}.tmp" && mv "${f}.tmp" "$f"
    done
else
    # Render all .md files recursively
    find "$DIR" -name '*.md' -type f | while read -r f; do
        envsubst "$VARS" < "$f" > "${f}.tmp" && mv "${f}.tmp" "$f"
    done
fi
