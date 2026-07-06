#!/bin/bash
# test-skill-copies-in-sync.sh
# Drift guard: hiclaw-find-skill.sh and push-shared.sh are duplicated across
# multiple worker-agent runtimes (and, for hiclaw-find-skill.sh, the plugin
# skill copy). These copies must stay byte-identical to their canonical
# source. This test hashes every copy in each set and fails, printing which
# files differ, if any copy has drifted from the canonical file.
#
# Usage: bash manager/tests/test-skill-copies-in-sync.sh

set -uo pipefail

PASS=0
FAIL=0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

# hiclaw-find-skill.sh: canonical + copies
FIND_SKILL_CANONICAL="${PROJECT_ROOT}/manager/agent/worker-agent/skills/find-skills/scripts/hiclaw-find-skill.sh"
FIND_SKILL_COPIES=(
    "${PROJECT_ROOT}/manager/agent/copaw-worker-agent/skills/find-skills/scripts/hiclaw-find-skill.sh"
    "${PROJECT_ROOT}/manager/agent/hermes-worker-agent/skills/find-skills/scripts/hiclaw-find-skill.sh"
    "${PROJECT_ROOT}/manager/agent/openhuman-worker-agent/skills/find-skills/scripts/hiclaw-find-skill.sh"
    "${PROJECT_ROOT}/plugins/teamharness/skills/agent/find-skills/scripts/hiclaw-find-skill.sh"
)

# push-shared.sh: canonical + copies
PUSH_SHARED_CANONICAL="${PROJECT_ROOT}/manager/agent/hermes-worker-agent/skills/file-sync/scripts/push-shared.sh"
PUSH_SHARED_COPIES=(
    "${PROJECT_ROOT}/manager/agent/openhuman-worker-agent/skills/file-sync/scripts/push-shared.sh"
)

hash_of() {
    sha256sum "$1" | awk '{print $1}'
}

check_set() {
    local set_name="$1" canonical="$2"
    shift 2
    local copies=("$@")

    if [ ! -f "${canonical}" ]; then
        echo "  FAIL: ${set_name}: canonical file missing: ${canonical}"
        FAIL=$((FAIL + 1))
        return
    fi

    local canonical_hash
    canonical_hash="$(hash_of "${canonical}")"

    local diverged=0
    local diverged_files=()

    for copy in "${copies[@]}"; do
        if [ ! -f "${copy}" ]; then
            echo "  FAIL: ${set_name}: copy missing: ${copy}"
            diverged=1
            diverged_files+=("${copy} (missing)")
            continue
        fi
        local copy_hash
        copy_hash="$(hash_of "${copy}")"
        if [ "${copy_hash}" != "${canonical_hash}" ]; then
            diverged=1
            diverged_files+=("${copy}")
        fi
    done

    if [ "${diverged}" -eq 0 ]; then
        echo "  PASS: ${set_name}: all copies match canonical (${canonical})"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: ${set_name}: drift detected against canonical (${canonical})"
        echo "        canonical sha256: ${canonical_hash}"
        for f in "${diverged_files[@]}"; do
            echo "        diverged: ${f}"
        done
        FAIL=$((FAIL + 1))
    fi
}

echo "=== Skill-copy drift guard ==="
echo ""

check_set "hiclaw-find-skill.sh" "${FIND_SKILL_CANONICAL}" "${FIND_SKILL_COPIES[@]}"
check_set "push-shared.sh" "${PUSH_SHARED_CANONICAL}" "${PUSH_SHARED_COPIES[@]}"

echo ""
if [ "${FAIL}" -eq 0 ]; then
    echo "All ${PASS} copy-sets in sync"
    exit 0
else
    echo "${FAIL} copy-set(s) diverged (${PASS} in sync)"
    exit 1
fi
