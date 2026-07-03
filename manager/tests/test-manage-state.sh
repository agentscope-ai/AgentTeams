#!/bin/bash
# test-manage-state.sh
# End-to-end tests for manage-state.sh — drives a temp state.json through every action.
#
# Usage: bash manager/tests/test-manage-state.sh

set -uo pipefail

PASS=0
FAIL=0
TMPDIR_ROOT=$(mktemp -d)
trap 'rm -rf "${TMPDIR_ROOT}"' EXIT

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
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

assert_contains() {
    local desc="$1" needle="$2" haystack="$3"
    if printf '%s' "${haystack}" | grep -qF -- "${needle}"; then
        pass "${desc}"
    else
        fail "${desc}" "contains '${needle}'" "not found: ${haystack}"
    fi
}

assert_not_contains() {
    local desc="$1" needle="$2" haystack="$3"
    if ! printf '%s' "${haystack}" | grep -qF -- "${needle}"; then
        pass "${desc}"
    else
        fail "${desc}" "should NOT contain '${needle}'" "found it"
    fi
}

# Each test case gets its own fake $HOME so state.json is isolated and
# manage-state.sh's `${HOME}/state.json` resolution works unmodified.
new_home() {
    mktemp -d "${TMPDIR_ROOT}/home-XXXXXX"
}

run_state() {
    # run_state <fake_home> <args...>
    local home="$1"; shift
    HOME="${home}" bash "${STATE_SCRIPT}" "$@"
}

state_json() {
    local home="$1"
    cat "${home}/state.json"
}

jqv() {
    # jqv <fake_home> <jq_filter>
    local home="$1" filter="$2"
    jq -r "${filter}" "${home}/state.json"
}

# ── Tests ─────────────────────────────────────────────────────────────────────

echo ""
echo "=== TC1: init — creates state.json with expected shape ==="
{
    h=$(new_home)
    out=$(run_state "${h}" --action init)
    assert_contains "reports OK" "OK: state.json ready" "${out}"
    assert_eq "admin_dm_room_id null" "null" "$(jqv "${h}" '.admin_dm_room_id')"
    assert_eq "active_tasks empty" "0" "$(jqv "${h}" '.active_tasks | length')"
    assert_eq "cancelled_tasks empty" "0" "$(jqv "${h}" '.cancelled_tasks | length')"
    assert_eq "last_digest_sent_at null" "null" "$(jqv "${h}" '.last_digest_sent_at')"
}

echo ""
echo "=== TC2: add-finite — adds a task ==="
{
    h=$(new_home)
    out=$(run_state "${h}" --action add-finite --task-id T1 --title "Do the thing" --assigned-to worker1 --room-id '!room1:x')
    assert_contains "reports OK" "OK: added finite task T1" "${out}"
    assert_eq "task count 1" "1" "$(jqv "${h}" '.active_tasks | length')"
    assert_eq "task_id" "T1" "$(jqv "${h}" '.active_tasks[0].task_id')"
    assert_eq "type finite" "finite" "$(jqv "${h}" '.active_tasks[0].type')"
    assert_eq "assigned_to" "worker1" "$(jqv "${h}" '.active_tasks[0].assigned_to')"
}

echo ""
echo "=== TC3: add-finite with optional project-room-id and delegated-to-team ==="
{
    h=$(new_home)
    run_state "${h}" --action add-finite --task-id T1 --title "T" --assigned-to w1 --room-id r1 \
        --project-room-id proj1 --delegated-to-team teamA > /dev/null
    assert_eq "project_room_id" "proj1" "$(jqv "${h}" '.active_tasks[0].project_room_id')"
    assert_eq "delegated_to_team" "teamA" "$(jqv "${h}" '.active_tasks[0].delegated_to_team')"
}

