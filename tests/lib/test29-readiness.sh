#!/usr/bin/env bash
# Readiness gate for test-29: wait until the probes answer AND the worker's
# roomID has stopped moving.
#
# Why this is a library: the gate used to live inline in
# tests/test-29-legacy-copaw-upgrade.sh, where the only way to exercise it was
# to run a real cluster. The stale-stability bug (a room change was cached as
# "stable" from an earlier poll) could not be reproduced deterministically.
# Callers: tests/test-29-legacy-copaw-upgrade.sh, tests/check-test29-readiness.sh
#
# The caller provides:
#   check_copaw_worker_probes <worker> ready <timeout>  -> 0 when the probes answer
#   get_worker_room_id <worker>                         -> the current roomID (may be empty)
#   log_info / log_pass / log_fail
#   READINESS_TIMEOUT                                   -> seconds
#
# Returns 0 when ready, 1 on timeout (callers decide what to do).
test29_wait_worker_ready() {
    local worker="$1"
    local timeout="${2:-${READINESS_TIMEOUT:-240}}"
    local deadline probes_ready="" probe_last="" prev_room="" stable_room="" current_room=""

    deadline=$(( $(date +%s) + timeout ))
    while [ "$(date +%s)" -lt "${deadline}" ]; do
        if [ -z "${probes_ready}" ] && probe_last=$(check_copaw_worker_probes "${worker}" ready 15 2>&1); then
            probes_ready="ready"
        fi
        current_room=$(get_worker_room_id "${worker}")
        if [ -n "${current_room}" ] && [ "${current_room}" = "${prev_room}" ]; then
            stable_room="${current_room}"
        elif [ "${stable_room}" != "${current_room}" ]; then
            # The room moved (or went empty): whatever we cached earlier is
            # stale. Without this the loop could exit on a poll that observed a
            # brand-new room twice only if it happened to repeat - in the
            # A,A,B,B case it exited at poll 3 reporting stable A / current B.
            stable_room=""
        fi
        prev_room="${current_room}"
        if [ -n "${probes_ready}" ] && [ -n "${stable_room}" ] && [ "${stable_room}" = "${current_room}" ]; then
            TEST29_READY_ROOM="${stable_room}"
            TEST29_PROBE_LAST="${probe_last}"
            return 0
        fi
        sleep "${TEST29_READINESS_POLL_INTERVAL:-5}"
    done

    TEST29_READY_ROOM=""
    TEST29_PROBE_LAST="${probe_last}"
    TEST29_LAST_ROOM="${prev_room}"
    return 1
}
