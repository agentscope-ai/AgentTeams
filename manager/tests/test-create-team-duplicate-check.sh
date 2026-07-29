#!/bin/bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CREATE_TEAM="${SCRIPT_DIR}/../agent/skills/team-management/scripts/create-team.sh"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

cat >"${TMP_DIR}/agt" <<'MOCK'
#!/bin/bash
set -euo pipefail

case "$*" in
    "get teams -o json")
        if [ "${MOCK_LIST_MODE}" = "fail" ]; then
            echo "controller unavailable" >&2
            exit 42
        fi
        printf '%s\n' "${MOCK_TEAMS_JSON}"
        ;;
    "get workers leader -o json")
        printf '{"name":"leader"}\n'
        ;;
    "get workers worker -o json")
        printf '{"name":"worker"}\n'
        ;;
    "create team --name target --leader-name leader --peer-mentions=true --workers worker")
        : >"${MOCK_CREATE_LOG}"
        ;;
    "get teams target -o json")
        printf '{"name":"target"}\n'
        ;;
    *)
        echo "unexpected agt invocation: $*" >&2
        exit 99
        ;;
esac
MOCK
chmod +x "${TMP_DIR}/agt"

run_create() {
    PATH="${TMP_DIR}:${PATH}" \
        MOCK_LIST_MODE="$1" \
        MOCK_TEAMS_JSON="$2" \
        MOCK_CREATE_LOG="${TMP_DIR}/created" \
        bash "${CREATE_TEAM}" --name target --leader leader --workers worker
}

rm -f "${TMP_DIR}/created"
if run_create ok '{"teams":[{"name":"target"}]}' >"${TMP_DIR}/out" 2>"${TMP_DIR}/err"; then
    echo "FAIL: existing Team was created" >&2
    exit 1
fi
grep -q "Team 'target' already exists" "${TMP_DIR}/err"
grep -q "agt get teams target -o json" "${TMP_DIR}/err"
test ! -e "${TMP_DIR}/created"

rm -f "${TMP_DIR}/created"
if ! run_create ok '{"teams":[{"name":"other"}]}' >"${TMP_DIR}/out" 2>"${TMP_DIR}/err"; then
    cat "${TMP_DIR}/err" >&2
    echo "FAIL: missing Team could not be created" >&2
    exit 1
fi
test -e "${TMP_DIR}/created"

rm -f "${TMP_DIR}/created"
if run_create fail '{"teams":[]}' >"${TMP_DIR}/out" 2>"${TMP_DIR}/err"; then
    echo "FAIL: Team was created after the list command failed" >&2
    exit 1
fi
grep -q "Failed to list existing teams" "${TMP_DIR}/err"
test ! -e "${TMP_DIR}/created"

echo "PASS: create-team duplicate checks fail closed"
