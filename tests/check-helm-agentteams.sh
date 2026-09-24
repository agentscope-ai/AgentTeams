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
trap 'rm -f "${render}"' EXIT

helm template agentteams "${CHART}" "${COMMON_ARGS[@]}" > "${render}"

grep -q 'name: agentteams-controller' "${render}"
grep -q 'app.kubernetes.io/name: agentteams' "${render}"

helm template agentteams "${CHART}" "${COMMON_ARGS[@]}" \
    --set credentials.llmProvider=atlascloud \
    --set credentials.defaultModel=deepseek-ai/deepseek-v4-pro > "${render}"

if [ "$(grep -c 'AGENTTEAMS_OPENAI_BASE_URL: "https://api.atlascloud.ai/v1"' "${render}")" -ne 2 ]; then
    echo "FAIL: Atlas Cloud base URL must be injected into preflight and runtime Secrets" >&2
    exit 1
fi

helm template agentteams "${CHART}" "${COMMON_ARGS[@]}" \
    --set credentials.llmProvider=atlascloud \
    --set credentials.llmBaseUrl=https://proxy.example.com/v1 > "${render}"

if [ "$(grep -c 'AGENTTEAMS_OPENAI_BASE_URL: "https://proxy.example.com/v1"' "${render}")" -ne 2 ]; then
    echo "FAIL: Explicit LLM base URL must override the Atlas Cloud preset" >&2
    exit 1
fi

echo "PASS: AgentTeams Helm release renders canonical resource names"
