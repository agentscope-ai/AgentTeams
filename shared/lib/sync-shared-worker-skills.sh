#!/bin/bash
# sync-shared-worker-skills.sh - Copy canonical shared worker skills into runtime agent trees
#
# Keeps deploy layout stable (each *-worker-agent/skills/<name>/ still exists in images)
# while deduplicating script content under manager/agent/shared-worker-skills/.
#
# Usage:
#   sync-shared-worker-skills.sh [AGENT_SRC]
# Default AGENT_SRC: /opt/hiclaw/agent

set -e

AGENT_SRC="${1:-/opt/hiclaw/agent}"
SHARED_ROOT="${AGENT_SRC}/shared-worker-skills"

if [ ! -d "${SHARED_ROOT}" ]; then
    exit 0
fi

for skill_dir in "${SHARED_ROOT}"/*/; do
    [ -d "${skill_dir}" ] || continue
    skill_name="$(basename "${skill_dir}")"

    for runtime_dir in worker-agent copaw-worker-agent hermes-worker-agent openhuman-worker-agent qwenpaw-worker-agent; do
        target_root="${AGENT_SRC}/${runtime_dir}/skills/${skill_name}"
        mkdir -p "${target_root}"

        if [ -d "${skill_dir}scripts" ]; then
            mkdir -p "${target_root}/scripts"
            cp -r "${skill_dir}scripts/." "${target_root}/scripts/"
        fi
    done
done

find "${AGENT_SRC}" -path '*/skills/find-skills/scripts/*.sh' -exec chmod +x {} + 2>/dev/null || true
