#!/bin/bash
# setup-higress.sh - Configure Higress routes, consumers, and MCP servers
# Called by start-manager-agent.sh after Higress Console is ready.
# Requires HIGRESS_COOKIE_FILE env var to be set.
#
# Design:
#   NON-IDEMPOTENT (marker-protected): service-sources, consumer, static routes.
#     These are created once on first boot. Re-running risks overwriting worker
#     consumers added to allowedConsumers by the Manager Agent.
#   IDEMPOTENT (always runs): AI Gateway Route, LLM Provider, GitHub MCP Server.
#     These reflect current env config and must be updated on every boot so that
#     upgrades (e.g. switching LLM provider) take effect without a clean reinstall.

source /opt/hiclaw/scripts/lib/base.sh

MATRIX_DOMAIN="${HICLAW_MATRIX_DOMAIN:-matrix-local.hiclaw.io:8080}"
MATRIX_CLIENT_DOMAIN="${HICLAW_MATRIX_CLIENT_DOMAIN:-matrix-client-local.hiclaw.io}"
AI_GATEWAY_DOMAIN="${HICLAW_AI_GATEWAY_DOMAIN:-aigw-local.hiclaw.io}"
FS_DOMAIN="${HICLAW_FS_DOMAIN:-fs-local.hiclaw.io}"
CONSOLE_DOMAIN="${HICLAW_CONSOLE_DOMAIN:-console-local.hiclaw.io}"
# Fixed internal domains used by workers inside hiclaw-net, regardless of user-configured domains.
# Higress routes always include these so workers can reach manager services reliably.
AI_GATEWAY_LOCAL_DOMAIN="aigw-local.hiclaw.io"
FS_LOCAL_DOMAIN="fs-local.hiclaw.io"

LLM_PROVIDER="${HICLAW_LLM_PROVIDER:-qwen}"
LLM_API_URL="${HICLAW_LLM_API_URL:-}"
if [ -z "${LLM_API_URL}" ]; then
    case "${LLM_PROVIDER}" in
        qwen) LLM_API_URL="https://dashscope.aliyuncs.com/compatible-mode/v1" ;;
        *)    LLM_API_URL="" ;;
    esac
fi

CONSOLE_URL="http://127.0.0.1:8001"

# ============================================================
# Helper: call Higress Console API, log result, never fail.
# ============================================================
higress_api() {
    local method="$1"
    local path="$2"
    local desc="$3"
    shift 3
    local body="$*"

    local tmpfile
    tmpfile=$(mktemp)
    local http_code
    http_code=$(curl -s -o "${tmpfile}" -w '%{http_code}' -X "${method}" "${CONSOLE_URL}${path}" \
        -b "${HIGRESS_COOKIE_FILE}" \
        -H 'Content-Type: application/json' \
        -d "${body}" 2>/dev/null) || true
    local response
    response=$(cat "${tmpfile}" 2>/dev/null)
    rm -f "${tmpfile}"

    if echo "${response}" | grep -q '<!DOCTYPE html>' 2>/dev/null; then
        log "ERROR: ${desc} ... got HTML page (session expired?). Re-login needed."
        return 1
    fi
    if [ "${http_code}" = "401" ] || [ "${http_code}" = "403" ]; then
        log "ERROR: ${desc} ... HTTP ${http_code} auth failed"
        return 1
    fi
    if echo "${response}" | grep -q '"success":true' 2>/dev/null; then
        log "${desc} ... OK"
    elif [ "${http_code}" = "409" ]; then
        log "${desc} ... already exists, skipping"
    elif echo "${response}" | grep -q '"success":false' 2>/dev/null; then
        log "WARNING: ${desc} ... FAILED (HTTP ${http_code}): ${response}"
    elif [ "${http_code}" = "200" ] || [ "${http_code}" = "201" ] || [ "${http_code}" = "204" ]; then
        log "${desc} ... OK (HTTP ${http_code})"
    else
        log "WARNING: ${desc} ... unexpected (HTTP ${http_code}): ${response}"
    fi
}

