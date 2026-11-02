#!/usr/bin/env bash
# Regression for #1390: create-project.sh must surface the per-worker
# auto-join outcome (joined / pending / failed) instead of returning 0
# silently for both "no credentials" and "join HTTP error".
#
# Structural assertions pin the contract; the functional harness extracts
# the helper functions and drives them with stubs for mc/curl, covering
# the two reported branches plus the success control.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="${ROOT_DIR}/manager/agent/skills/project-management/scripts/create-project.sh"
REFERENCE="${ROOT_DIR}/manager/agent/skills/project-management/references/create-project.md"

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

[ -f "${SCRIPT}" ] || fail "create-project.sh not found at ${SCRIPT}"
[ -f "${REFERENCE}" ] || fail "create-project.md reference not found at ${REFERENCE}"

# --- structural contract ---------------------------------------------------
grep -Fq '_record_worker_join' "${SCRIPT}" ||
    fail "the join outcome must be recorded per worker (_record_worker_join)"
grep -Fq '_worker_join_summary' "${SCRIPT}" ||
    fail "the caller must be able to read a joined/pending/failed summary"
grep -Fq 'return 2' "${SCRIPT}" ||
    fail "a pending join (no credentials/token) must return a distinct code (2)"
grep -Fq 'return 3' "${SCRIPT}" ||
    fail "a failed join must return a distinct code (3)"
grep -Fq 'http-${join_http' "${SCRIPT}" ||
    fail "the join failure detail must carry the HTTP status"
grep -Fq '_write_worker_join_status' "${SCRIPT}" ||
    fail "the join outcome must be written to a status file"
grep -Fq 'AGENTTEAMS_JOIN_STATUS_FILE' "${SCRIPT}" ||
    fail "the status file path must be overridable (AGENTTEAMS_JOIN_STATUS_FILE)"
grep -Fq 'set -e' "${SCRIPT}" ||
    fail "create-project.sh is expected to run with set -e (the join call must be guarded)"
grep -Fq '_worker_auto_join "${worker}" "${ROOM_ID}" || true' "${SCRIPT}" ||
    fail "the join loop must tolerate a pending/failed join under set -e"
grep -Fq 'Worker auto-join: $(_worker_join_summary)' "${SCRIPT}" ||
    fail "the script must log the aggregated join summary"
grep -qi 'join status' "${REFERENCE}" ||
    fail "create-project.md must document the join outcome surface"

# --- functional harness ----------------------------------------------------
WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

awk '/^WORKER_JOIN_STATUS_ROWS=/{f=1}
     f && /^# Builds the project-room member list/{exit}
     f' "${SCRIPT}" >"${WORK}/funcs.sh"

[ -s "${WORK}/funcs.sh" ] || fail "could not extract the join helpers"

# shellcheck disable=SC1090
. "${WORK}/funcs.sh"

log() { printf '%s\n' "$*" >>"${WORK}/log.txt"; }
AGENTTEAMS_MATRIX_URL="https://mx.test"
AGENTTEAMS_STORAGE_PREFIX="s3://bucket"
ROOM_ID="!room:test"

reset_ledger() {
    WORKER_JOIN_STATUS_ROWS=""
}

# A) no credentials anywhere -> pending, rc=2
mc() { return 1; }
reset_ledger
rc=0
_worker_auto_join "w-nocreds" "${ROOM_ID}" || rc=$?
[ "${rc}" -eq 2 ] || fail "A: pending join must return 2 (got ${rc})"
[ "$(_worker_join_summary)" = "joined=0 pending=1 failed=0" ] ||
    fail "A: summary mismatch: $(_worker_join_summary)"
grep -q 'pending' "${WORK}/log.txt" 2>/dev/null || true

# B) password present, join answers 403 -> failed, rc=3
mc() { echo "worker-pass"; }
curl() {
    case "$*" in
        *"/login"*)
            printf '%s' '{"access_token":"tok-123"}'
            return 0
            ;;
        *"/join"*)
            printf '%s' '403'
            return 0
            ;;
    esac
    return 1
}
reset_ledger
rc=0
_worker_auto_join "w-403" "${ROOM_ID}" || rc=$?
[ "${rc}" -eq 3 ] || fail "B: a failed join must return 3 (got ${rc})"
[ "$(_worker_join_summary)" = "joined=0 pending=0 failed=1" ] ||
    fail "B: summary mismatch: $(_worker_join_summary)"

# C) success control -> joined, rc=0
curl() {
    case "$*" in
        *"/login"*)
            printf '%s' '{"access_token":"tok-123"}'
            return 0
            ;;
        *"/join"*)
            printf '%s' '200'
            return 0
            ;;
    esac
    return 1
}
reset_ledger
rc=0
_worker_auto_join "w-ok" "${ROOM_ID}" || rc=$?
[ "${rc}" -eq 0 ] || fail "C: a successful join must return 0 (got ${rc})"
[ "$(_worker_join_summary)" = "joined=1 pending=0 failed=0" ] ||
    fail "C: summary mismatch: $(_worker_join_summary)"

# D) status file is valid JSON with per-worker rows
OUT="${WORK}/join-status.json"
_write_worker_join_status "${OUT}"
[ -f "${OUT}" ] || fail "D: status file was not written"
jq -e '.room_id == "!room:test"' "${OUT}" >/dev/null ||
    fail "D: status file must carry the room id"
jq -e '.workers | length == 1' "${OUT}" >/dev/null ||
    fail "D: status file must carry one row per worker"
jq -e '.workers[0].status == "joined"' "${OUT}" >/dev/null ||
    fail "D: status file must carry the per-worker status"

echo "PASS: create-project.sh join outcome is surfaced (pending/failed/joined + status file)"
