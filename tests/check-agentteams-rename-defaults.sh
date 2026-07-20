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

assert_not_contains() {
    local file="$1"
    local unexpected="$2"
    if grep -Fq -- "${unexpected}" "${ROOT_DIR}/${file}"; then
        echo "FAIL: ${file} still contains: ${unexpected}" >&2
        return 1
    fi
}

assert_missing_path() {
    local path="$1"
    if [ -e "${ROOT_DIR}/${path}" ]; then
        echo "FAIL: legacy path still exists: ${path}" >&2
        return 1
    fi
}

assert_contains helm/agentteams/values.yaml 'bucket: "agentteams-storage"'
assert_contains helm/agentteams/Chart.yaml 'name: agentteams'
assert_contains helm/agentteams/templates/_helpers.tpl 'define "agentteams.name"'
assert_contains helm/agentteams/templates/_helpers.tpl 'eq .Release.Name "hiclaw"'
assert_contains helm/agentteams/values.yaml 'resourcePrefix: "agentteams-"'
assert_contains helm/agentteams/templates/controller/deployment.yaml 'default "agentteams-" | quote'
assert_contains helm/agentteams/templates/_helpers.infra.tpl 'printf "agentteams/%s"'
assert_not_contains helm/agentteams/templates/_helpers.tpl 'define "hiclaw.'
assert_missing_path helm/hiclaw

echo "PASS: AgentTeams Helm defaults"

assert_contains agentteams-controller/go.mod 'module github.com/agentscope-ai/AgentTeams/agentteams-controller'
assert_not_contains agentteams-controller/go.mod 'github.com/hiclaw/hiclaw-controller'
assert_missing_path hiclaw-controller
assert_contains agentteams-controller/Dockerfile 'go build -o /agt ./cmd/agt/'
assert_contains agentteams-controller/Dockerfile 'ln -s /usr/local/bin/agt /usr/local/bin/hiclaw'
assert_contains install/agentteams-install.sh 'agentteams-install.sh - One-click installation'
assert_contains install/agentteams-install.sh 'migrate_legacy_env_file'
assert_contains install/agentteams-install.sh '${HOME}/hiclaw-manager.env'
assert_contains shared/lib/agentteams-paths.sh 'agentteams_migrate_legacy_path /root/hiclaw-fs /root/agentteams-fs'
assert_contains shared/lib/agentteams-paths.sh 'agentteams_migrate_legacy_path /data/hiclaw-secrets.env /data/agentteams-secrets.env'

echo "PASS: AgentTeams controller and installer entrypoints"

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
