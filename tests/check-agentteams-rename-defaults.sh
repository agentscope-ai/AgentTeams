#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

assert_contains() {
    local file="$1"
    local expected="$2"
    if ! grep -Fq -- "${expected}" "${ROOT_DIR}/${file}"; then
        echo "FAIL: ${file} does not contain: ${expected}" >&2
        return 1
    fi
}

assert_contains helm/hiclaw/values.yaml 'bucket: "agentteams-storage"'
assert_contains helm/hiclaw/values.yaml 'resourcePrefix: "agentteams-"'
assert_contains helm/hiclaw/templates/controller/deployment.yaml 'default "agentteams-" | quote'
assert_contains helm/hiclaw/templates/_helpers.infra.tpl 'printf "agentteams/%s"'

echo "PASS: AgentTeams Helm defaults"

OPENHUMAN_ENTRYPOINT="openhuman/scripts/openhuman-worker-entrypoint.sh"
assert_contains "${OPENHUMAN_ENTRYPOINT}" 'AGENTTEAMS_WORKER_NAME="${AGENTTEAMS_WORKER_NAME:-${HICLAW_WORKER_NAME:-}}"'
assert_contains "${OPENHUMAN_ENTRYPOINT}" 'WORKER_NAME="${AGENTTEAMS_WORKER_NAME:?AGENTTEAMS_WORKER_NAME is required}"'
assert_contains "${OPENHUMAN_ENTRYPOINT}" 'if [ "${AGENTTEAMS_RUNTIME:-}" = "aliyun" ]; then'
assert_contains "${OPENHUMAN_ENTRYPOINT}" 'mc alias set "${AGENTTEAMS_STORAGE_ALIAS}"'
assert_contains "${OPENHUMAN_ENTRYPOINT}" 'mc mirror "${AGENTTEAMS_STORAGE_PREFIX}/agents/${WORKER_NAME}/"'
assert_contains "${OPENHUMAN_ENTRYPOINT}" 'AGENTTEAMS_AI_GATEWAY_URL%/}/v1'
assert_contains "${OPENHUMAN_ENTRYPOINT}" 'if [ -n "${AGENTTEAMS_CONTROLLER_URL:-}" ]; then'
assert_contains "${OPENHUMAN_ENTRYPOINT}" 'cat ${AGENTTEAMS_AUTH_TOKEN_FILE:-/var/run/secrets/agentteams/token}'

echo "PASS: OpenHuman AgentTeams environment contract"
