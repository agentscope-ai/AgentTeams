#!/usr/bin/env bash
# QwenPaw worker storage sync integration test against real MinIO.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/qwenpaw-image-e2e.sh"

qwenpaw_e2e_require_enabled
qwenpaw_e2e_require_docker
qwenpaw_e2e_init "sync-minio"
qwenpaw_e2e_build_or_use_image

qwenpaw_e2e_write_runtime_yaml "1"
mkdir -p \
    "${QWENPAW_E2E_TMP_DIR}/seed/agents/${QWENPAW_E2E_WORKER_NAME}/credentials" \
    "${QWENPAW_E2E_TMP_DIR}/seed/shared"
cat >"${QWENPAW_E2E_TMP_DIR}/seed/agents/${QWENPAW_E2E_WORKER_NAME}/remote-start.txt" <<EOF
from-storage
EOF
cat >"${QWENPAW_E2E_TMP_DIR}/seed/agents/${QWENPAW_E2E_WORKER_NAME}/credentials/bootstrap-token" <<EOF
must-not-mirror
EOF
cat >"${QWENPAW_E2E_TMP_DIR}/seed/shared/team-note.md" <<EOF
# Shared Context

Mirrored at worker startup.
EOF

qwenpaw_e2e_create_network
qwenpaw_e2e_start_minio
qwenpaw_e2e_wait_for_minio
qwenpaw_e2e_seed_storage
qwenpaw_e2e_start_worker
qwenpaw_e2e_wait_worker_http /api/version 240

qwenpaw_e2e_exec sh -lc '
    grep -q "from-storage" "$HOME/remote-start.txt"
    test -f "/root/agentteams-fs/shared/team-note.md"
    test -L "$HOME/.qwenpaw/workspaces/default/shared"
    python -c '"'"'from pathlib import Path; import os; p = Path(os.environ["HOME"]) / ".qwenpaw/workspaces/default/shared"; assert p.resolve() == Path("/root/agentteams-fs/shared").resolve()'"'"'
    test ! -e "$HOME/credentials/bootstrap-token"
'

qwenpaw_e2e_exec sh -lc '
    mkdir -p \
        "$HOME/credentials" \
        "$HOME/.qwenpaw/workspaces/default/tool_result" \
        "$HOME/.qwenpaw/workspaces/default/file_store" \
        "$HOME/.qwenpaw/workspaces/default/shared/tasks/t-2/workspace" \
        "$HOME/shared/tasks/t-1"
    printf "runtime upload\n" > "$HOME/runtime-note.md"
    printf "workspace upload\n" > "$HOME/.qwenpaw/workspaces/default/integration-sync.md"
    printf "secret\n" > "$HOME/credentials/local-token"
    printf "log\n" > "$HOME/.qwenpaw/qwenpaw.log"
    printf "{}\n" > "$HOME/.qwenpaw/workspaces/default/tool_result/result.json"
    printf "file\n" > "$HOME/.qwenpaw/workspaces/default/file_store/a.txt"
    printf "workspace shared\n" > "$HOME/.qwenpaw/workspaces/default/shared/tasks/t-2/workspace/result.md"
    grep -q "workspace shared" "/root/agentteams-fs/shared/tasks/t-2/workspace/result.md"
    printf "team shared\n" > "$HOME/shared/tasks/t-1/result.md"
'

wait_for_object() {
    local key="$1"
    local expected="$2"
    local i
    for i in $(seq 1 30); do
        if qwenpaw_e2e_run_mc "/usr/local/bin/mc.bin cat 'local/${QWENPAW_E2E_BUCKET}/${key}' 2>/dev/null" | grep -q "${expected}"; then
            return 0
        fi
        sleep 2
    done
    qwenpaw_e2e_fail "object was not pushed: ${key}"
}

assert_missing_object() {
    local key="$1"
    if qwenpaw_e2e_run_mc "/usr/local/bin/mc.bin stat 'local/${QWENPAW_E2E_BUCKET}/${key}' >/dev/null 2>&1"; then
        qwenpaw_e2e_fail "excluded object was pushed: ${key}"
    fi
}

wait_for_object "agents/${QWENPAW_E2E_WORKER_NAME}/runtime-note.md" "runtime upload"
wait_for_object "agents/${QWENPAW_E2E_WORKER_NAME}/.qwenpaw/workspaces/default/integration-sync.md" "workspace upload"

assert_missing_object "agents/${QWENPAW_E2E_WORKER_NAME}/credentials/local-token"
assert_missing_object "agents/${QWENPAW_E2E_WORKER_NAME}/.qwenpaw/qwenpaw.log"
assert_missing_object "agents/${QWENPAW_E2E_WORKER_NAME}/.qwenpaw/workspaces/default/tool_result/result.json"
assert_missing_object "agents/${QWENPAW_E2E_WORKER_NAME}/.qwenpaw/workspaces/default/file_store/a.txt"
assert_missing_object "agents/${QWENPAW_E2E_WORKER_NAME}/shared/tasks/t-1/result.md"

qwenpaw_e2e_log "PASS: startup mirror and background push boundaries verified"
