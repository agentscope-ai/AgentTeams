#!/usr/bin/env bash
# check-create-project-team.sh - create-project.sh --team expands a Team, each
# member is invited once, and lookup and join failures are reported (#1389).
#
# create-project.sh itself needs the Manager container, so the member-list
# helpers it sources run here against stub agt, curl and mc on PATH.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT_DIR="${ROOT_DIR}/manager/agent/skills/project-management/scripts"
SCRIPT="${SCRIPT_DIR}/create-project.sh"
HELPERS="${SCRIPT_DIR}/project-members.sh"
REFERENCE="${ROOT_DIR}/manager/agent/skills/project-management/references/create-project.md"

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

[ -f "${HELPERS}" ] || fail "project-members.sh not found at ${HELPERS}"

STUB_DIR="$(mktemp -d)"
trap 'rm -rf "${STUB_DIR}"' EXIT

# agt: two Teams, one with a leader only, one with nobody; anything else fails.
cat > "${STUB_DIR}/agt" << 'EOF'
#!/usr/bin/env bash
case "$*" in
    "get teams alpha -o json") echo '{"name":"alpha","leaderName":"lead","workerNames":["w1","w2"]}' ;;
    "get teams solo -o json")  echo '{"name":"solo","leaderName":"lead"}' ;;
    "get teams empty -o json") echo '{"name":"empty","workerNames":[]}' ;;
    *) echo "Error: teams.agentteams.io not found" >&2; exit 1 ;;
esac
EOF
# mc: every worker but "nopass" has a stored password.
cat > "${STUB_DIR}/mc" << 'EOF'
#!/usr/bin/env bash
case "$*" in
    *"/agents/nopass/"*) exit 1 ;;
    "cat "*) echo "secret" ;;
esac
EOF
# curl: login always succeeds; joining fails for the worker named in JOIN_FAIL.
cat > "${STUB_DIR}/curl" << 'EOF'
#!/usr/bin/env bash
args="$*"
case "${args}" in
    *"/login"*) echo '{"access_token":"tok"}' ;;
    *"/join"*)
        if [ -n "${JOIN_FAIL:-}" ] && [ "${LAST_WORKER:-}" = "${JOIN_FAIL}" ]; then exit 22; fi
        echo '{}' ;;
esac
EOF
chmod +x "${STUB_DIR}/agt" "${STUB_DIR}/mc" "${STUB_DIR}/curl"
export PATH="${STUB_DIR}:${PATH}"
export AGENTTEAMS_MATRIX_URL="http://matrix.test" AGENTTEAMS_STORAGE_PREFIX="minio/agentteams"
log() { :; }

# shellcheck source=../manager/agent/skills/project-management/scripts/project-members.sh
source "${HELPERS}"

# 1. A Team expands to its leader and then all its workers.
[ "$(project_team_roster alpha | paste -sd, -)" = "lead,w1,w2" ] ||
    fail "--team alpha must expand to lead,w1,w2"
[ "$(project_team_roster solo | paste -sd, -)" = "lead" ] ||
    fail "a Team with no workers must expand to its leader"

# 2. Lookup failures stop with a message instead of an empty member list.
if out=$(project_team_roster missing 2>&1); then fail "an unknown Team must fail"; fi
grep -q "Team missing not found" <<< "${out}" || fail "an unknown Team must be named in the error: ${out}"
if out=$(project_team_roster empty 2>&1); then fail "a Team naming nobody must fail"; fi
grep -q "has no leader or workers" <<< "${out}" || fail "an empty Team must say so: ${out}"

# 3. Each member once, in first-seen order; spaces and empty entries dropped.
[ "$(project_merge_members "lead,w1,w2" "w2, w3,lead,,")" = "lead,w1,w2,w3" ] ||
    fail "the union of --team and --workers must list each member once"
[ "$(project_merge_members "w1,w1, w1")" = "w1" ] || fail "a repeated --workers entry must be invited once"

# 4. --workers alone keeps its meaning: a leader passed alone is just the leader.
[ "$(project_merge_members "lead")" = "lead" ] || fail "--workers lead must stay lead"
grep -q -- '--team)        TEAM_NAME="$2"' "${SCRIPT}" || fail "create-project.sh must parse --team"
grep -q 'if \[ -n "${TEAM_NAME}" \]; then' "${SCRIPT}" ||
    fail "the Team lookup must run only when --team is given"

# 5. A join is reported only when it happened.
project_worker_auto_join w1 '!room:test' || fail "a worker that joins must report success"
if LAST_WORKER=w2 JOIN_FAIL=w2 project_worker_auto_join w2 '!room:test'; then
    fail "a failed join must not count as membership"
fi
if project_worker_auto_join nopass '!room:test'; then
    fail "a worker with no credentials was only invited, not joined"
fi

# 6. create-project.sh reports the joins in its result, and documents --team.
grep -q 'source "${SCRIPT_DIR}/project-members.sh"' "${SCRIPT}" ||
    fail "create-project.sh must source project-members.sh"
grep -q 'join_failed: (\$join_failed' "${SCRIPT}" ||
    fail "the result JSON must list the workers that did not join"
! grep -q '^_worker_auto_join()' "${SCRIPT}" || fail "the join must go through project_worker_auto_join"
grep -Fq -- '--team' "${REFERENCE}" || fail "create-project.md must document --team for the Manager"
grep -Fq -- 'join_failed' "${REFERENCE}" || fail "create-project.md must say what join_failed means"

echo "PASS: create-project.sh --team expands the roster once and reports lookup and join failures"
