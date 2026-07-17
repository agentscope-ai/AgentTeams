#!/bin/bash
# test-verify-output.sh
# Unit tests for verify-output.sh and manage-state.sh --action verify wiring.
#
# Usage: bash manager/tests/test-verify-output.sh

set -uo pipefail

PASS=0
FAIL=0
TMPDIR_ROOT=$(mktemp -d)
trap 'rm -rf "${TMPDIR_ROOT}"' EXIT

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
VERIFY_SCRIPT="${PROJECT_ROOT}/manager/agent/skills/task-management/scripts/verify-output.sh"
STATE_SCRIPT="${PROJECT_ROOT}/manager/agent/skills/task-management/scripts/manage-state.sh"

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; echo "       expected: $2"; echo "       got:      $3"; FAIL=$((FAIL + 1)); }

assert_eq() {
    local desc="$1" expected="$2" actual="$3"
    if [ "${expected}" = "${actual}" ]; then
        pass "${desc}"
    else
        fail "${desc}" "${expected}" "${actual}"
    fi
}

assert_json_true() {
    local desc="$1" json="$2" filter="$3"
    local actual
    actual=$(jq -r "${filter}" <<< "${json}")
    assert_eq "${desc}" "true" "${actual}"
}

assert_json_false() {
    local desc="$1" json="$2" filter="$3"
    local actual
    actual=$(jq -r "${filter}" <<< "${json}")
    assert_eq "${desc}" "false" "${actual}"
}

new_task_root() {
    mktemp -d "${TMPDIR_ROOT}/taskroot-XXXXXX"
}

run_verify() {
    local task_root="$1"
    local task_id="$2"
    shift 2
    HICLAW_TASK_ROOT="${task_root}" bash "${VERIFY_SCRIPT}" --task-id "${task_id}" "$@"
}

run_manage_verify() {
    local task_root="$1"
    local task_id="$2"
    HICLAW_TASK_ROOT="${task_root}" HICLAW_MANAGER_STATE_IMPL=shell \
        bash "${STATE_SCRIPT}" --action verify --task-id "${task_id}"
}

write_result() {
    local task_dir="$1"
    cat > "${task_dir}/result.md" <<'EOF'
STATUS: SUCCESS
SUMMARY: Done.

DELIVERABLES:
- shared/tasks/TASK-ID/result.md
- shared/tasks/TASK-ID/deliverable.md

NOTES:
- ok
EOF
    sed -i "s/TASK-ID/${task_dir##*/}/g" "${task_dir}/result.md"
}

echo ""
echo "=== TC1: verify passes when result.md and deliverables are non-empty ==="
{
    root=$(new_task_root)
    task_id="task-20260717-120000"
    task_dir="${root}/shared/tasks/${task_id}"
    mkdir -p "${task_dir}"
    write_result "${task_dir}"
    echo "content" > "${task_dir}/deliverable.md"

    set +e
    out=$(run_verify "${root}" "${task_id}" 2>&1)
    rc=$?
    set -e

    assert_eq "exit code 0" "0" "${rc}"
    assert_json_true "verified true" "${out}" '.verified'
    assert_eq "claims include result.md" "true" "$(jq -r '[.claims[] | select(.path | endswith("result.md"))] | length > 0' <<< "${out}")"
}

echo ""
echo "=== TC2: verify fails when result.md is missing ==="
{
    root=$(new_task_root)
    task_id="task-missing-result"
    task_dir="${root}/shared/tasks/${task_id}"
    mkdir -p "${task_dir}"

    set +e
    out=$(run_verify "${root}" "${task_id}" 2>&1)
    rc=$?
    set -e

    assert_eq "exit code 1" "1" "${rc}"
    assert_json_false "verified false" "${out}" '.verified'
}

echo ""
echo "=== TC3: verify fails when result.md is empty ==="
{
    root=$(new_task_root)
    task_id="task-empty-result"
    task_dir="${root}/shared/tasks/${task_id}"
    mkdir -p "${task_dir}"
    : > "${task_dir}/result.md"

    set +e
    out=$(run_verify "${root}" "${task_id}" 2>&1)
    rc=$?
    set -e

    assert_eq "exit code 1" "1" "${rc}"
    assert_json_false "verified false" "${out}" '.verified'
}