# Helper: GET a resource, return body if 200, empty string otherwise.
higress_get() {
    local path="$1"
    local tmpfile
    tmpfile=$(mktemp)
    local http_code
    http_code=$(curl -s -o "${tmpfile}" -w '%{http_code}' -X GET "${CONSOLE_URL}${path}" \
        -b "${HIGRESS_COOKIE_FILE}" 2>/dev/null) || true
    local body
    body=$(cat "${tmpfile}" 2>/dev/null)
    rm -f "${tmpfile}"
    if [ "${http_code}" = "200" ]; then
        echo "${body}"
    fi
}

# ============================================================
# NON-IDEMPOTENT SECTION
# Skipped after first boot (marker exists).
# ============================================================
SETUP_MARKER="/data/.higress-setup-done"
if [ ! -f "${SETUP_MARKER}" ]; then
    log "First boot: configuring Higress static resources..."

    # 0. Local service sources
    higress_api POST /v1/service-sources "Registering Tuwunel service source" \
        '{"name":"tuwunel","type":"static","domain":"127.0.0.1:6167","port":6167,"properties":{},"authN":{"enabled":false}}'
    higress_api POST /v1/service-sources "Registering Element Web service source" \
        '{"name":"element-web","type":"static","domain":"127.0.0.1:8088","port":8088,"properties":{},"authN":{"enabled":false}}'
    higress_api POST /v1/service-sources "Registering MinIO service source" \
        '{"name":"minio","type":"static","domain":"127.0.0.1:9000","port":9000,"properties":{},"authN":{"enabled":false}}'
    higress_api POST /v1/service-sources "Registering OpenClaw Console service source" \
        '{"name":"openclaw-console","type":"static","domain":"127.0.0.1:18888","port":18888,"properties":{},"authN":{"enabled":false}}'

    # 1. Domains
    higress_api POST /v1/domains "Creating Matrix Client domain" \
        '{"name":"'"${MATRIX_CLIENT_DOMAIN}"'","enableHttps":"off"}'
    higress_api POST /v1/domains "Creating File System domain" \
        '{"name":"'"${FS_DOMAIN}"'","enableHttps":"off"}'
    # Always register the fixed internal FS domain so workers on hiclaw-net can reach MinIO
    if [ "${FS_DOMAIN}" != "${FS_LOCAL_DOMAIN}" ]; then
        higress_api POST /v1/domains "Creating internal File System domain" \
            '{"name":"'"${FS_LOCAL_DOMAIN}"'","enableHttps":"off"}'
    fi
    higress_api POST /v1/domains "Creating OpenClaw Console domain" \
        '{"name":"'"${CONSOLE_DOMAIN}"'","enableHttps":"off"}'

    # 2. Manager Consumer
    higress_api POST /v1/consumers "Creating Manager consumer" \
        '{"name":"manager","credentials":[{"type":"key-auth","source":"BEARER","values":["'"${HICLAW_MANAGER_GATEWAY_KEY}"'"]}]}'

    # 3. Matrix Homeserver Route
    higress_api POST /v1/routes "Creating Matrix Homeserver route" \
        '{"name":"matrix-homeserver","domains":[],"path":{"matchType":"PRE","matchValue":"/_matrix"},"services":[{"name":"tuwunel.static","port":6167,"weight":100}]}'

    # 4. Element Web Route
    higress_api POST /v1/routes "Creating Element Web route" \
        '{"name":"matrix-web-client","domains":["'"${MATRIX_CLIENT_DOMAIN}"'"],"path":{"matchType":"PRE","matchValue":"/"},"services":[{"name":"element-web.static","port":8088,"weight":100}]}'

    # 5. HTTP File System Route — always include internal domain for worker access
    FS_ROUTE_DOMAINS='["'"${FS_DOMAIN}"'"]'
    if [ "${FS_DOMAIN}" != "${FS_LOCAL_DOMAIN}" ]; then
        FS_ROUTE_DOMAINS='["'"${FS_DOMAIN}"'","'"${FS_LOCAL_DOMAIN}"'"]'
    fi
    higress_api POST /v1/routes "Creating HTTP file system route" \
        '{"name":"http-filesystem","domains":'"${FS_ROUTE_DOMAINS}"',"path":{"matchType":"PRE","matchValue":"/"},"services":[{"name":"minio.static","port":9000,"weight":100}]}'

    # 6. OpenClaw Console Route (reverse-proxied via nginx with auto-token injection)
    higress_api POST /v1/routes "Creating OpenClaw Console route" \
        '{"name":"openclaw-console","domains":["'"${CONSOLE_DOMAIN}"'"],"path":{"matchType":"PRE","matchValue":"/"},"services":[{"name":"openclaw-console.static","port":18888,"weight":100}]}'

    # 6a. Enable basic-auth on OpenClaw Console route
    higress_api PUT /v1/routes/openclaw-console/plugin-instances/basic-auth "Enabling basic-auth on OpenClaw Console route" \
        '{"version":null,"scope":"ROUTE","target":"openclaw-console","targets":{"ROUTE":"openclaw-console"},"pluginName":"basic-auth","pluginVersion":null,"internal":false,"enabled":true,"rawConfigurations":"consumers:\n  - name: admin\n    credential: '"${HICLAW_ADMIN_USER:-admin}"':'"${HICLAW_ADMIN_PASSWORD}"'"}'

    touch "${SETUP_MARKER}"
    log "First-boot setup complete"
