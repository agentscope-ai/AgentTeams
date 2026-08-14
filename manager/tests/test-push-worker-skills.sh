#!/bin/bash
# Regression tests for Manager-controlled Worker skill distribution.

set -uo pipefail

PASS=0
FAIL=0
TMPDIR_ROOT=$(mktemp -d)
trap 'rm -rf "${TMPDIR_ROOT}"' EXIT

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
SCRIPT="${PROJECT_ROOT}/manager/agent/skills/worker-management/scripts/push-worker-skills.sh"

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; echo "       expected: $2"; echo "       got:      $3"; FAIL=$((FAIL + 1)); }

assert_contains() {
    local desc="$1" needle="$2" file="$3"
    if grep -qF -- "${needle}" "${file}"; then
        pass "${desc}"
    else
        fail "${desc}" "contains '${needle}'" "$(cat "${file}" 2>/dev/null || true)"
    fi
}

assert_not_contains() {
    local desc="$1" needle="$2" file="$3"
    if ! grep -qF -- "${needle}" "${file}"; then
        pass "${desc}"
    else
        fail "${desc}" "does not contain '${needle}'" "$(cat "${file}" 2>/dev/null || true)"
    fi
}

assert_eq() {
    local desc="$1" expected="$2" actual="$3"
    if [ "${expected}" = "${actual}" ]; then
        pass "${desc}"
    else
        fail "${desc}" "${expected}" "${actual}"
    fi
}

setup_case() {
    CASE_DIR="$1"
    mkdir -p "${CASE_DIR}/bin" "${CASE_DIR}/home/worker-skills/competition-skill"
    EVENTS="${CASE_DIR}/events.log"
    STATE="${CASE_DIR}/state.json"
    : > "${EVENTS}"

    cat > "${CASE_DIR}/home/worker-skills/competition-skill/SKILL.md" <<'EOF'
---
name: competition-skill
description: Competition skill used by the regression test.
assign_when: Use for competition tasks.
---
EOF

    cat > "${STATE}" <<'EOF'
{"workers":[{"name":"amy-ai","runtime":"openclaw","skills":[],"roomID":"!amy:example.com"}],"total":1}
EOF

    cat > "${CASE_DIR}/bin/agt" <<'EOF'
#!/bin/bash
set -euo pipefail
printf 'agt %s\n' "$*" >> "${TEST_EVENTS}"
if [ "${1:-}" = get ] && [ "${2:-}" = workers ]; then
    if [ "${3:-}" = -o ]; then
        cat "${TEST_STATE}"
    else
        jq --arg worker "${3:-}" '.workers[] | select(.name == $worker)' "${TEST_STATE}"
    fi
    exit 0
fi
if [ "${1:-}" = update ] && [ "${2:-}" = worker ]; then
    worker=""
    skills=""
    shift 2
    while [ $# -gt 0 ]; do
        case "$1" in
            --name) worker="$2"; shift 2 ;;
            --skills) skills="$2"; shift 2 ;;
            *) shift ;;
        esac
    done
    if [ "${TEST_SKIP_STATE_UPDATE:-0}" != 1 ]; then
        jq --arg worker "${worker}" --arg skills "${skills}" \
            '.workers |= map(if .name == $worker then .skills = (if $skills == "" then [] else ($skills | split(",")) end) else . end)' \
            "${TEST_STATE}" > "${TEST_STATE}.tmp"
        mv "${TEST_STATE}.tmp" "${TEST_STATE}"
    fi
    exit 0
fi
exit 2
EOF

cat > "${CASE_DIR}/bin/mc" <<'EOF'
#!/bin/bash
set -euo pipefail
printf 'mc %s\n' "$*" >> "${TEST_EVENTS}"
if [ "${1:-}" = stat ]; then
    if [ "${TEST_MC_STAT_SUCCESS:-0}" = 1 ] || grep -q '^mc mirror ' "${TEST_EVENTS}"; then
        exit 0
    fi
    exit 1
fi
exit 0
EOF
cat > "${CASE_DIR}/bin/curl" <<'EOF'
#!/bin/bash
set -euo pipefail
printf 'curl %s\n' "$*" >> "${TEST_EVENTS}"
EOF
    chmod +x "${CASE_DIR}/bin/agt" "${CASE_DIR}/bin/mc" "${CASE_DIR}/bin/curl"
}

run_script() {
    HOME="${CASE_DIR}/home" \
    PATH="${CASE_DIR}/bin:/usr/bin:/bin" \
    TEST_EVENTS="${EVENTS}" \
    TEST_STATE="${STATE}" \
    TEST_MC_STAT_SUCCESS="${MC_STAT_SUCCESS:-0}" \
    TEST_SKIP_STATE_UPDATE="${SKIP_STATE_UPDATE:-0}" \
    AGENTTEAMS_STORAGE_PREFIX="test/agentteams-storage" \
    AGENTTEAMS_MANAGER_MATRIX_TOKEN="manager-token" \
    AGENTTEAMS_MATRIX_URL="http://matrix.example.com" \
    AGENTTEAMS_MATRIX_DOMAIN="example.com" \
    bash "${SCRIPT}" "$@"
}

echo "=== TC1: add-skill distributes files before updating Worker.spec.skills ==="
setup_case "${TMPDIR_ROOT}/add"
if run_script --worker amy-ai --add-skill competition-skill; then
    pass "add-skill exits successfully"
