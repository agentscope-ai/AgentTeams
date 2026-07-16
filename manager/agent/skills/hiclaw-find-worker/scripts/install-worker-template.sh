#!/bin/bash
# install-worker-template.sh - Install a Worker from a Nacos template
#
# Usage:
#   install-worker-template.sh --template <template-name> --worker-name <name>
#   install-worker-template.sh --template reviewer --worker-name alice --model claude-sonnet-4-6
#   install-worker-template.sh --package-uri nacos://127.0.0.1:8848/public/reviewer --worker-name alice

set -euo pipefail

TEMPLATE_NAME=""
WORKER_NAME=""
PACKAGE_URI=""
VERSION=""
MODEL=""
SKILLS=""
RUNTIME=""
DRY_RUN=false
NACOS_REGISTRY_URI="${AGENTTEAMS_NACOS_REGISTRY_URI:-nacos://market.agentteams.io:80/public}"
NACOS_HOST=""
NACOS_PORT=""
NACOS_NAMESPACE=""

usage() {
    echo "Usage: $0 (--template <template-name> | --package-uri <nacos://...>) --worker-name <name> [--version <v>] [--model <model>] [--skills s1,s2] [--runtime openclaw|copaw|hermes|openhuman] [--dry-run]" >&2
    exit 1
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --template)
            TEMPLATE_NAME="${2:-}"
            shift 2
            ;;
        --worker-name|--name)
            WORKER_NAME="${2:-}"
            shift 2
            ;;
        --package-uri)
            PACKAGE_URI="${2:-}"
            shift 2
            ;;
        --version)
            VERSION="${2:-}"
            shift 2
            ;;
        --model)
            MODEL="${2:-}"
            shift 2
            ;;
        --skills)
            SKILLS="${2:-}"
            shift 2
            ;;
        --runtime)
            RUNTIME="${2:-}"
            shift 2
            ;;
        --dry-run)
            DRY_RUN=true
            shift
            ;;
        -h|--help)
            usage
            ;;
        *)
            echo "Unknown option: $1" >&2
            usage
            ;;
    esac
done

if [[ -z "$WORKER_NAME" ]]; then
    echo "--worker-name is required" >&2
    exit 1
fi

if [[ -z "$TEMPLATE_NAME" && -z "$PACKAGE_URI" ]]; then
    echo "Provide either --template or --package-uri" >&2
    exit 1
fi

url_encode_path_segment() {
    jq -nr --arg value "$1" '$value|@uri'
}

parse_registry_uri() {
    local url="$1"
    [[ -z "$url" ]] && return 0
    url="${url#nacos://}"
    url="${url%%\?*}"
    local host_port="${url%%/*}"
    local path=""
    if [[ "$url" == */* ]]; then
        path="${url#*/}"
        path="${path%%/*}"
    fi
    NACOS_HOST="${host_port%%:*}"
    if [[ "$host_port" == *:* ]]; then
        NACOS_PORT="${host_port##*:}"
    fi
    NACOS_NAMESPACE="$path"
}

if [[ -z "$PACKAGE_URI" ]]; then
    parse_registry_uri "$NACOS_REGISTRY_URI"
    NACOS_HOST="${NACOS_HOST:-market.agentteams.io}"
    NACOS_PORT="${NACOS_PORT:-8848}"
    NACOS_NAMESPACE="${NACOS_NAMESPACE:-public}"

    ENCODED_TEMPLATE_NAME="$(url_encode_path_segment "$TEMPLATE_NAME")"
    PACKAGE_URI="nacos://${NACOS_HOST}:${NACOS_PORT}/${NACOS_NAMESPACE}/${ENCODED_TEMPLATE_NAME}"
    if [[ -n "$VERSION" ]]; then
        ENCODED_VERSION="$(url_encode_path_segment "$VERSION")"
        PACKAGE_URI="${PACKAGE_URI}/${ENCODED_VERSION}"
    fi
fi

AGENTTEAMS_ARGS=(apply worker --name "$WORKER_NAME" --package "$PACKAGE_URI")
if [[ -n "$MODEL" ]]; then
    AGENTTEAMS_ARGS+=(--model "$MODEL")
fi
if [[ -n "$SKILLS" ]]; then
    AGENTTEAMS_ARGS+=(--skills "$SKILLS")
fi
if [[ -n "$RUNTIME" ]]; then
    AGENTTEAMS_ARGS+=(--runtime "$RUNTIME")
fi

if [[ "$DRY_RUN" == true ]]; then
    jq -n \
        --arg worker_name "$WORKER_NAME" \
        --arg template_name "$TEMPLATE_NAME" \
        --arg package_uri "$PACKAGE_URI" \
        --arg model "$MODEL" \
        --arg skills "$SKILLS" \
        --arg runtime "$RUNTIME" \
        --argjson hiclaw_args "$(printf '%s\n' "${AGENTTEAMS_ARGS[@]}" | jq -R . | jq -s .)" \
        '{
            worker_name: $worker_name,
            template_name: (if $template_name == "" then null else $template_name end),
            package_uri: $package_uri,
            overrides: {
                model: (if $model == "" then null else $model end),
                skills: (if $skills == "" then null else ($skills | split(",")) end),
                runtime: (if $runtime == "" then null else $runtime end)
            },
            hiclaw_args: $hiclaw_args
        }'
    exit 0
fi

exec hiclaw "${AGENTTEAMS_ARGS[@]}"
