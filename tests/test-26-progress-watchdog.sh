#!/bin/bash
# test-26-progress-watchdog.sh - Unit-style test for Manager task progress watchdog

set -euo pipefail

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
      "task_id": "task-002",
      "title": "Missing progress log",
      "type": "finite",
      "assigned_to": "bob",
      "room_id": "!bob:example"
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

first="$("${WATCHDOG}" --task-id task-001)"
assert_json_field "${first}" '.status' 'normal'
assert_json_field "${first}" '.stale_heartbeat_count' '0'
first_progress_at="$(jq -r '.active_tasks[] | select(.task_id == "task-001") | .last_progress_at' "${HOME}/state.json")"

sleep 1
second="$("${WATCHDOG}" --task-id task-001)"
assert_json_field "${second}" '.status' 'repeated'
assert_json_field "${second}" '.stale_heartbeat_count' '1'

count="$(jq -r '.active_tasks[] | select(.task_id == "task-001") | .stale_heartbeat_count' "${HOME}/state.json")"
if [ "${count}" != "1" ]; then
    echo "FAIL: state.json stale_heartbeat_count should be 1, got ${count}" >&2
    exit 1
fi
second_progress_at="$(jq -r '.active_tasks[] | select(.task_id == "task-001") | .last_progress_at' "${HOME}/state.json")"
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

third="$("${WATCHDOG}" --task-id task-001)"
assert_json_field "${third}" '.status' 'normal'
assert_json_field "${third}" '.stale_heartbeat_count' '0'

missing="$("${WATCHDOG}" --task-id task-002)"
assert_json_field "${missing}" '.status' 'unknown'
assert_json_field "${missing}" '.stale_heartbeat_count' '1'

blocked="$("${WATCHDOG}" --task-id task-003)"
assert_json_field "${blocked}" '.status' 'blocked'
assert_json_field "${blocked}" '.stale_heartbeat_count' '0'

long_running="$("${WATCHDOG}" --task-id task-004)"
assert_json_field "${long_running}" '.status' 'long_running'
assert_json_field "${long_running}" '.stale_heartbeat_count' '0'