echo ""
echo "=== TC4: add-finite id collision — IDENTICAL call (same id+title+assignee) SKIPs ==="
{
    h=$(new_home)
    run_state "${h}" --action add-finite --task-id T1 --title "Same" --assigned-to w1 --room-id r1 > /dev/null
    out=$(run_state "${h}" --action add-finite --task-id T1 --title "Same" --assigned-to w1 --room-id r1)
    assert_contains "reports SKIP" "SKIP: task T1 already in active_tasks" "${out}"
    assert_eq "still only 1 task" "1" "$(jqv "${h}" '.active_tasks | length')"
}

echo ""
echo "=== TC5: add-finite id collision — same id, DIFFERENT title/assignee suffixes id ==="
{
    h=$(new_home)
    run_state "${h}" --action add-finite --task-id T1 --title "Original" --assigned-to w1 --room-id r1 > /dev/null
    out=$(run_state "${h}" --action add-finite --task-id T1 --title "Different title" --assigned-to w2 --room-id r2)
    assert_contains "reports OK with suffixed id" "OK: added finite task T1-2" "${out}"
    assert_eq "now 2 tasks" "2" "$(jqv "${h}" '.active_tasks | length')"
    assert_eq "original still T1" "T1" "$(jqv "${h}" '.active_tasks[0].task_id')"
    assert_eq "new one is T1-2" "T1-2" "$(jqv "${h}" '.active_tasks[1].task_id')"
    assert_eq "new one keeps its own title" "Different title" "$(jqv "${h}" '.active_tasks[1].title')"

    # A third collision (same id T1, yet another different assignee) should suffix to -3.
    out2=$(run_state "${h}" --action add-finite --task-id T1 --title "Third" --assigned-to w3 --room-id r3)
    assert_contains "reports OK with -3 suffix" "OK: added finite task T1-3" "${out2}"
    assert_eq "now 3 tasks" "3" "$(jqv "${h}" '.active_tasks | length')"
}

echo ""
echo "=== TC6: add-infinite — adds a recurring task ==="
{
    h=$(new_home)
    out=$(run_state "${h}" --action add-infinite --task-id I1 --title "Recurring" --assigned-to w1 --room-id r1 \
        --schedule "0 9 * * *" --timezone "UTC" --next-scheduled-at "2026-07-03T09:00:00Z")
    assert_contains "reports OK" "OK: added infinite task I1" "${out}"
    assert_eq "type infinite" "infinite" "$(jqv "${h}" '.active_tasks[0].type')"
    assert_eq "schedule" "0 9 * * *" "$(jqv "${h}" '.active_tasks[0].schedule')"
    assert_eq "next_scheduled_at" "2026-07-03T09:00:00Z" "$(jqv "${h}" '.active_tasks[0].next_scheduled_at')"
    assert_eq "last_executed_at null" "null" "$(jqv "${h}" '.active_tasks[0].last_executed_at')"
}

echo ""
echo "=== TC7: add-infinite id collision — identical call SKIPs (legacy behavior preserved) ==="
{
    h=$(new_home)
    run_state "${h}" --action add-infinite --task-id I1 --title "R" --assigned-to w1 --room-id r1 \
        --schedule "0 9 * * *" --timezone "UTC" --next-scheduled-at "2026-07-03T09:00:00Z" > /dev/null
    out=$(run_state "${h}" --action add-infinite --task-id I1 --title "R" --assigned-to w1 --room-id r1 \
        --schedule "0 9 * * *" --timezone "UTC" --next-scheduled-at "2026-07-03T09:00:00Z")
    assert_contains "reports SKIP" "SKIP: task I1 already in active_tasks" "${out}"
    assert_eq "still only 1 task" "1" "$(jqv "${h}" '.active_tasks | length')"
}

echo ""
echo "=== TC8: complete — removes a finite task ==="
{
    h=$(new_home)
    run_state "${h}" --action add-finite --task-id T1 --title "T" --assigned-to w1 --room-id r1 > /dev/null
    out=$(run_state "${h}" --action complete --task-id T1)
    assert_contains "reports OK" "OK: removed task T1" "${out}"
    assert_eq "task removed" "0" "$(jqv "${h}" '.active_tasks | length')"
}

