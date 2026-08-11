#!/bin/bash
# Regression tests for safe chat-attachment Worker Skill installation.

set -uo pipefail

PASS=0
FAIL=0
TMPDIR_ROOT=$(mktemp -d)
trap 'rm -rf "${TMPDIR_ROOT}"' EXIT

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
SCRIPT="${PROJECT_ROOT}/manager/agent/skills/worker-management/scripts/install-worker-skill.sh"

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

setup_case() {
    CASE_DIR="$1"
    mkdir -p "${CASE_DIR}/bin" "${CASE_DIR}/home"
    EVENTS="${CASE_DIR}/events.log"
    STATE="${CASE_DIR}/state.json"
    : > "${EVENTS}"
    cat > "${STATE}" <<'EOF'
{"workers":[{"name":"amy-ai","runtime":"qwenpaw","skills":[],"roomID":"!amy:example.com"}],"total":1}
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
    jq --arg worker "${worker}" --arg skills "${skills}" \
        '.workers |= map(if .name == $worker then .skills = (if $skills == "" then [] else ($skills | split(",")) end) else . end)' \
        "${TEST_STATE}" > "${TEST_STATE}.tmp"
    mv "${TEST_STATE}.tmp" "${TEST_STATE}"
    exit 0
fi
exit 2
EOF

    cat > "${CASE_DIR}/bin/mc" <<'EOF'
#!/bin/bash
set -euo pipefail
printf 'mc %s\n' "$*" >> "${TEST_EVENTS}"
if [ "${1:-}" = stat ]; then
    grep -q '^mc mirror ' "${TEST_EVENTS}"
fi
EOF
    chmod +x "${CASE_DIR}/bin/agt" "${CASE_DIR}/bin/mc"
}

create_zip() {
    local archive="$1" skill_name="$2" description="$3" assign_when="${4:-}"
    ARCHIVE="${archive}" SKILL_NAME="${skill_name}" DESCRIPTION="${description}" ASSIGN_WHEN="${assign_when}" \
        python3 - <<'PY'
import os
import zipfile

lines = [
    "---",
    f"name: {os.environ['SKILL_NAME']}",
    f"description: {os.environ['DESCRIPTION']}",
]
if os.environ["ASSIGN_WHEN"]:
    lines.append(f"assign_when: {os.environ['ASSIGN_WHEN']}")
lines.extend(["---", "", "# Test Skill", ""])
with zipfile.ZipFile(os.environ["ARCHIVE"], "w") as archive:
    archive.writestr(
        f"{os.environ['SKILL_NAME']}/SKILL.md",
        "\n".join(lines),
    )
PY
}

run_script() {
    HOME="${CASE_DIR}/home" \
    PATH="${CASE_DIR}/bin:${PATH}" \
    TEST_EVENTS="${EVENTS}" \
    TEST_STATE="${STATE}" \
    AGENTTEAMS_STORAGE_PREFIX="test/agentteams-storage" \
    bash "${SCRIPT}" "$@"
}

echo "=== TC1: explicit installation accepts a Skill without assign_when ==="
setup_case "${TMPDIR_ROOT}/without-assign-when"
create_zip "${CASE_DIR}/skill.zip" "direct-skill" "Explicitly assigned test Skill."
if run_script --worker amy-ai --archive "${CASE_DIR}/skill.zip" --no-notify \
    >"${CASE_DIR}/output.log" 2>"${CASE_DIR}/error.log"; then
    pass "installation without assign_when exits successfully"
else
    fail "installation without assign_when exits successfully" "exit 0" "$(cat "${CASE_DIR}/error.log")"
fi
if [ -f "${CASE_DIR}/home/worker-skills/direct-skill/SKILL.md" ]; then
    pass "validated Skill is staged in the Manager Worker Skill repository"
else
    fail "validated Skill is staged in the Manager Worker Skill repository" "SKILL.md exists" "missing"
fi
assert_contains "standard push script mirrors the imported Skill" \
    "mc mirror ${CASE_DIR}/home/worker-skills/direct-skill/ test/agentteams-storage/agents/amy-ai/skills/direct-skill/ --overwrite" \
    "${EVENTS}"
assert_contains "standard push script updates Worker.spec.skills" \
    "agt update worker --name amy-ai --skills direct-skill" "${EVENTS}"
assert_contains "result records that assign_when was absent" \
    '"hasAssignWhen":false' "${CASE_DIR}/output.log"

echo "=== TC2: optional assign_when is preserved when present ==="
setup_case "${TMPDIR_ROOT}/with-assign-when"
create_zip "${CASE_DIR}/skill.zip" "auto-skill" "Automatically selectable test Skill." "Use for automated tests."
if run_script --worker amy-ai --archive "${CASE_DIR}/skill.zip" --no-notify \
    >"${CASE_DIR}/output.log" 2>"${CASE_DIR}/error.log"; then
    pass "installation with assign_when exits successfully"
else
    fail "installation with assign_when exits successfully" "exit 0" "$(cat "${CASE_DIR}/error.log")"
fi
assert_contains "result records that assign_when was present" \
    '"hasAssignWhen":true' "${CASE_DIR}/output.log"

echo "=== TC3: missing required description is rejected before distribution ==="
setup_case "${TMPDIR_ROOT}/missing-description"
create_zip "${CASE_DIR}/skill.zip" "invalid-skill" ""
if run_script --worker amy-ai --archive "${CASE_DIR}/skill.zip" --no-notify \
    >"${CASE_DIR}/output.log" 2>"${CASE_DIR}/error.log"; then
    fail "missing description is rejected" "non-zero" "exit 0"
