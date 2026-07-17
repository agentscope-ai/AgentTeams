#!/bin/bash
# Golden/parity test for shared/lib/merge-openclaw-config.sh.
#
# Feeds the shared fixture pairs under
# shared/tests/fixtures/openclaw-merge/<case>/{remote,local,expected}.json
# through merge_openclaw_config() and asserts the output is JSON-equal to
# expected.json. The SAME fixtures are also consumed by
# test_merge_openclaw_config_parity.py — see
# shared/tests/fixtures/openclaw-merge/README.md for the shared-fixture
# contract. The shell script delegates to agentteams_openclaw_merge.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
FIXTURES_DIR="${SCRIPT_DIR}/fixtures/openclaw-merge"
TMPDIR_ROOT="$(mktemp -d)"
trap 'rm -rf "${TMPDIR_ROOT}"' EXIT

if ! command -v jq >/dev/null 2>&1; then
    echo "SKIP: jq not found on PATH; cannot compare merge output" >&2
    exit 0
fi

if ! command -v python3 >/dev/null 2>&1; then
    echo "SKIP: python3 not found on PATH; cannot exercise merge-openclaw-config.sh" >&2
    exit 0
fi

MERGE_SRC="${PROJECT_ROOT}/shared/python/agentteams_openclaw_merge/src"
if ! PYTHONPATH="${MERGE_SRC}${PYTHONPATH:+:${PYTHONPATH}}" python3 -c "import agentteams_openclaw_merge" 2>/dev/null; then
    echo "SKIP: agentteams_openclaw_merge not importable; pip install shared/python/agentteams_openclaw_merge" >&2
    exit 0
fi

export PYTHONPATH="${MERGE_SRC}${PYTHONPATH:+:${PYTHONPATH}}"
source "${PROJECT_ROOT}/shared/lib/merge-openclaw-config.sh"

pass() { echo "  PASS: $1"; }
fail() { echo "  FAIL: $1" >&2; exit 1; }

failures=0

for case_dir in "${FIXTURES_DIR}"/*/; do
    case_name="$(basename "${case_dir}")"
    remote="${case_dir}remote.json"
    local_file="${case_dir}local.json"
    expected="${case_dir}expected.json"
    [ -f "${remote}" ] && [ -f "${local_file}" ] && [ -f "${expected}" ] || continue

    work_local="${TMPDIR_ROOT}/${case_name}-local.json"
    cp "${local_file}" "${work_local}"

    merge_openclaw_config "${remote}" "${work_local}" "${work_local}.out"

    # Prefer --slurpfile: --argfile was removed in modern jq (GitHub runners).
    if ! jq -e --slurpfile a "${work_local}.out" --slurpfile b "${expected}" -n '$a[0] == $b[0]' >/dev/null; then
        echo "  FAIL: ${case_name} merged output does not match expected.json" >&2
        echo "    got:      $(jq -c . "${work_local}.out")" >&2
        echo "    expected: $(jq -c . "${expected}")" >&2
        failures=$((failures + 1))
        continue
    fi
    pass "${case_name}"
done

if [ "${failures}" -ne 0 ]; then
    echo "FAILED: ${failures} case(s) did not match golden output" >&2
    exit 1
fi

echo "All merge-openclaw-config golden tests passed"
