#!/usr/bin/env bash
set -eo pipefail

export TEST_CONTROLLER_CONTAINER=infra TEST_AGENT_CONTAINER=manager
export STORAGE_PREFIX=agentteams/test AGENTTEAMS_DEFAULT_WORKER_RUNTIME=hermes
source "$(cd "$(dirname "$0")" && pwd)/lib/test-helpers.sh"

# The Manager is QwenPaw while its Workers are Hermes. Its SDK has no web UI;
# the old projection deliberately contains a different, stale allow-list.
docker() { return 0; }
sleep() { :; }
exec_in_manager() { echo '{"channels":{"matrix":{"groupAllowFrom":["@stale-worker:test"]}}}'; }
exec_in_agent() {
    case "$*" in
        'printenv AGENTTEAMS_MANAGER_RUNTIME') echo qwenpaw ;;
        'agt get managers default -o json') printf '{"ready":%s}\n' "${READY}" ;;
        'curl -sf http://127.0.0.1:18799/api/access-control/agentteams_matrix')
            echo '{"whitelist":{"@leader:test":{}}}' ;;
        *) return 1 ;;
    esac
}

actual=$(read_worker_matrix_allowlist manager)
test "${actual}" = '@leader:test'
wait_agent_matrix_allow_contains manager .channels.matrix.groupAllowFrom '@leader:test' 1

READY=false
if wait_for_manager_agent_ready 1 >/dev/null 2>&1; then
    echo 'FAIL: a running process must not replace Harness readiness' >&2
    exit 1
fi
READY=true
wait_for_manager_agent_ready 1 >/dev/null
echo 'PASS: Manager readiness and ACL follow its own Harness contract'