else
    log "Higress static resources already configured (marker found) — skipping non-idempotent setup"
fi

# ============================================================
# IDEMPOTENT SECTION
# Always runs: reflects current env config, supports upgrades.
# ============================================================

# ============================================================
# AI Gateway Domain (idempotent: POST returns 409 if exists)
# ============================================================
higress_api POST /v1/domains "Creating AI Gateway domain" \
    '{"name":"'"${AI_GATEWAY_DOMAIN}"'","enableHttps":"off"}'
# Always register the fixed internal AI Gateway domain for worker access
if [ "${AI_GATEWAY_DOMAIN}" != "${AI_GATEWAY_LOCAL_DOMAIN}" ]; then
    higress_api POST /v1/domains "Creating internal AI Gateway domain" \
        '{"name":"'"${AI_GATEWAY_LOCAL_DOMAIN}"'","enableHttps":"off"}'
fi

# Build AI Gateway route domains: always include internal domain for worker access
AI_ROUTE_DOMAINS='["'"${AI_GATEWAY_DOMAIN}"'"]'
if [ "${AI_GATEWAY_DOMAIN}" != "${AI_GATEWAY_LOCAL_DOMAIN}" ]; then
    AI_ROUTE_DOMAINS='["'"${AI_GATEWAY_DOMAIN}"'","'"${AI_GATEWAY_LOCAL_DOMAIN}"'"]'
fi

