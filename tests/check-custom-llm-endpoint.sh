#!/usr/bin/env bash
set -eo pipefail
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# Execute the real openai-compat bootstrap branch; only the Console transport is stubbed.
block="$(sed -n '/^        openai-compat)$/,/^        \*)$/p' "$ROOT_DIR/manager/scripts/init/setup-higress.sh" | sed '$d')"
workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT
log() { :; }
higress_get() { if [ "$existing" = yes ]; then echo '{"data":{"version":"7","extra":"preserve","rawConfigs":{"extra":"preserve"}}}'; fi; }
higress_api() { printf '%s\n' "$4" > "$workdir/$(echo "$2" | tr / _).json"; }
for existing in no yes; do
    for spec in 'http://192.0.2.10:3000/v1|static|192.0.2.10:3000|3000|http://192.0.2.10/v1|http' 'https://192.0.2.10:8443/v1|static|192.0.2.10:8443|8443|https://192.0.2.10/v1|https' 'https://api.example.com:8443/v1|dns|api.example.com|8443|https://api.example.com/v1|https' 'https://api.example.com/v1|dns|api.example.com|443|https://api.example.com/v1|https'; do
        IFS='|' read -r AGENTTEAMS_OPENAI_BASE_URL kind domain port custom protocol <<< "$spec"
        AGENTTEAMS_LLM_API_KEY=synthetic-test
        eval "case openai-compat in $block esac"
        svc="$workdir/_v1_service-sources.json"
        provider="$workdir/_v1_ai_providers.json"
        [ "$existing" = yes ] && { svc="$workdir/_v1_service-sources_openai-compat.json"; provider="$workdir/_v1_ai_providers_openai-compat.json"; }
        jq -e --arg kind "$kind" --arg domain "$domain" --argjson port "$port" --arg protocol "$protocol" '.type==$kind and .domain==$domain and .port==$port and .protocol==$protocol' "$svc" >/dev/null
        jq -e --arg service "openai-compat.$kind" --arg custom "$custom" --argjson port "$port" '.rawConfigs.openaiCustomServiceName==$service and .rawConfigs.openaiCustomUrl==$custom and .rawConfigs.openaiCustomServicePort==$port' "$provider" >/dev/null
        [ "$existing" = no ] || jq -e '.version=="7" and .extra=="preserve"' "$svc" "$provider" >/dev/null
    done
done
AGENTTEAMS_OPENAI_BASE_URL='http://[::1]:3000/v1'
if (eval "case openai-compat in $block esac"); then echo 'FAIL: unsupported IPv6 accepted'; exit 1; fi
echo 'PASS: Shell custom LLM IP/DNS, HTTP/HTTPS, port, upgrade/restart, explicit IPv6 rejection'
