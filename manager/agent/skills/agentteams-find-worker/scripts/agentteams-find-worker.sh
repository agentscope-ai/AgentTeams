#!/bin/bash
# agentteams-find-worker.sh - Search Worker templates stored as Nacos AgentSpecs
#
# Usage:
#   agentteams-find-worker.sh --name <template-name> --json
#   agentteams-find-worker.sh --query "frontend performance" --limit 3 --json
#   agentteams-find-worker.sh --limit 10

set -euo pipefail

PAGE_SIZE="${AGENTTEAMS_NACOS_AGENTSPEC_PAGE_SIZE:-100}"
LIMIT=3
QUERY=""
EXACT_NAME=""
OUTPUT_JSON=false
NACOS_REGISTRY_URI="${AGENTTEAMS_NACOS_REGISTRY_URI:-nacos://market.agentteams.io:80/public}"
NACOS_USERNAME="${AGENTTEAMS_NACOS_USERNAME:-}"
NACOS_PASSWORD="${AGENTTEAMS_NACOS_PASSWORD:-}"
NACOS_HOST=""
NACOS_PORT=""
NACOS_NAMESPACE=""

usage() {
    echo "Usage: $0 [--query <text> | --name <template-name>] [--limit <n>] [--json]" >&2
    exit 1
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --query)
            QUERY="${2:-}"
            shift 2
            ;;
        --name)
            EXACT_NAME="${2:-}"
            shift 2
            ;;
        --limit)
            LIMIT="${2:-}"
            shift 2
            ;;
        --json)
            OUTPUT_JSON=true
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

if [[ -n "$QUERY" && -n "$EXACT_NAME" ]]; then
    echo "Use either --query or --name, not both." >&2
    exit 1
fi

if ! [[ "$LIMIT" =~ ^[0-9]+$ ]] || [[ "$LIMIT" -lt 1 ]]; then
    echo "--limit must be a positive integer" >&2
    exit 1
fi

url_encode_path_segment() {
    jq -nr --arg value "$1" '$value|@uri'
}

