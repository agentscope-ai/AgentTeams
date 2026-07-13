#!/bin/bash
# test-26-progress-watchdog.sh - Unit-style test for Manager task progress watchdog

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
WATCHDOG="${PROJECT_ROOT}/manager/agent/skills/task-management/scripts/check-progress-watchdog.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

export HOME="${TMP_DIR}/home"
export HICLAW_FS_ROOT="${TMP_DIR}/hiclaw-fs"
mkdir -p "${HOME}" "${HICLAW_FS_ROOT}/shared/tasks/task-001/progress"
mkdir -p "${HICLAW_FS_ROOT}/shared/tasks/task-003/progress"
mkdir -p "${HICLAW_FS_ROOT}/shared/tasks/task-004/progress"

cat > "${HOME}/state.json" <<'JSON'
{
  "admin_dm_room_id": null,
  "active_tasks": [
    {
      "task_id": "task-001",
      "title": "Investigate stuck worker handling",
      "type": "finite",
      "assigned_to": "alice",
      "room_id": "!alice:example"
    },
    {
      "task_id": "task-001",
      "title": "Duplicate non-finite task should not be touched",
      "type": "infinite",
      "assigned_to": "alice",
      "room_id": "!alice:example",
      "stale_heartbeat_count": 99,
      "last_watchdog_action": "do_not_touch"
    },
    {
      "task_id": "task-002",
      "title": "Missing progress log",
      "type": "finite",
      "assigned_to": "bob",
      "room_id": "!bob:example"
    },
    {
      "task_id": "task-002",
      "title": "Duplicate missing-progress non-finite task should not be touched",
      "type": "infinite",
      "assigned_to": "bob",
      "room_id": "!bob:example",
      "stale_heartbeat_count": 42,
      "last_watchdog_action": "do_not_touch"
    },
    {
      "task_id": "task-003",
      "title": "Blocked progress log",
      "type": "finite",
      "assigned_to": "carol",
      "room_id": "!carol:example"
    },
    {
      "task_id": "task-004",
      "title": "Long running progress log",
      "type": "finite",
      "assigned_to": "dave",
      "room_id": "!dave:example"
    }
  ],
  "updated_at": "2026-06-19T00:00:00Z"
}
JSON

cat > "${HICLAW_FS_ROOT}/shared/tasks/task-001/progress/2026-06-19.md" <<'EOF_PROGRESS'
## 10:00 - Inspect state

- What was done: Read the task state file.
- Current state: Investigation started.
- Issues encountered: None.
- Next step: Inspect heartbeat.
EOF_PROGRESS

cat > "${HICLAW_FS_ROOT}/shared/tasks/task-003/progress/2026-06-19.md" <<'EOF_PROGRESS'
## 11:00 - Waiting for input

- What was done: Checked the deployment state.
- Current state: Cannot continue safely.
- Issues encountered: Blocked waiting for admin credentials.
- Next step: Ask for the missing credential.
EOF_PROGRESS

cat > "${HICLAW_FS_ROOT}/shared/tasks/task-004/progress/2026-06-19.md" <<'EOF_PROGRESS'
## 12:00 - Run full integration suite

- What was done: Started the full integration suite.
- Current state: Long-running command is still executing.
- Issues encountered: None.
- Next step: Wait for completion.
- Expected next update: 2999-01-01T00:00:00Z
EOF_PROGRESS

assert_json_field() {
    local json="$1"
    local jq_filter="$2"
    local expected="$3"
    local actual
    actual="$(printf '%s\n' "${json}" | jq -r "${jq_filter}")"
    if [ "${actual}" != "${expected}" ]; then
        echo "FAIL: expected ${jq_filter}=${expected}, got ${actual}" >&2
        echo "JSON was: ${json}" >&2
        exit 1
    fi
}

run_watchdog() {
    local task_id="$1"
    local output
    if ! output="$("${WATCHDOG}" --task-id "${task_id}")"; then
        echo "FAIL: watchdog exited non-zero for ${task_id}" >&2
        exit 1
    fi
    printf '%s\n' "${output}"
}

first="$(run_watchdog task-001)"
assert_json_field "${first}" '.status' 'normal'
assert_json_field "${first}" '.stale_heartbeat_count' '0'
first_progress_at="$(jq -r '.active_tasks[] | select(.task_id == "task-001" and .type == "finite") | .last_progress_at' "${HOME}/state.json")"

