#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"

legacy_brand='hi''claw'
archive_paths=(
    ':(exclude)blog/**'
    ':(exclude)changelog/**'
    ':(exclude)docs/faq-legacy.md'
    ':(exclude)docs/zh-cn/faq-legacy.md'
)

if matches="$(git grep -nIi "${legacy_brand}" -- . "${archive_paths[@]}" 2>/dev/null)"; then
    echo "FAIL: active files still contain the retired brand:" >&2
    echo "${matches}" >&2
    exit 1
fi

if paths="$(git ls-files | grep -i "${legacy_brand}" || true)" && [ -n "${paths}" ]; then
    echo "FAIL: tracked paths still use the retired brand:" >&2
    echo "${paths}" >&2
    exit 1
fi

required_paths=(
    agentteams-controller/go.mod
    helm/agentteams/Chart.yaml
    install/agentteams-install.sh
    install/agentteams-install.ps1
    shared/lib/agentteams-env.sh
)
for path in "${required_paths[@]}"; do
    if [ ! -e "${path}" ]; then
        echo "FAIL: canonical AgentTeams path is missing: ${path}" >&2
        exit 1
    fi
done

grep -Fq 'module github.com/agentscope-ai/AgentTeams/agentteams-controller' agentteams-controller/go.mod
grep -Fq 'name: agentteams' helm/agentteams/Chart.yaml
grep -Fq 'define "agentteams.name"' helm/agentteams/templates/_helpers.tpl

echo "PASS: active source tree uses AgentTeams-only names and contracts"