echo ""
echo "=== TC4: verify fails when a listed deliverable is missing ==="
{
    root=$(new_task_root)
    task_id="task-missing-deliverable"
    task_dir="${root}/shared/tasks/${task_id}"
    mkdir -p "${task_dir}"
    cat > "${task_dir}/result.md" <<EOF
STATUS: SUCCESS
SUMMARY: Done.

DELIVERABLES:
- shared/tasks/${task_id}/result.md
- shared/tasks/${task_id}/deliverable.md
EOF

    set +e
    out=$(run_verify "${root}" "${task_id}" 2>&1)
    rc=$?
    set -e

    assert_eq "exit code 1" "1" "${rc}"
    assert_json_false "verified false" "${out}" '.verified'
}

echo ""
echo "=== TC5: optional verifiable_claim failure does not fail verification ==="
{
    root=$(new_task_root)
    task_id="task-optional-claim"
    task_dir="${root}/shared/tasks/${task_id}"
    mkdir -p "${task_dir}"
    cat > "${task_dir}/result.md" <<EOF
STATUS: SUCCESS
SUMMARY: Done.

DELIVERABLES:
- shared/tasks/${task_id}/result.md
EOF
    cat > "${task_dir}/meta.json" <<EOF
{
  "task_id": "${task_id}",
  "verifiable_claims": [
    {"path": "shared/tasks/${task_id}/optional.md", "check": "nonempty", "required": false}
  ]
}
EOF

    set +e
    out=$(run_verify "${root}" "${task_id}" 2>&1)
    rc=$?
    set -e

    assert_eq "exit code 0" "0" "${rc}"
    assert_json_true "verified true" "${out}" '.verified'
    assert_eq "optional claim recorded as failed" "false" \
        "$(jq -r '.claims[] | select(.path | endswith("optional.md")) | .passed' <<< "${out}")"
}

echo ""
echo "=== TC6: required verifiable_claim failure fails verification ==="
{
    root=$(new_task_root)
    task_id="task-required-claim"
    task_dir="${root}/shared/tasks/${task_id}"
    mkdir -p "${task_dir}"
    cat > "${task_dir}/result.md" <<EOF
STATUS: SUCCESS
SUMMARY: Done.

DELIVERABLES:
- shared/tasks/${task_id}/result.md
EOF
    cat > "${task_dir}/meta.json" <<EOF
{
  "task_id": "${task_id}",
  "verifiable_claims": [
    {"path": "shared/tasks/${task_id}/required.md", "check": "exists"}
  ]
}
EOF

    set +e
    out=$(run_verify "${root}" "${task_id}" 2>&1)
    rc=$?
    set -e

    assert_eq "exit code 1" "1" "${rc}"
    assert_json_false "verified false" "${out}" '.verified'
}

echo ""
echo "=== TC7: manage-state.sh --action verify delegates to verify-output.sh ==="
{
    root=$(new_task_root)
    task_id="task-manage-verify"
    task_dir="${root}/shared/tasks/${task_id}"
    mkdir -p "${task_dir}"
    echo "done" > "${task_dir}/result.md"

    set +e
    out=$(run_manage_verify "${root}" "${task_id}" 2>&1)
    rc=$?
    set -e

    assert_eq "exit code 0" "0" "${rc}"
    assert_json_true "verified true via manage-state" "${out}" '.verified'
    assert_eq "task_id echoed" "${task_id}" "$(jq -r '.task_id' <<< "${out}")"
}

