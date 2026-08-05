#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHART="${ROOT_DIR}/helm/agentteams"
COMMON_ARGS=(
    --set credentials.registrationToken=test
    --set credentials.adminPassword=test
    --set credentials.llmApiKey=test
    --set gateway.publicURL=http://localhost:18080
)

render="$(mktemp)"
deepagents_render="$(mktemp)"
deepagents_special_render="$(mktemp)"
oss_render="$(mktemp)"
oss_deepagents_error="$(mktemp)"
trap 'rm -f "${render}" "${deepagents_render}" "${deepagents_special_render}" "${oss_render}" "${oss_deepagents_error}"' EXIT

helm template agentteams "${CHART}" "${COMMON_ARGS[@]}" > "${render}"

grep -q 'name: agentteams-controller' "${render}"
grep -q 'app.kubernetes.io/name: agentteams' "${render}"

helm template agentteams "${CHART}" "${COMMON_ARGS[@]}" \
    --set deepagents.enabled=true > "${deepagents_render}"

grep -q 'name: agentteams-deepagents-postgresql' "${deepagents_render}"
grep -q 'name: deepagents-checkpoint-migrate' "${deepagents_render}"
grep -q 'name: AGENTTEAMS_DEEPAGENTS_RUNNER_IMAGE' "${deepagents_render}"
grep -q 'name: AGENTTEAMS_DEEPAGENTS_STATE_PVC_SIZE' "${deepagents_render}"
grep -q 'name: AGENTTEAMS_DEEPAGENTS_CHECKPOINT_SECRET' "${deepagents_render}"

helm template agentteams "${CHART}" "${COMMON_ARGS[@]}" \
    --set deepagents.enabled=true \
    --set-string 'deepagents.postgresql.auth.username=user@name' \
    --set-string 'deepagents.postgresql.auth.password=p@ss:/?#% x' \
    --set-string 'deepagents.postgresql.auth.database=db/name' > "${deepagents_special_render}"

grep -q 'postgresql://user%40name:p%40ss%3A%2F%3F%23%25%20x@agentteams-deepagents-postgresql:5432/db%2Fname?sslmode=disable' "${deepagents_special_render}"

helm template agentteams "${CHART}" "${COMMON_ARGS[@]}" \
    --set storage.provider=oss \
    --set storage.mode=existing \
    --set storage.oss.region=cn-test \
    --set credentialProvider.enabled=true \
    --set credentialProvider.image.repository=example.invalid/credential-provider > "${oss_render}"

grep -q 'name: agentteams-controller' "${oss_render}"

if helm template agentteams "${CHART}" "${COMMON_ARGS[@]}" \
    --set deepagents.enabled=true \
    --set storage.provider=oss \
    --set storage.mode=existing \
    --set storage.oss.region=cn-test \
    --set credentialProvider.enabled=true \
    --set credentialProvider.image.repository=example.invalid/credential-provider > /dev/null 2> "${oss_deepagents_error}"; then
    echo "expected DeepAgents with OSS storage to be rejected" >&2
    exit 1
fi

grep -q 'deepagents.enabled=true requires storage.provider=minio' "${oss_deepagents_error}"

echo "PASS: AgentTeams Helm release renders canonical resources and DeepAgents dependencies"
