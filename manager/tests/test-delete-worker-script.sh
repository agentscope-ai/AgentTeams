#!/bin/bash
# Regression tests for worker-management/scripts/delete-worker.sh.
#
# Usage: bash manager/tests/test-delete-worker-script.sh

set -uo pipefail

PASS=0
FAIL=0
TMPDIR_ROOT=$(mktemp -d)
trap 'rm -rf "${TMPDIR_ROOT}"' EXIT

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
SCRIPT="${PROJECT_ROOT}/manager/agent/skills/worker-management/scripts/delete-worker.sh"

if ! command -v jq >/dev/null 2>&1; then
    echo "SKIP: jq not found; delete-worker.sh runs inside Manager images where jq is installed."
    exit 0
fi

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

assert_json_eq() {
    local desc="$1" expected="$2" json="$3" filter="$4"
    local actual
    actual=$(echo "${json}" | jq -r "${filter}")
    assert_eq "${desc}" "${expected}" "${actual}"
}

new_home() {
    mktemp -d "${TMPDIR_ROOT}/home-XXXXXX"
}

new_mockbin() {
    local dir
    dir=$(mktemp -d "${TMPDIR_ROOT}/bin-XXXXXX")
    cat > "${dir}/hiclaw" <<'EOF'
#!/bin/sh
set -eu
printf '%s\n' "$*" >> "${HICLAW_MOCK_LOG:?}"

if [ "${HICLAW_MOCK_FAIL:-0}" = "1" ]; then
    echo "mock delete failed" >&2
    exit 17
fi

if [ "$#" -eq 3 ] && [ "$1" = "delete" ] && [ "$2" = "worker" ]; then
    echo "worker/$3 deleted"
    exit 0
fi

echo "unexpected args: $*" >&2
exit 2
EOF
    chmod +x "${dir}/hiclaw"
    echo "${dir}"
}

echo ""
echo "=== TC1: delete uses positional hiclaw syntax ==="
{
    home=$(new_home)
    mockbin=$(new_mockbin)
    log="${TMPDIR_ROOT}/hiclaw.log"
    output=$(HOME="${home}" PATH="${mockbin}:$PATH" HICLAW_MOCK_LOG="${log}" bash "${SCRIPT}" --worker zhao-yun)
    assert_json_eq "delete status" "deleted" "${output}" ".status"
    assert_json_eq "deleted true" "true" "${output}" ".deleted"
    assert_eq "hiclaw called with positional worker name" "delete worker zhao-yun" "$(cat "${log}")"
}

echo ""
echo "=== TC2: failed hiclaw delete is not reported as success ==="
{
    home=$(new_home)
    mockbin=$(new_mockbin)
    log="${TMPDIR_ROOT}/hiclaw-fail.log"
    set +e
    output=$(HOME="${home}" PATH="${mockbin}:$PATH" HICLAW_MOCK_LOG="${log}" HICLAW_MOCK_FAIL=1 bash "${SCRIPT}" --worker zhao-yun)
    exit_code=$?
    set -e
    assert_eq "script exits with hiclaw failure" "17" "${exit_code}"
    assert_json_eq "failure status" "failed" "${output}" ".status"
    assert_json_eq "deleted false" "false" "${output}" ".deleted"
}

echo ""
echo "=== TC3: records-only cleans local state ==="
{
    home=$(new_home)
    cat > "${home}/state.json" <<'EOF'
{"active_tasks":[
  {"task_id":"keep","assigned_to":"other"},
  {"task_id":"drop","assigned_to":"zhao-yun"},
  {"task_id":"drop-legacy","worker":"zhao-yun"}
]}
EOF
    cat > "${home}/worker-lifecycle.json" <<'EOF'
{"workers":{"zhao-yun":{"container_status":"running"},"other":{"container_status":"running"}}}
EOF
    cat > "${home}/workers-registry.json" <<'EOF'
{"workers":{"zhao-yun":{"runtime":"openclaw"},"other":{"runtime":"openclaw"}}}
EOF

    output=$(HOME="${home}" bash "${SCRIPT}" --worker zhao-yun --records-only)
    assert_json_eq "records-only status" "skipped" "${output}" ".status"
    assert_eq "only unrelated active task remains" "keep" "$(jq -r '.active_tasks[].task_id' "${home}/state.json")"
    assert_eq "lifecycle entry removed" "false" "$(jq -r '.workers | has("zhao-yun")' "${home}/worker-lifecycle.json")"
    assert_eq "registry entry removed" "false" "$(jq -r '.workers | has("zhao-yun")' "${home}/workers-registry.json")"
}

echo ""
echo "=== Summary ==="
echo "PASS: ${PASS}"
echo "FAIL: ${FAIL}"

if [ "${FAIL}" -ne 0 ]; then
    exit 1
fi
