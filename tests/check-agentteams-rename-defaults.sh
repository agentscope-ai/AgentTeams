#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"

legacy_brand='hi''claw'
legacy_brand_title='Hi''Claw'
archive_paths=(
    ':(exclude)blog/**'
    ':(exclude)changelog/**'
    ':(exclude)docs/usage/troubleshooting/legacy-faq.md'
    ':(exclude)docs/zh-cn/usage/troubleshooting/legacy-faq.md'
)

if matches="$(git grep -nIi "${legacy_brand}" -- . "${archive_paths[@]}" 2>/dev/null)"; then
    unexpected_matches=""
    while IFS= read -r match; do
        case "${match}" in
            install/agentteams-dashboard-tests.sh:*)
                ;;
            install/agentteams-install.sh:*"${legacy_brand}-controller"* | \
            install/agentteams-install.sh:*"/var/run/${legacy_brand}/cli-token"* | \
            install/agentteams-install.sh:*"older ${legacy_brand_title} images"* | \
            install/agentteams-install.sh:*"AgentTeams/${legacy_brand_title} images"*)
                ;;
            *)
                unexpected_matches="${unexpected_matches}${unexpected_matches:+$'\n'}${match}"
                ;;
        esac
    done <<< "${matches}"

    if [ -n "${unexpected_matches}" ]; then
        echo "FAIL: active files still contain the retired brand:" >&2
        echo "${unexpected_matches}" >&2
        exit 1
    fi
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