else
    fail "add-skill exits successfully" "exit 0" "non-zero"
fi
assert_contains "mirror removes destination-only files from amy-ai storage" \
    "mc mirror ${CASE_DIR}/home/worker-skills/competition-skill/ test/agentteams-storage/agents/amy-ai/skills/competition-skill/ --overwrite --remove" \
    "${EVENTS}"
assert_contains "uploaded SKILL.md is verified before updating the CR" \
    "mc stat test/agentteams-storage/agents/amy-ai/skills/competition-skill/SKILL.md" \
    "${EVENTS}"
assert_contains "Worker.spec.skills is updated" \
    "agt update worker --name amy-ai --skills competition-skill" \
    "${EVENTS}"
assert_contains "worker is notified to pull the installed skill" \
    "http://matrix.example.com/_matrix/client/v3/rooms/!amy:example.com/send/m.room.message/" \
    "${EVENTS}"
mirror_line=$(grep -n '^mc mirror ' "${EVENTS}" | head -1 | cut -d: -f1)
update_line=$(grep -n '^agt update worker ' "${EVENTS}" | head -1 | cut -d: -f1)
if [ -n "${mirror_line}" ] && [ -n "${update_line}" ] && [ "${mirror_line}" -lt "${update_line}" ]; then
    pass "files are distributed before the CR update can trigger reconciliation"
else
    fail "files are distributed before the CR update can trigger reconciliation" \
        "mirror line before update line" "$(cat "${EVENTS}")"
fi

echo "=== TC2: reconcile mode pushes one assigned skill without calling the Controller API ==="
setup_case "${TMPDIR_ROOT}/reconcile"
cat > "${STATE}" <<'EOF'
{"workers":[{"name":"amy-ai","runtime":"openclaw","skills":["competition-skill"],"roomID":"!amy:example.com"}],"total":1}
EOF
if run_script --worker amy-ai --skill competition-skill --no-notify; then
    pass "reconcile push exits successfully"
else
    fail "reconcile push exits successfully" "exit 0" "non-zero"
fi
assert_contains "reconcile mirrors the assigned skill" \
    "mc mirror ${CASE_DIR}/home/worker-skills/competition-skill/ test/agentteams-storage/agents/amy-ai/skills/competition-skill/ --overwrite --remove" \
    "${EVENTS}"
assert_not_contains "reconcile does not recursively update Worker.spec.skills" \
    "agt update worker" "${EVENTS}"
assert_not_contains "reconcile does not call the Controller API" \
    "agt get workers" "${EVENTS}"
assert_not_contains "reconcile --no-notify does not send Matrix messages" \
    "curl " "${EVENTS}"

echo "=== TC3: Manager-only skills are not silently exposed to Workers ==="
setup_case "${TMPDIR_ROOT}/manager-only"
mkdir -p "${CASE_DIR}/home/skills/manager-only"
cat > "${CASE_DIR}/home/skills/manager-only/SKILL.md" <<'EOF'
---
name: manager-only
description: Must remain private to the Manager.
---
EOF
if run_script --worker amy-ai --add-skill manager-only >"${CASE_DIR}/output.log" 2>&1; then
    fail "Manager-only source is rejected" "non-zero" "exit 0"
else
    pass "Manager-only source is rejected"
fi
assert_contains "error points to the Worker skill directory" \
    "worker-skills/manager-only/SKILL.md" "${CASE_DIR}/output.log"
assert_not_contains "failed validation does not update Worker.spec.skills" \
    "agt update worker" "${EVENTS}"

echo "=== TC4: reconcile accepts a custom skill already uploaded by Manager ==="
setup_case "${TMPDIR_ROOT}/remote-existing"
rm -rf "${CASE_DIR}/home/worker-skills/competition-skill"
cat > "${STATE}" <<'EOF'
{"workers":[{"name":"amy-ai","runtime":"qwenpaw","skills":["competition-skill"],"roomID":"!amy:example.com"}],"total":1}
EOF
MC_STAT_SUCCESS=1
if run_script --worker amy-ai --skill competition-skill --no-notify; then
    pass "reconcile reuses an existing remote custom skill"
else
    fail "reconcile reuses an existing remote custom skill" "exit 0" "non-zero"
fi
unset MC_STAT_SUCCESS
assert_contains "reconcile verifies the remote SKILL.md" \
    "mc stat test/agentteams-storage/agents/amy-ai/skills/competition-skill/SKILL.md" \
    "${EVENTS}"
assert_not_contains "remote reuse does not recursively update Worker.spec.skills" \
    "agt update worker" "${EVENTS}"

echo "=== TC5: add-skill fails when the Worker CR does not confirm the assignment ==="
setup_case "${TMPDIR_ROOT}/assignment-verification"
SKIP_STATE_UPDATE=1
if run_script --worker amy-ai --add-skill competition-skill --no-notify \
    >"${CASE_DIR}/output.log" 2>&1; then
    fail "unconfirmed assignment exits non-zero" "non-zero" "exit 0"
else
    pass "unconfirmed assignment exits non-zero"
fi
unset SKIP_STATE_UPDATE
assert_contains "unconfirmed assignment reports the failed contract" \
    "Worker Skill assignment verification failed" "${CASE_DIR}/output.log"
assert_not_contains "failed verification does not notify the Worker" \
    "curl " "${EVENTS}"

echo
echo "Results: ${PASS} passed, ${FAIL} failed"
[ "${FAIL}" -eq 0 ]