echo ""
echo "=== TC9: complete — unknown task SKIPs ==="
{
    h=$(new_home)
    run_state "${h}" --action init > /dev/null
    out=$(run_state "${h}" --action complete --task-id NOPE)
    assert_contains "reports SKIP" "SKIP: task NOPE not found" "${out}"
}

echo ""
echo "=== TC10: executed — updates infinite task's next_scheduled_at/last_executed_at ==="
{
    h=$(new_home)
    run_state "${h}" --action add-infinite --task-id I1 --title "R" --assigned-to w1 --room-id r1 \
        --schedule "0 9 * * *" --timezone "UTC" --next-scheduled-at "2026-07-03T09:00:00Z" > /dev/null
    out=$(run_state "${h}" --action executed --task-id I1 --next-scheduled-at "2026-07-04T09:00:00Z")
    assert_contains "reports OK" "OK: updated infinite task I1" "${out}"
    assert_eq "next_scheduled_at updated" "2026-07-04T09:00:00Z" "$(jqv "${h}" '.active_tasks[0].next_scheduled_at')"
    assert_not_contains "last_executed_at no longer null" "null" "$(jqv "${h}" '.active_tasks[0].last_executed_at')"
}

echo ""
echo "=== TC11: executed — unknown infinite task WARNs, does not fail ==="
{
    h=$(new_home)
    run_state "${h}" --action init > /dev/null
    out=$(run_state "${h}" --action executed --task-id NOPE --next-scheduled-at "2026-07-04T09:00:00Z")
    assert_contains "reports WARN" "WARN: infinite task NOPE not found" "${out}"
}

echo ""
echo "=== TC12: set-admin-dm — stores admin DM room id ==="
{
    h=$(new_home)
    out=$(run_state "${h}" --action set-admin-dm --room-id '!admin:x')
    assert_contains "reports OK" "OK: admin_dm_room_id set to !admin:x" "${out}"
    assert_eq "admin_dm_room_id stored" "!admin:x" "$(jqv "${h}" '.admin_dm_room_id')"
}

echo ""
echo "=== TC13: list — shows tasks, admin DM room, and total count ==="
{
    h=$(new_home)
    run_state "${h}" --action set-admin-dm --room-id '!admin:x' > /dev/null
    run_state "${h}" --action add-finite --task-id T1 --title "Alpha" --assigned-to w1 --room-id r1 > /dev/null
    run_state "${h}" --action add-finite --task-id T2 --title "Beta" --assigned-to w2 --room-id r2 > /dev/null
    out=$(run_state "${h}" --action list)
    assert_contains "shows admin dm room" "Admin DM room: !admin:x" "${out}"
    assert_contains "shows T1" "T1" "${out}"
    assert_contains "shows T2" "T2" "${out}"
    assert_contains "shows total 2" "Total: 2 active task(s)" "${out}"
}

echo ""
echo "=== TC14: list — no active tasks ==="
{
    h=$(new_home)
    run_state "${h}" --action init > /dev/null
    out=$(run_state "${h}" --action list)
    assert_contains "no active tasks message" "No active tasks." "${out}"
}

echo ""
echo "=== TC15: mark-blocked — sets status + blocked_since, list prefixes [BLOCKED since ...] ==="
{
    h=$(new_home)
    run_state "${h}" --action add-finite --task-id T1 --title "Alpha" --assigned-to w1 --room-id r1 > /dev/null
    out=$(run_state "${h}" --action mark-blocked --task-id T1 --reason "waiting on upstream")
    assert_contains "reports OK" "OK: task T1 marked blocked" "${out}"
    assert_eq "status blocked" "blocked" "$(jqv "${h}" '.active_tasks[0].status')"
    assert_not_contains "blocked_since is set (not null)" "null" "$(jqv "${h}" '.active_tasks[0].blocked_since')"
    assert_eq "blocked_reason stored" "waiting on upstream" "$(jqv "${h}" '.active_tasks[0].blocked_reason')"

    list_out=$(run_state "${h}" --action list)
    assert_contains "list shows BLOCKED prefix" "[BLOCKED since" "${list_out}"
    assert_contains "list still shows task id" "T1" "${list_out}"
}

