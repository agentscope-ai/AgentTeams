#!/bin/bash
# Golden/parity test for shared/lib/merge-openclaw-config.sh.
#
# Feeds the shared fixture pairs under
# shared/tests/fixtures/openclaw-merge/<case>/{remote,local,expected}.json
# through merge_openclaw_config() and asserts the output is JSON-equal to
# expected.json. The SAME fixtures are also consumed by
# test_merge_openclaw_config_parity.py (Python impls) — see
# shared/tests/fixtures/openclaw-merge/README.md for the shared-fixture
# contract. Keep this file's merge semantics in lockstep with:
#   - copaw/src/copaw_worker/sync.py (_merge_openclaw_config)
#   - hermes/src/hermes_worker/sync.py (_merge_openclaw_config)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
FIXTURES_DIR="${SCRIPT_DIR}/fixtures/openclaw-merge"
TMPDIR_ROOT="$(mktemp -d)"
trap 'rm -rf "${TMPDIR_ROOT}"' EXIT

if ! command -v jq >/dev/null 2>&1; then
    echo "SKIP: jq not found on PATH; cannot exercise merge-openclaw-config.sh" >&2
    exit 0
fi

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

    if ! jq -e --argfile a "${work_local}.out" --argfile b "${expected}" -n '$a == $b' >/dev/null; then
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
