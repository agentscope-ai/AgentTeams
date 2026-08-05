#!/usr/bin/env bash
# Verifies the Manager runtime-switch wrapper carries the safe DeepAgents
# sandbox policy through to the agt CLI without requiring a deployed cluster.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script="${repo_root}/manager/agent/skills/worker-management/scripts/update-worker-config.sh"

output="$(bash -c '
source() { :; }
agentteams() { printf "CLI:%s\n" "$*"; }
agt() {
    if [ "$1" = "get" ]; then
        printf "%s\n" "{\"workers\":[{\"name\":\"deep-worker\",\"phase\":\"Running\",\"runtime\":\"deepagents\"}]}"
    fi
}
export -f source agentteams agt
bash "$1" --name deep-worker --runtime deepagents --deepagents-coordinators @human:example.org
' _ "${script}")"

case "${output}" in
    *"agentteams update worker --name deep-worker --runtime deepagents --deepagents-sandbox --deepagents-coordinators @human:example.org"*) ;;
    *)
        echo "DeepAgents runtime switch did not pass sandbox approval flags to agt:" >&2
        echo "${output}" >&2
        exit 1
        ;;
esac

if error_output="$(bash -c '
source() { :; }
export -f source
bash "$1" --name regular-worker --runtime openclaw --deepagents-coordinators @human:example.org
' _ "${script}" 2>&1)"; then
    echo "non-DeepAgents runtime accepted --deepagents-coordinators" >&2
    exit 1
fi
if ! grep -Fq -- "--deepagents-coordinators is only valid with --runtime deepagents" <<<"${error_output}"; then
    echo "non-DeepAgents coordinator rejection was not actionable" >&2
    printf '%s\n' "${error_output}" >&2
    exit 1
fi

echo "worker-management DeepAgents runtime-switch checks passed"