echo ""
echo "=== TC16: mark-blocked — unknown task SKIPs ==="
{
    h=$(new_home)
    run_state "${h}" --action init > /dev/null
    out=$(run_state "${h}" --action mark-blocked --task-id NOPE --reason "x")
    assert_contains "reports SKIP" "SKIP: task NOPE not found" "${out}"
}

echo ""
echo "=== TC17: unblock — clears blocked status + blocked_since ==="
{
    h=$(new_home)
    run_state "${h}" --action add-finite --task-id T1 --title "Alpha" --assigned-to w1 --room-id r1 > /dev/null
    run_state "${h}" --action mark-blocked --task-id T1 --reason "x" > /dev/null
    out=$(run_state "${h}" --action unblock --task-id T1)
    assert_contains "reports OK" "OK: task T1 unblocked" "${out}"
    assert_eq "status field removed" "null" "$(jqv "${h}" '.active_tasks[0].status // "null"')"
    assert_eq "blocked_since field removed" "null" "$(jqv "${h}" '.active_tasks[0].blocked_since // "null"')"
    assert_eq "blocked_reason field removed" "null" "$(jqv "${h}" '.active_tasks[0].blocked_reason // "null"')"

    list_out=$(run_state "${h}" --action list)
    assert_not_contains "list no longer shows BLOCKED prefix" "[BLOCKED since" "${list_out}"
}

echo ""
echo "=== TC18: cancel — removes task, records reason in cancelled_tasks ==="
{
    h=$(new_home)
    run_state "${h}" --action add-finite --task-id T1 --title "Alpha" --assigned-to w1 --room-id r1 > /dev/null
    out=$(run_state "${h}" --action cancel --task-id T1 --reason "no longer needed")
    assert_contains "reports OK" "OK: cancelled task T1" "${out}"
    assert_eq "removed from active_tasks" "0" "$(jqv "${h}" '.active_tasks | length')"
    assert_eq "recorded in cancelled_tasks" "1" "$(jqv "${h}" '.cancelled_tasks | length')"
    assert_eq "cancelled task_id preserved" "T1" "$(jqv "${h}" '.cancelled_tasks[0].task_id')"
    assert_eq "cancel_reason recorded" "no longer needed" "$(jqv "${h}" '.cancelled_tasks[0].cancel_reason')"
    assert_not_contains "cancelled_at is set" "null" "$(jqv "${h}" '.cancelled_tasks[0].cancelled_at')"
}

echo ""
echo "=== TC19: cancel — unknown task SKIPs ==="
{
    h=$(new_home)
    run_state "${h}" --action init > /dev/null
    out=$(run_state "${h}" --action cancel --task-id NOPE --reason "x")
    assert_contains "reports SKIP" "SKIP: task NOPE not found" "${out}"
    assert_eq "cancelled_tasks unaffected" "0" "$(jqv "${h}" '.cancelled_tasks | length')"
}

echo ""
echo "=== TC20: reassign — swaps assigned_to and room_id ==="
{
    h=$(new_home)
    run_state "${h}" --action add-finite --task-id T1 --title "Alpha" --assigned-to w1 --room-id r1 > /dev/null
    out=$(run_state "${h}" --action reassign --task-id T1 --assigned-to w2 --room-id r2)
    assert_contains "reports OK" "OK: reassigned task T1 to w2" "${out}"
    assert_eq "assigned_to updated" "w2" "$(jqv "${h}" '.active_tasks[0].assigned_to')"
    assert_eq "room_id updated" "r2" "$(jqv "${h}" '.active_tasks[0].room_id')"
    assert_eq "title unchanged" "Alpha" "$(jqv "${h}" '.active_tasks[0].title')"
}

echo ""
echo "=== TC21: reassign — unknown task SKIPs ==="
{
    h=$(new_home)
    run_state "${h}" --action init > /dev/null
    out=$(run_state "${h}" --action reassign --task-id NOPE --assigned-to w2 --room-id r2)
    assert_contains "reports SKIP" "SKIP: task NOPE not found" "${out}"
}