else
    pass "missing description is rejected"
fi
assert_contains "validation explains the required field" \
    "frontmatter field 'description' is required" "${CASE_DIR}/error.log"
assert_not_contains "invalid archive never invokes object storage" "mc " "${EVENTS}"
assert_not_contains "invalid archive never updates Worker CR" "agt update worker" "${EVENTS}"

echo "=== TC4: path traversal is rejected before extraction ==="
setup_case "${TMPDIR_ROOT}/path-traversal"
ARCHIVE="${CASE_DIR}/skill.zip" python3 - <<'PY'
import os
import zipfile

with zipfile.ZipFile(os.environ["ARCHIVE"], "w") as archive:
    archive.writestr("../SKILL.md", "unsafe")
PY
if run_script --worker amy-ai --archive "${CASE_DIR}/skill.zip" --no-notify \
    >"${CASE_DIR}/output.log" 2>"${CASE_DIR}/error.log"; then
    fail "path traversal is rejected" "non-zero" "exit 0"
else
    pass "path traversal is rejected"
fi
assert_contains "path traversal failure is explicit" \
    "unsafe ZIP entry path" "${CASE_DIR}/error.log"

echo "=== TC5: symlinks are rejected before extraction ==="
setup_case "${TMPDIR_ROOT}/symlink"
ARCHIVE="${CASE_DIR}/skill.zip" python3 - <<'PY'
import os
import stat
import zipfile

with zipfile.ZipFile(os.environ["ARCHIVE"], "w") as archive:
    archive.writestr(
        "linked-skill/SKILL.md",
        "---\nname: linked-skill\ndescription: Symlink test.\n---\n",
    )
    info = zipfile.ZipInfo("linked-skill/scripts/run.sh")
    info.create_system = 3
    info.external_attr = (stat.S_IFLNK | 0o777) << 16
    archive.writestr(info, "/tmp/target")
PY
if run_script --worker amy-ai --archive "${CASE_DIR}/skill.zip" --no-notify \
    >"${CASE_DIR}/output.log" 2>"${CASE_DIR}/error.log"; then
    fail "symlink is rejected" "non-zero" "exit 0"
else
    pass "symlink is rejected"
fi
assert_contains "symlink failure is explicit" "symlinks are not allowed" "${CASE_DIR}/error.log"

echo "=== TC6: the Skill directory must match the frontmatter name ==="
setup_case "${TMPDIR_ROOT}/name-mismatch"
ARCHIVE="${CASE_DIR}/skill.zip" python3 - <<'PY'
import os
import zipfile

with zipfile.ZipFile(os.environ["ARCHIVE"], "w") as archive:
    archive.writestr(
        "wrong-directory/SKILL.md",
        "---\nname: expected-skill\ndescription: Name mismatch test.\n---\n",
    )
PY
if run_script --worker amy-ai --archive "${CASE_DIR}/skill.zip" --no-notify \
    >"${CASE_DIR}/output.log" 2>"${CASE_DIR}/error.log"; then
    fail "directory/name mismatch is rejected" "non-zero" "exit 0"
else
    pass "directory/name mismatch is rejected"
fi
assert_contains "directory/name mismatch failure is explicit" \
    "does not match frontmatter name" "${CASE_DIR}/error.log"

echo "=== TC7: an archive cannot contain multiple Skill roots ==="
setup_case "${TMPDIR_ROOT}/multiple-roots"
ARCHIVE="${CASE_DIR}/skill.zip" python3 - <<'PY'
import os
import zipfile

with zipfile.ZipFile(os.environ["ARCHIVE"], "w") as archive:
    archive.writestr(
        "first-skill/SKILL.md",
        "---\nname: first-skill\ndescription: First Skill.\n---\n",
    )
    archive.writestr(
        "second-skill/SKILL.md",
        "---\nname: second-skill\ndescription: Second Skill.\n---\n",
    )
PY
if run_script --worker amy-ai --archive "${CASE_DIR}/skill.zip" --no-notify \
    >"${CASE_DIR}/output.log" 2>"${CASE_DIR}/error.log"; then
    fail "multiple Skill roots are rejected" "non-zero" "exit 0"
else
    pass "multiple Skill roots are rejected"
fi
assert_contains "multiple root failure is explicit" \
    "must contain exactly one Skill root" "${CASE_DIR}/error.log"

echo "=== TC8: failed distribution rolls back a newly staged canonical Skill ==="
setup_case "${TMPDIR_ROOT}/distribution-rollback"
jq '.workers[0].skills = ["missing-skill"]' "${STATE}" > "${STATE}.tmp"
mv "${STATE}.tmp" "${STATE}"
create_zip "${CASE_DIR}/skill.zip" "rollback-skill" "Rollback test Skill."
if run_script --worker amy-ai --archive "${CASE_DIR}/skill.zip" --no-notify \
    >"${CASE_DIR}/output.log" 2>"${CASE_DIR}/error.log"; then
    fail "distribution failure is reported" "non-zero" "exit 0"
else
    pass "distribution failure is reported"
fi
assert_contains "failure identifies the unresolved assigned Skill" \
    "Worker skill not found" "${CASE_DIR}/error.log"
if [ ! -e "${CASE_DIR}/home/worker-skills/rollback-skill" ]; then
    pass "new canonical Skill is removed after failed distribution"
else
    fail "new canonical Skill is removed after failed distribution" "path absent" "path exists"
fi
assert_not_contains "failed distribution does not update Worker CR" \
    "agt update worker" "${EVENTS}"

echo
echo "Results: ${PASS} passed, ${FAIL} failed"
[ "${FAIL}" -eq 0 ]