sleep 1
second="$(run_watchdog task-001)"
assert_json_field "${second}" '.status' 'repeated'
assert_json_field "${second}" '.stale_heartbeat_count' '1'

finite_count="$(jq -r '.active_tasks[] | select(.task_id == "task-001" and .type == "finite") | .stale_heartbeat_count' "${HOME}/state.json")"
duplicate_count="$(jq -r '.active_tasks[] | select(.task_id == "task-001" and .type == "infinite") | .stale_heartbeat_count' "${HOME}/state.json")"
duplicate_action="$(jq -r '.active_tasks[] | select(.task_id == "task-001" and .type == "infinite") | .last_watchdog_action' "${HOME}/state.json")"
if [ "${finite_count}" != "1" ]; then
    echo "FAIL: finite state.json stale_heartbeat_count should be 1, got ${finite_count}" >&2
    exit 1
fi
if [ "${duplicate_count}" != "99" ] || [ "${duplicate_action}" != "do_not_touch" ]; then
    echo "FAIL: duplicate non-finite task-001 entry was modified" >&2
    exit 1
fi
second_progress_at="$(jq -r '.active_tasks[] | select(.task_id == "task-001" and .type == "finite") | .last_progress_at' "${HOME}/state.json")"
if [ "${second_progress_at}" != "${first_progress_at}" ]; then
    echo "FAIL: repeated progress should not refresh last_progress_at" >&2
    exit 1
fi

cat >> "${HICLAW_FS_ROOT}/shared/tasks/task-001/progress/2026-06-19.md" <<'EOF_PROGRESS'

## 10:15 - Inspect heartbeat

- What was done: Read the heartbeat checklist.
- Current state: Found finite task follow-up flow.
- Issues encountered: None.
- Next step: Add watchdog hook.
EOF_PROGRESS

third="$(run_watchdog task-001)"
assert_json_field "${third}" '.status' 'normal'
assert_json_field "${third}" '.stale_heartbeat_count' '0'

missing="$(run_watchdog task-002)"
assert_json_field "${missing}" '.status' 'unknown'
assert_json_field "${missing}" '.stale_heartbeat_count' '1'
missing_duplicate_count="$(jq -r '.active_tasks[] | select(.task_id == "task-002" and .type == "infinite") | .stale_heartbeat_count' "${HOME}/state.json")"
missing_duplicate_action="$(jq -r '.active_tasks[] | select(.task_id == "task-002" and .type == "infinite") | .last_watchdog_action' "${HOME}/state.json")"
if [ "${missing_duplicate_count}" != "42" ] || [ "${missing_duplicate_action}" != "do_not_touch" ]; then
    echo "FAIL: duplicate non-finite task-002 entry was modified" >&2
    exit 1
fi

blocked="$(run_watchdog task-003)"
assert_json_field "${blocked}" '.status' 'blocked'
assert_json_field "${blocked}" '.stale_heartbeat_count' '0'
blocked_progress_at="$(jq -r '.active_tasks[] | select(.task_id == "task-003") | .last_progress_at' "${HOME}/state.json")"

sleep 1
blocked_again="$(run_watchdog task-003)"
assert_json_field "${blocked_again}" '.status' 'blocked'
assert_json_field "${blocked_again}" '.stale_heartbeat_count' '0'
blocked_again_progress_at="$(jq -r '.active_tasks[] | select(.task_id == "task-003") | .last_progress_at' "${HOME}/state.json")"
if [ "${blocked_again_progress_at}" != "${blocked_progress_at}" ]; then
    echo "FAIL: unchanged blocked progress should not refresh last_progress_at" >&2
    exit 1
fi

long_running="$(run_watchdog task-004)"
assert_json_field "${long_running}" '.status' 'long_running'
assert_json_field "${long_running}" '.stale_heartbeat_count' '0'
long_running_progress_at="$(jq -r '.active_tasks[] | select(.task_id == "task-004") | .last_progress_at' "${HOME}/state.json")"

sleep 1
long_running_again="$(run_watchdog task-004)"
assert_json_field "${long_running_again}" '.status' 'long_running'
assert_json_field "${long_running_again}" '.stale_heartbeat_count' '0'
long_running_again_progress_at="$(jq -r '.active_tasks[] | select(.task_id == "task-004") | .last_progress_at' "${HOME}/state.json")"
if [ "${long_running_again_progress_at}" != "${long_running_progress_at}" ]; then
    echo "FAIL: unchanged long-running progress should not refresh last_progress_at" >&2
    exit 1
fi
