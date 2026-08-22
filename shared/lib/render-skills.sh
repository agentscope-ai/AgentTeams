#!/bin/bash
# render-skills.sh - Replace env var placeholders in agent doc files
# Usage: render-skills.sh <directory> [file1 file2 ...]
#   render-skills.sh /opt/hiclaw/agent/skills          — render all .md in dir
#   render-skills.sh /dir AGENTS.md TOOLS.md            — render specific files in dir

DIR="${1:?Usage: render-skills.sh <directory> [files...]}"
[ ! -d "$DIR" ] && exit 0
shift

source /opt/hiclaw/scripts/lib/hiclaw-env.sh 2>/dev/null || true

# Defaults for variables that may not be set in all environments
export HICLAW_MATRIX_SERVER="${HICLAW_MATRIX_SERVER:-http://127.0.0.1:6167}"
export HICLAW_DEFAULT_WORKER_RUNTIME="${HICLAW_DEFAULT_WORKER_RUNTIME:-openclaw}"
export HICLAW_SKILLS_API_URL="${HICLAW_SKILLS_API_URL:-https://skills.sh}"

# Whitelist: only replace these known variables, leave $task_id etc. untouched
VARS='${HICLAW_STORAGE_PREFIX} ${HICLAW_MATRIX_DOMAIN} ${HICLAW_MATRIX_SERVER}
${HICLAW_ADMIN_USER} ${HICLAW_ADMIN_PASSWORD} ${HICLAW_REGISTRATION_TOKEN}
${HICLAW_DEFAULT_MODEL} ${HICLAW_AI_GATEWAY_DOMAIN} ${HICLAW_FS_DOMAIN}
${HICLAW_DEFAULT_WORKER_RUNTIME} ${HICLAW_WORKER_IMAGE} ${HICLAW_SKILLS_API_URL}
${HICLAW_CONTAINER_RUNTIME} ${HICLAW_GITHUB_TOKEN} ${HICLAW_WORKER_NAME}
${HICLAW_YOLO}
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
