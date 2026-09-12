#!/bin/bash
# test-27-qwenpaw-manager-startup.sh
# Verifies QwenPaw Manager startup and .copaw → .qwenpaw migration correctness.
# Covers the scenario where only QWENPAW_WORKING_DIR is set (no COPAW_WORKING_DIR).

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/test-helpers.sh"

test_setup "27-qwenpaw-manager-startup"

_AGENT_CTR="${TEST_AGENT_CONTAINER:-${TEST_CONTROLLER_CONTAINER:-agentteams-controller}}"
_HOT_SKILL="test-manager-hot-skill-$$"

_cleanup_hot_skill() {
    docker exec "${_AGENT_CTR}" rm -rf \
        "/root/manager-workspace/skills/${_HOT_SKILL}" 2>/dev/null || true
}
trap _cleanup_hot_skill EXIT

# Guard: skip if Manager is not QwenPaw (e.g., openclaw shard)
_MANAGER_RUNTIME=$(docker exec "${_AGENT_CTR}" printenv AGENTTEAMS_MANAGER_RUNTIME 2>/dev/null || echo "openclaw")
if [ "${_MANAGER_RUNTIME}" != "qwenpaw" ] && [ "${_MANAGER_RUNTIME}" != "copaw" ]; then
    log_info "Manager runtime is ${_MANAGER_RUNTIME} — skipping QwenPaw-specific test"
    test_teardown "27-qwenpaw-manager-startup"
    test_summary
    exit 0
fi

# ---- QWENPAW_WORKING_DIR ----
log_section "QWENPAW_WORKING_DIR"

_qwenpaw_wd=$(docker exec "${_AGENT_CTR}" printenv QWENPAW_WORKING_DIR 2>/dev/null || echo "")
if [ -n "${_qwenpaw_wd}" ]; then
    log_pass "QWENPAW_WORKING_DIR is set: ${_qwenpaw_wd}"
else
    log_info "QWENPAW_WORKING_DIR not set (using default ~/.qwenpaw)"
    _qwenpaw_wd="/root/manager-workspace/.qwenpaw"
fi

# ---- Working directory structure ----
log_section "Working Directory"

if docker exec "${_AGENT_CTR}" test -d "${_qwenpaw_wd}" 2>/dev/null; then
    log_pass ".qwenpaw directory exists"
else
    log_fail ".qwenpaw directory exists"
fi

if docker exec "${_AGENT_CTR}" test -f "${_qwenpaw_wd}/workspaces/default/agent.json" 2>/dev/null; then
    log_pass "agent.json exists in .qwenpaw workspace"
else
    log_fail "agent.json exists in .qwenpaw workspace"
fi

if docker exec "${_AGENT_CTR}" test -d "${_qwenpaw_wd}/workspaces/default/media" 2>/dev/null; then
    log_pass "QwenPaw Manager media directory exists for incoming attachments"
else
    log_fail "QwenPaw Manager media directory exists for incoming attachments"
fi

if docker exec "${_AGENT_CTR}" test -f "${_qwenpaw_wd}/workspaces/default/SOUL.md" 2>/dev/null || \
   docker exec "${_AGENT_CTR}" test -f "/root/manager-workspace/SOUL.md" 2>/dev/null; then
    log_pass "SOUL.md accessible"
else
    log_fail "SOUL.md accessible"
fi

# ---- Workspace prompt files ----
log_section "Workspace Prompts"

for _file in SOUL.md AGENTS.md HEARTBEAT.md; do
    _found=false
    if docker exec "${_AGENT_CTR}" test -f "${_qwenpaw_wd}/workspaces/default/${_file}" 2>/dev/null; then
        _found=true
    elif docker exec "${_AGENT_CTR}" test -f "/root/manager-workspace/${_file}" 2>/dev/null; then
        _found=true
    fi

    if [ "${_found}" = "true" ]; then
        log_pass "${_file} found"
    else
        log_fail "${_file} found"
    fi
done

# ---- Migration marker ----
log_section "Migration Marker"

if docker exec "${_AGENT_CTR}" test -f "${_qwenpaw_wd}/.copaw-migrated" 2>/dev/null; then
    log_pass ".copaw-migrated marker exists (migration completed)"
elif docker exec "${_AGENT_CTR}" test -d "/root/manager-workspace/.copaw" 2>/dev/null; then
    # Legacy .copaw exists but no marker → migration should retry on next boot
    log_info ".copaw exists but no migration marker (will retry on next startup)"
else
    # Fresh install — no .copaw to migrate
    log_info "No .copaw directory (fresh install, migration not needed)"
fi

# ---- Process check ----
log_section "Process Check"

