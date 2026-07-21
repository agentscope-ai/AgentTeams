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

new_render="$(mktemp)"
legacy_render="$(mktemp)"
trap 'rm -f "${new_render}" "${legacy_render}"' EXIT

helm template agentteams "${CHART}" "${COMMON_ARGS[@]}" > "${new_render}"
helm template hiclaw "${CHART}" "${COMMON_ARGS[@]}" > "${legacy_render}"

grep -q 'name: agentteams-controller' "${new_render}"
grep -q 'app.kubernetes.io/name: agentteams' "${new_render}"
grep -q 'name: hiclaw-controller' "${legacy_render}"
grep -q 'app.kubernetes.io/name: hiclaw' "${legacy_render}"

echo "PASS: new AgentTeams names and legacy hiclaw Helm release names render compatibly"