run_nacos_cli() {
    local args=()
    if [[ -n "$NACOS_HOST" ]]; then
        args+=(--host "$NACOS_HOST")
    fi
    if [[ -n "$NACOS_PORT" ]]; then
        args+=(--port "$NACOS_PORT")
    fi
    if [[ -n "$NACOS_NAMESPACE" ]]; then
        args+=(--namespace "$NACOS_NAMESPACE")
    fi
    if [[ -n "$NACOS_USERNAME" ]]; then
        args+=(--username "$NACOS_USERNAME")
    fi
    if [[ -n "$NACOS_PASSWORD" ]]; then
        args+=(--password "$NACOS_PASSWORD")
    fi
    npx -y @nacos-group/cli "${args[@]}" "$@"
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

parse_registry_uri "$NACOS_REGISTRY_URI"

NACOS_HOST="${NACOS_HOST:-market.agentteams.io}"
NACOS_PORT="${NACOS_PORT:-8848}"
NACOS_NAMESPACE="${NACOS_NAMESPACE:-public}"

lower() {
    printf '%s' "$1" | tr '[:upper:]' '[:lower:]'
}

ALL_LINES=()
TOTAL=0
SEEN=0
PAGE=1

while :; do
    LIST_OUTPUT="$(run_nacos_cli agentspec-list --page "$PAGE" --size "$PAGE_SIZE")"

    if printf '%s\n' "$LIST_OUTPUT" | grep -q '^No agent specs found$'; then
        break
    fi

    if [[ "$TOTAL" -eq 0 ]]; then
        TOTAL="$(printf '%s\n' "$LIST_OUTPUT" | sed -n 's/.*Total: \([0-9][0-9]*\).*/\1/p' | head -n 1)"
        TOTAL="${TOTAL:-0}"
    fi

    PAGE_LINES=()
    while IFS= read -r line; do
        PAGE_LINES+=("$line")
        ALL_LINES+=("$line")
        SEEN=$((SEEN + 1))
    done < <(printf '%s\n' "$LIST_OUTPUT" | sed -n 's/^[[:space:]]*[0-9][0-9]*\. \(.*\)$/\1/p')

    if [[ "${#PAGE_LINES[@]}" -eq 0 || "$SEEN" -ge "$TOTAL" ]]; then
        break
    fi

    PAGE=$((PAGE + 1))
done

QUERY_LOWER="$(lower "$QUERY")"
NAME_LOWER="$(lower "$EXACT_NAME")"
RESULTS='[]'

for line in "${ALL_LINES[@]}"; do
    if [[ ! "$line" =~ ^(.+)\ \[(enabled|disabled),\ online:([0-9]+)\]$ ]]; then
        continue
    fi

    core="${BASH_REMATCH[1]}"
    status="${BASH_REMATCH[2]}"
    online="${BASH_REMATCH[3]}"

    if [[ "$core" == *" - "* ]]; then
        template_name="${core%% - *}"
        summary="${core#* - }"
    else
        template_name="$core"
        summary=""
    fi

    score=0
    match_reason="all"
    template_name_lower="$(lower "$template_name")"
    summary_lower="$(lower "$summary")"
    matched=true

    if [[ -n "$NAME_LOWER" ]]; then
        if [[ "$template_name_lower" != "$NAME_LOWER" ]]; then
            matched=false
        else
            score=1000
            match_reason="exact_name"
        fi
    elif [[ -n "$QUERY_LOWER" ]]; then
        matched=false
        if [[ "$template_name_lower" == "$QUERY_LOWER" ]]; then
            matched=true
            score=900
            match_reason="exact_name"
        elif [[ "$template_name_lower" == *"$QUERY_LOWER"* ]]; then
            matched=true
            score=700
            match_reason="name_contains_query"
        elif [[ "$summary_lower" == *"$QUERY_LOWER"* ]]; then
            matched=true
            score=500
            match_reason="summary_contains_query"
        fi

        token_hits=0
        all_tokens_match=true
        for token in $QUERY_LOWER; do
            [[ -z "$token" ]] && continue
            if [[ "$template_name_lower" == *"$token"* ]]; then
                token_hits=$((token_hits + 2))
                matched=true
            elif [[ "$summary_lower" == *"$token"* ]]; then
                token_hits=$((token_hits + 1))
                matched=true
            else
                all_tokens_match=false
            fi
        done

        if [[ "$token_hits" -gt 0 ]]; then
            score=$((score + token_hits * 50))
            if [[ "$match_reason" == "all" ]]; then
                match_reason="token_match"
            fi
        fi
        if [[ "$all_tokens_match" == true && "$QUERY_LOWER" == *" "* ]]; then
            score=$((score + 120))
            match_reason="all_tokens_match"
        fi
    else
        score=100
    fi

    if [[ "$matched" != true ]]; then
        continue
    fi

    encoded_template_name="$(url_encode_path_segment "$template_name")"
    package_uri="nacos://${NACOS_HOST}:${NACOS_PORT}/${NACOS_NAMESPACE}/${encoded_template_name}"

    RESULTS="$(printf '%s\n' "$RESULTS" | jq \
        --arg name "$template_name" \
        --arg summary "$summary" \
        --arg status "$status" \
        --arg match_reason "$match_reason" \
        --arg package_uri "$package_uri" \
        --argjson online "$online" \
        --argjson score "$score" \
        '. + [{
            name: $name,
            summary: (if $summary == "" then null else $summary end),
            status: $status,
            online_instances: $online,
            package_uri: $package_uri,
            match_reason: $match_reason,
            score: $score
        }]')"
done

RESULTS="$(printf '%s\n' "$RESULTS" | jq 'sort_by(-.score, .name)')"
MATCHED_COUNT="$(printf '%s\n' "$RESULTS" | jq 'length')"
LIMITED_RESULTS="$(printf '%s\n' "$RESULTS" | jq --argjson limit "$LIMIT" '.[0:$limit]')"

if [[ "$OUTPUT_JSON" == true ]]; then
    jq -n \
        --arg query "$QUERY" \
        --arg exact_name "$EXACT_NAME" \
        --arg host "$NACOS_HOST" \
        --arg port "$NACOS_PORT" \
        --arg namespace "$NACOS_NAMESPACE" \
        --argjson total_templates "$TOTAL" \
        --argjson matched "$MATCHED_COUNT" \
        --argjson limit "$LIMIT" \
        --argjson templates "$LIMITED_RESULTS" \
        '{
            source: "nacos_agentspec",
            registry: {
                host: $host,
                port: $port,
                namespace: $namespace
            },
            query: (if $query == "" then null else $query end),
            exact_name: (if $exact_name == "" then null else $exact_name end),
            total_templates: $total_templates,
            matched_templates: $matched,
            returned_templates: ($templates | length),
            limit: $limit,
            templates: $templates
        }'
    exit 0
fi

printf 'Nacos worker templates (%s:%s/%s)\n' "$NACOS_HOST" "$NACOS_PORT" "$NACOS_NAMESPACE"
if [[ -n "$EXACT_NAME" ]]; then
    printf 'Exact name: %s\n' "$EXACT_NAME"
elif [[ -n "$QUERY" ]]; then
    printf 'Query: %s\n' "$QUERY"
fi
printf 'Matched: %s\n\n' "$MATCHED_COUNT"
printf '%s\n' "$LIMITED_RESULTS" | jq -r '.[] | "- \(.name): \(.summary // "no summary") [\(.status), online:\(.online_instances)]"'