echo ""
echo "=== TC8: verify bypasses hiclaw manager-state delegation when hiclaw is on PATH ==="
{
    root=$(new_task_root)
    task_id="task-bypass-hiclaw"
    task_dir="${root}/shared/tasks/${task_id}"
    mkdir -p "${task_dir}"
    echo "done" > "${task_dir}/result.md"

    fake_bin="${TMPDIR_ROOT}/fakebin"
    mkdir -p "${fake_bin}"
    cat > "${fake_bin}/hiclaw" <<'EOF'
#!/bin/bash
echo "hiclaw manager-state should not run for verify" >&2
exit 99
EOF
    chmod +x "${fake_bin}/hiclaw"

    set +e
    out=$(PATH="${fake_bin}:${PATH}" HICLAW_TASK_ROOT="${root}" \
        bash "${STATE_SCRIPT}" --action verify --task-id "${task_id}" 2>&1)
    rc=$?
    set -e

    assert_eq "exit code 0 (not 99)" "0" "${rc}"
    assert_json_true "verified true with fake hiclaw on PATH" "${out}" '.verified'
}

echo ""
echo "=== TC9: verify parses ## Deliverables heading ==="
{
    root=$(new_task_root)
    task_id="task-markdown-deliverables"
    task_dir="${root}/shared/tasks/${task_id}"
    mkdir -p "${task_dir}"
    cat > "${task_dir}/result.md" <<EOF
STATUS: SUCCESS
SUMMARY: Done.

## Deliverables
- shared/tasks/${task_id}/result.md
- shared/tasks/${task_id}/artifact.md

NOTES:
- ok
EOF
    echo "artifact body" > "${task_dir}/artifact.md"
    echo "result body" > "${task_dir}/result.md"

    set +e
    out=$(run_verify "${root}" "${task_id}" 2>&1)
    rc=$?
    set -e

    assert_eq "exit code 0" "0" "${rc}"
    assert_json_true "verified true for ## Deliverables" "${out}" '.verified'
    assert_eq "artifact claim passed" "true" \
        "$(jq -r '.claims[] | select(.path | endswith("artifact.md")) | .passed' <<< "${out}")"
}

echo ""
echo "=== TC10: verify resolves task-relative deliverable paths ==="
{
    root=$(new_task_root)
    task_id="task-relative-path"
    task_dir="${root}/shared/tasks/${task_id}"
    mkdir -p "${task_dir}"
    cat > "${task_dir}/result.md" <<EOF
STATUS: SUCCESS
SUMMARY: Done.

DELIVERABLES:
- result.md
- deliverable.md
EOF
    echo "result body" > "${task_dir}/result.md"
    echo "deliverable body" > "${task_dir}/deliverable.md"

    set +e
    out=$(run_verify "${root}" "${task_id}" 2>&1)
    rc=$?
    set -e

    assert_eq "exit code 0" "0" "${rc}"
    assert_json_true "verified true for task-relative paths" "${out}" '.verified'
    assert_eq "relative deliverable claim passed" "true" \
        "$(jq -r '.claims[] | select(.path | endswith("deliverable.md")) | .passed' <<< "${out}")"
}

echo ""
echo "=== TC11: verify passes exists check for present task-relative path ==="
{
    root=$(new_task_root)
    task_id="task-exists-check"
    task_dir="${root}/shared/tasks/${task_id}"
    mkdir -p "${task_dir}"
    cat > "${task_dir}/result.md" <<EOF
STATUS: SUCCESS
SUMMARY: Done.

DELIVERABLES:
- shared/tasks/${task_id}/result.md
EOF
    echo "result body" > "${task_dir}/result.md"
    touch "${task_dir}/marker.txt"
    cat > "${task_dir}/meta.json" <<EOF
{
  "task_id": "${task_id}",
  "verifiable_claims": [
    {"path": "marker.txt", "check": "exists"}
  ]
}
EOF

    set +e
    out=$(run_verify "${root}" "${task_id}" 2>&1)
    rc=$?
    set -e

    assert_eq "exit code 0" "0" "${rc}"
    assert_json_true "verified true with exists claim" "${out}" '.verified'
    assert_eq "exists claim passed" "true" \
        "$(jq -r '.claims[] | select(.path | endswith("marker.txt")) | .passed' <<< "${out}")"
    assert_eq "exists detail mentions path exists" "path exists" \
        "$(jq -r '.claims[] | select(.path | endswith("marker.txt")) | .detail' <<< "${out}")"
}

echo ""
echo "================================"
echo "Results: ${PASS} passed, ${FAIL} failed"
echo "================================"
[ "${FAIL}" -eq 0 ]