echo ""
echo "=== TC22: last-digest get — defaults to null on fresh state ==="
{
    h=$(new_home)
    run_state "${h}" --action init > /dev/null
    out=$(run_state "${h}" --action last-digest get)
    assert_eq "prints null" "null" "${out}"
}

echo ""
echo "=== TC23: last-digest set/get — round-trips a timestamp ==="
{
    h=$(new_home)
    run_state "${h}" --action init > /dev/null
    set_out=$(run_state "${h}" --action last-digest set --at "2026-07-02T08:00:00Z")
    assert_contains "set reports OK" "OK: last_digest_sent_at set to 2026-07-02T08:00:00Z" "${set_out}"
    get_out=$(run_state "${h}" --action last-digest get)
    assert_eq "get returns the timestamp" "2026-07-02T08:00:00Z" "${get_out}"
    assert_eq "state.json field matches" "2026-07-02T08:00:00Z" "$(jqv "${h}" '.last_digest_sent_at')"
}

echo ""
echo "=== TC24: last-digest — positional 'get'/'set <ts>' form (plan-specified) also works ==="
{
    h=$(new_home)
    run_state "${h}" --action init > /dev/null
    run_state "${h}" --action last-digest set "2026-07-01T00:00:00Z" > /dev/null
    out=$(run_state "${h}" --action last-digest get)
    assert_eq "positional set/get round-trips" "2026-07-01T00:00:00Z" "${out}"
}

echo ""
echo "=== TC25: backward compatibility — pre-existing state.json (no new fields) is backfilled ==="
{
    h=$(new_home)
    cat > "${h}/state.json" << 'EOF'
{
  "active_tasks": [],
  "updated_at": "2020-01-01T00:00:00Z"
}
EOF
    out=$(run_state "${h}" --action list)
    assert_contains "list still works on legacy file" "Admin DM room: null" "${out}"
    assert_eq "admin_dm_room_id backfilled" "null" "$(jqv "${h}" '.admin_dm_room_id')"
    assert_eq "cancelled_tasks backfilled" "0" "$(jqv "${h}" '.cancelled_tasks | length')"
    assert_eq "last_digest_sent_at backfilled" "null" "$(jqv "${h}" '.last_digest_sent_at')"
}

echo ""
echo "=== TC26: legacy 'add' action still dispatches to add-finite/add-infinite ==="
{
    h=$(new_home)
    run_state "${h}" --action add --task-id T1 --title "Legacy" --assigned-to w1 --room-id r1 > /dev/null
    assert_eq "legacy add defaults to finite" "finite" "$(jqv "${h}" '.active_tasks[0].type')"

    h2=$(new_home)
    run_state "${h2}" --action add --type infinite --task-id I1 --title "Legacy Infinite" --assigned-to w1 --room-id r1 \
        --schedule "0 9 * * *" --timezone UTC --next-scheduled-at "2026-07-03T09:00:00Z" > /dev/null
    assert_eq "legacy add with --type infinite" "infinite" "$(jqv "${h2}" '.active_tasks[0].type')"
}

echo ""
echo "=== TC27: missing required args errors out (non-zero exit) ==="
{
    h=$(new_home)
    set +e
    HOME="${h}" bash "${STATE_SCRIPT}" --action add-finite --task-id T1 > /dev/null 2>"${TMPDIR_ROOT}/tc27-err.txt"
    rc=$?
    set -e
    assert_eq "non-zero exit on missing args" "1" "${rc}"
}

echo ""
echo "=== TC28: unknown action errors out (non-zero exit) ==="
{
    h=$(new_home)
    set +e
    HOME="${h}" bash "${STATE_SCRIPT}" --action bogus-action > /dev/null 2>/dev/null
    rc=$?
    set -e
    assert_eq "non-zero exit on unknown action" "1" "${rc}"
}

# ── Summary ───────────────────────────────────────────────────────────────────
echo ""
echo "================================"
echo "Results: ${PASS} passed, ${FAIL} failed"
echo "================================"
[ "${FAIL}" -eq 0 ]