if docker exec "${_AGENT_CTR}" pgrep -f "copaw(_worker\\.run_copaw_app)? app" >/dev/null 2>&1 || \
   docker exec "${_AGENT_CTR}" pgrep -f "qwenpaw app" >/dev/null 2>&1; then
    log_pass "QwenPaw process running"
else
    log_fail "QwenPaw process running"
fi

# ---- Health endpoint ----
log_section "Health Endpoint"

if docker exec "${_AGENT_CTR}" curl -sf http://127.0.0.1:18799/ >/dev/null 2>&1; then
    log_pass "QwenPaw health endpoint responding on :18799"
else
    log_fail "QwenPaw health endpoint responding on :18799"
fi

# ---- Manager workspace skill hot loading ----
log_section "Manager Skill Hot Loading"

docker exec "${_AGENT_CTR}" mkdir -p \
    "/root/manager-workspace/skills/${_HOT_SKILL}"
docker exec "${_AGENT_CTR}" bash -c \
    "printf '%s\n' '---' 'name: ${_HOT_SKILL}' 'description: QwenPaw Manager hot loading regression skill.' '---' '' '# Hot Loading Regression' > '/root/manager-workspace/skills/${_HOT_SKILL}/SKILL.md'"

_skill_loaded=false
_deadline=$(( $(date +%s) + 30 ))
while [ "$(date +%s)" -lt "${_deadline}" ]; do
    if docker exec "${_AGENT_CTR}" curl -fsS http://127.0.0.1:18799/api/skills 2>/dev/null \
        | jq -e --arg name "${_HOT_SKILL}" '.[] | select(.name == $name and .enabled == true)' \
            >/dev/null 2>&1; then
        _skill_loaded=true
        break
    fi
    sleep 1
done

if [ "${_skill_loaded}" = "true" ]; then
    log_pass "QwenPaw Manager discovers and enables a workspace skill without restart"
else
    log_fail "QwenPaw Manager did not hot-load workspace skill ${_HOT_SKILL}"
fi

# Update the canonical workspace copy and verify QwenPaw sees the new content.
docker exec "${_AGENT_CTR}" bash -c \
    "printf '%s\n' '---' 'name: ${_HOT_SKILL}' 'description: QwenPaw Manager updated hot loading regression skill.' '---' '' '# Hot Loading Regression Updated' > '/root/manager-workspace/skills/${_HOT_SKILL}/SKILL.md'"

# QwenPaw >= 2.2.0 skills API: the list endpoint (GET /api/skills) returns
# metadata only (SkillSpec, no content); skill content is served by the
# per-skill detail endpoint (GET /api/skills/{name}, SkillDetail) and read
# from disk at request time. Poll the detail endpoint: the Manager skill
# sync copies the canonical file into the workspace within ~1s, and the
# detail endpoint then serves the fresh content.
_skill_updated=false
_deadline=$(( $(date +%s) + 30 ))
while [ "$(date +%s)" -lt "${_deadline}" ]; do
    if docker exec "${_AGENT_CTR}" curl -fsS "http://127.0.0.1:18799/api/skills/${_HOT_SKILL}" 2>/dev/null \
        | jq -e '.content | contains("# Hot Loading Regression Updated")' \
            >/dev/null 2>&1; then
        _skill_updated=true
        break
    fi
    sleep 1
done

if [ "${_skill_updated}" = "true" ]; then
    log_pass "QwenPaw Manager hot-reloads updated workspace skill content"
else
    log_fail "QwenPaw Manager did not hot-reload updated skill ${_HOT_SKILL}"
fi

_cleanup_hot_skill

_skill_removed=false
_deadline=$(( $(date +%s) + 30 ))
while [ "$(date +%s)" -lt "${_deadline}" ]; do
    if docker exec "${_AGENT_CTR}" curl -fsS http://127.0.0.1:18799/api/skills 2>/dev/null \
        | jq -e --arg name "${_HOT_SKILL}" 'all(.name != $name)' >/dev/null 2>&1; then
        _skill_removed=true
        break
    fi
    sleep 1
done

if [ "${_skill_removed}" = "true" ]; then
    log_pass "QwenPaw Manager removes a deleted workspace skill without restart"
else
    log_fail "QwenPaw Manager still lists deleted skill ${_HOT_SKILL}"
fi

if docker exec "${_AGENT_CTR}" test ! -e \
    "${_qwenpaw_wd}/workspaces/default/active_skills/${_HOT_SKILL}"; then
    log_pass "QwenPaw Manager removes the deleted skill's active startup copy"
else
    log_fail "QwenPaw Manager left an active startup copy for ${_HOT_SKILL}"
fi

test_teardown "27-qwenpaw-manager-startup"
test_summary