# ============================================================
# LLM Provider + AI Gateway Route
# ============================================================
if [ -n "${HICLAW_LLM_API_KEY}" ]; then

    # Create/update LLM provider (GET → PUT if exists, POST if not)
    case "${LLM_PROVIDER}" in
        qwen)
            PROVIDER_BODY='{"type":"qwen","name":"qwen","tokens":["'"${HICLAW_LLM_API_KEY}"'"],"protocol":"openai/v1","tokenFailoverConfig":{"enabled":false},"rawConfigs":{"qwenEnableSearch":false,"qwenEnableCompatible":true,"qwenFileIds":[],"hiclawMode":true}}'
            existing_provider=$(higress_get /v1/ai/providers/qwen)
            if [ -n "${existing_provider}" ]; then
                higress_api PUT /v1/ai/providers/qwen "Updating LLM provider (qwen)" "${PROVIDER_BODY}"
            else
                higress_api POST /v1/ai/providers "Creating LLM provider (qwen)" "${PROVIDER_BODY}"
            fi
            ;;
        openai-compat)
            OPENAI_BASE_URL="${HICLAW_OPENAI_BASE_URL:-}"
            if [ -z "${OPENAI_BASE_URL}" ]; then
                log "WARNING: HICLAW_OPENAI_BASE_URL not set, skipping openai-compat provider setup"
            else
                # Parse domain, port, protocol from base URL
                OC_PROTO="https"
                OC_PORT="443"
                OC_URL_STRIP="${OPENAI_BASE_URL#https://}"
                OC_URL_STRIP="${OC_URL_STRIP#http://}"
                echo "${OPENAI_BASE_URL}" | grep -q '^http://' && { OC_PROTO="http"; OC_PORT="80"; }
                OC_DOMAIN="${OC_URL_STRIP%%/*}"
                echo "${OC_DOMAIN}" | grep -q ':' && { OC_PORT="${OC_DOMAIN##*:}"; OC_DOMAIN="${OC_DOMAIN%:*}"; }

                # Service source: GET → PUT if exists, POST if not
                existing_svc=$(higress_get /v1/service-sources/openai-compat)
                SVC_BODY='{"type":"dns","name":"openai-compat","port":'"${OC_PORT}"',"protocol":"'"${OC_PROTO}"'","proxyName":"","domain":"'"${OC_DOMAIN}"'"}'
                if [ -n "${existing_svc}" ]; then
                    higress_api PUT /v1/service-sources/openai-compat "Updating openai-compat DNS service source" "${SVC_BODY}"
                else
                    higress_api POST /v1/service-sources "Registering openai-compat DNS service source" "${SVC_BODY}"
                fi

                PROVIDER_BODY='{"type":"openai","name":"openai-compat","tokens":["'"${HICLAW_LLM_API_KEY}"'"],"version":0,"protocol":"openai/v1","tokenFailoverConfig":{"enabled":false},"rawConfigs":{"openaiCustomUrl":"'"${OPENAI_BASE_URL}"'","openaiCustomServiceName":"openai-compat.dns","openaiCustomServicePort":'"${OC_PORT}"',"hiclawMode":true}}'
                existing_provider=$(higress_get /v1/ai/providers/openai-compat)
                if [ -n "${existing_provider}" ]; then
                    higress_api PUT /v1/ai/providers/openai-compat "Updating LLM provider (openai-compat)" "${PROVIDER_BODY}"
                else
                    higress_api POST /v1/ai/providers "Creating LLM provider (openai-compat)" "${PROVIDER_BODY}"
                fi
            fi
            ;;
        *)
            PROVIDER_BODY='{"name":"'"${LLM_PROVIDER}"'","type":"openai","tokens":["'"${HICLAW_LLM_API_KEY}"'"],"modelMapping":{},"protocol":"openai/v1"'
            if [ -n "${LLM_API_URL}" ]; then
                PROVIDER_BODY="${PROVIDER_BODY}"',"rawConfigs":{"apiUrl":"'"${LLM_API_URL}"'","hiclawMode":true}'
            else
                PROVIDER_BODY="${PROVIDER_BODY}"',"rawConfigs":{"hiclawMode":true}'
            fi
            PROVIDER_BODY="${PROVIDER_BODY}"'}'
            existing_provider=$(higress_get /v1/ai/providers/"${LLM_PROVIDER}")
            if [ -n "${existing_provider}" ]; then
                higress_api PUT /v1/ai/providers/"${LLM_PROVIDER}" "Updating LLM provider (${LLM_PROVIDER})" "${PROVIDER_BODY}"
            else
                higress_api POST /v1/ai/providers "Creating LLM provider (${LLM_PROVIDER})" "${PROVIDER_BODY}"
            fi
            ;;
    esac

    # 5b. Create or update AI Gateway Route (GET → PUT if exists, POST if not)
    AI_ROUTE_BODY='{"name":"default-ai-route","domains":'"${AI_ROUTE_DOMAINS}"',"pathPredicate":{"matchType":"PRE","matchValue":"/","caseSensitive":false},"upstreams":[{"provider":"'"${LLM_PROVIDER}"'","weight":100,"modelMapping":{}}],"authConfig":{"enabled":true,"allowedCredentialTypes":["key-auth"],"allowedConsumers":["manager"]}}'

    HICLAW_VERSION=$(cat /opt/hiclaw/agent/.builtin-version 2>/dev/null | tr -d '[:space:]')
    HICLAW_VERSION="${HICLAW_VERSION:-latest}"

    existing_route_resp=$(higress_get /v1/ai/routes/default-ai-route)
    if [ -n "${existing_route_resp}" ]; then
        # Extract the AiRoute object from the response wrapper (.data), then patch:
        #   - upstreams[0].provider: reflect current LLM provider
        #   - domains: ensure internal local domain is always present
        #   - headerControl.request.add: inject User-Agent header (add = set if absent, don't overwrite)
        # Preserve all other fields (especially authConfig.allowedConsumers and version).
        patched=$(echo "${existing_route_resp}" | jq --argjson domains "${AI_ROUTE_DOMAINS}" '
            .data
            | .upstreams[0].provider = "'"${LLM_PROVIDER}"'"
            | .domains = $domains
            | .headerControl.enabled = true
            | .headerControl.request.add = [{"key":"user-agent","value":"HiClaw/'"${HICLAW_VERSION}"'"}]
            | .headerControl.request.set  //= []
            | .headerControl.request.remove //= []
            | .headerControl.response.add //= []
            | .headerControl.response.set //= []
            | .headerControl.response.remove //= []
        ' 2>/dev/null)
        if [ -n "${patched}" ] && [ "${patched}" != "null" ]; then
            higress_api PUT /v1/ai/routes/default-ai-route "Updating AI Gateway route (provider=${LLM_PROVIDER}, User-Agent=HiClaw/${HICLAW_VERSION})" "${patched}"
        fi
    else
        # Inject headerControl into the initial route body
        AI_ROUTE_BODY=$(echo "${AI_ROUTE_BODY}" | jq '
            . + {"headerControl":{"enabled":true,"request":{"add":[{"key":"user-agent","value":"HiClaw/'"${HICLAW_VERSION}"'"}],"set":[],"remove":[]},"response":{"add":[],"set":[],"remove":[]}}}
        ' 2>/dev/null)
        higress_api POST /v1/ai/routes "Creating AI Gateway route (provider=${LLM_PROVIDER}, User-Agent=HiClaw/${HICLAW_VERSION})" "${AI_ROUTE_BODY}"
    fi

else
    log "Skipping AI Gateway configuration (no HICLAW_LLM_API_KEY)"
fi

# ============================================================
# 6. GitHub MCP Server (idempotent via PUT)
# ============================================================
if [ -n "${HICLAW_GITHUB_TOKEN}" ]; then
    higress_api POST /v1/service-sources "Registering GitHub API service source" \
        '{"type":"dns","name":"github-api","domain":"api.github.com","port":443,"protocol":"https"}'

    MCP_YAML_FILE="/opt/hiclaw/agent/skills/mcp-server-management/references/mcp-github.yaml"
    if [ -f "${MCP_YAML_FILE}" ]; then
        MCP_YAML=$(sed "s|accessToken: \"\"|accessToken: \"${HICLAW_GITHUB_TOKEN}\"|" "${MCP_YAML_FILE}")
        RAW_CONFIG=$(printf '%s' "${MCP_YAML}" | jq -Rs .)
        MCP_BODY=$(cat <<MCPEOF
{"name":"mcp-github","description":"GitHub MCP Server","type":"OPEN_API","rawConfigurations":${RAW_CONFIG},"mcpServerName":"mcp-github","domains":["${AI_GATEWAY_DOMAIN}"],"services":[{"name":"github-api.dns","port":443,"weight":100}],"consumerAuthInfo":{"type":"key-auth","enable":true,"allowedConsumers":["manager"]}}
MCPEOF
        )
        higress_api PUT /v1/mcpServer "Configuring GitHub MCP Server" "${MCP_BODY}"
        # GET to check if manager is already authorized; PUT (add) only if not present
        # GET with consumerName filter returns matching entries; empty list means not authorized
        consumer_check=$(higress_get "/v1/mcpServer/consumers?mcpServerName=mcp-github&consumerName=manager")
        consumer_count=$(echo "${consumer_check}" | jq '.total // 0' 2>/dev/null)
        if [ "${consumer_count}" = "0" ] || [ -z "${consumer_count}" ]; then
            higress_api PUT /v1/mcpServer/consumers "Authorizing Manager for GitHub MCP" \
                '{"mcpServerName":"mcp-github","consumers":["manager"]}'
        else
            log "Manager already authorized for GitHub MCP, skipping"
        fi
    else
        log "WARNING: MCP config not found at ${MCP_YAML_FILE}, skipping GitHub MCP Server"
    fi
else
    log "Skipping GitHub MCP Server configuration (no HICLAW_GITHUB_TOKEN)"
fi

# ============================================================
# Wait for AI plugin activation (~45 seconds for first config)
# ============================================================
log "Waiting for AI Gateway plugin activation (45s)..."
sleep 45

log "Higress setup complete"
