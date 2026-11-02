#!/usr/bin/env bash
# Regression for the readiness gate: a room change must NOT be reported as
# stable just because an earlier room had repeated.
#
# Scenario from the review: observations A, A, B, B with the probes becoming
# ready on poll 3. The old loop cached A at poll 2, so poll 3 saw current=B
# against cached stable=A and still broke out - seeding started before B had
# ever been observed twice.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1
source tests/lib/test29-readiness.sh

FAILS=0; PASSES=0
pass(){ PASSES=$((PASSES+1)); echo "PASS: $*"; }
fail(){ FAILS=$((FAILS+1)); echo "FAIL: $*"; }

log_info(){ :; }; log_pass(){ :; }; log_fail(){ :; }
TEST29_READINESS_POLL_INTERVAL=0   # no sleeping in the check

# --- deterministic drivers -------------------------------------------------
# The gate calls both helpers in command substitutions, so anything they touch
# lives in a subshell. The poll counter therefore lives in a file.
ROOMFILE="$(mktemp)"; POLLFILE="$(mktemp)"
trap 'rm -f "${ROOMFILE}" "${POLLFILE}"' EXIT
PROBE_READY_AT=0
_seed(){ printf '%s\n' "$@" > "${ROOMFILE}"; echo 0 > "${POLLFILE}"; }
polls(){ cat "${POLLFILE}"; }
# Each iteration: probes are asked first, then the room. Both must see the same
# iteration number, so the room read is what advances it.
get_worker_room_id(){ local n; n=$(( $(cat "${POLLFILE}") + 1 )); echo "$n" > "${POLLFILE}"; sed -n "${n}p" "${ROOMFILE}"; }
check_copaw_worker_probes(){
    local n; n=$(( $(cat "${POLLFILE}") + 1 ))
    if [ "${n}" -ge "${PROBE_READY_AT}" ]; then echo "probes ok"; return 0; fi
    echo "probes not ready"; return 1
}

run_case(){ # name, expect_rc, expect_poll, expect_room, ready_at, rooms...
    local name="$1" want_rc="$2" want_poll="$3" want_room="$4" ready_at="$5"; shift 5
    _seed "$@"; PROBE_READY_AT="${ready_at}"
    test29_wait_worker_ready w 30 >/dev/null 2>&1; local rc=$?
    if [ "$rc" -ne "$want_rc" ]; then fail "$name: rc=$rc want $want_rc"; return; fi
    if [ "${want_poll}" != "-" ] && [ "$(polls)" -ne "$want_poll" ]; then
        fail "$name: exited on poll $(polls), want ${want_poll}"; return
    fi
    if [ "$want_room" = "-" ]; then
        pass "$name: rc=${rc} after poll $(polls)"
        return
    fi
    if [ "${TEST29_READY_ROOM}" != "$want_room" ]; then
        fail "$name: stable room='${TEST29_READY_ROOM}' want '$want_room'"; return
    fi
    pass "$name: poll $(polls), stable roomID=${want_room}"
}

echo "=== A. stale stability must not survive a room change ==="
run_case "A,A,B,B ready@3 waits for poll 4" 0 4 "B" 3 A A B B
run_case "A,A ready@1 exits on poll 2"      0 2 "A" 1 A A
run_case "A,A,A,B,B ready@4 exits poll 5"   0 5 "B" 4 A A A B B
run_case "A,B,A,B never settles times out"  1 - "-" 1 A B A B A
run_case "A,A,,B,B ready@4 exits poll 5"    0 5 "B" 4 A A "" B B
# control: probes ready while the FIRST room is still the current one -> exits early
run_case "A,A,,B,B ready@2 exits poll 2"    0 2 "A" 2 A A "" B B
run_case "probes never ready times out"     1 - "-" 99 A A A A

echo
if [ "${FAILS}" -eq 0 ]; then
    echo "ALL PASS: test-29 readiness gate (${PASSES} cases)"
    exit 0
fi
echo "${FAILS} FAILURES in test-29 readiness gate (${PASSES} passed)"
exit 1
