#!/bin/bash
# setup-mcp-proxy.sh - Proxy an existing MCP Server through Higress
#
# Sets up an existing SSE or StreamableHTTP MCP server as a proxied MCP server
# on the Higress AI Gateway. Unlike setup-mcp-server.sh (REST-to-MCP), this
# script does not require a YAML template — it generates the mcp-proxy config
# automatically from the provided URL and transport type.
#
# Usage:
#   bash setup-mcp-proxy.sh <server-name> <url> <transport> [--header "Key: Value"] ...
#
# Arguments:
#   server-name   MCP server name (e.g., "sentry", "notion"). Auto-prefixed with "mcp-".
#   url           Backend MCP server URL (e.g., "https://mcp.sentry.dev/mcp")
#   transport     Protocol type: "http" (StreamableHTTP) or "sse" (Server-Sent Events)
#
# Options:
#   --header "Key: Value"   Repeatable. Auth header(s) to forward to the backend MCP server.
#                           Examples: --header "Authorization: Bearer xxx"
#                                     --header "X-API-Key: my-key"
#
# Examples:
#   bash setup-mcp-proxy.sh sentry https://mcp.sentry.dev/mcp http
#   bash setup-mcp-proxy.sh notion https://mcp.notion.com/mcp http --header "Authorization: Bearer ntn_xxx"
#   bash setup-mcp-proxy.sh asana https://mcp.asana.com/sse sse --header "X-API-Key: my-key"
#
# Prerequisites:
#   - HIGRESS_COOKIE_FILE env var (session cookie for Higress Console)
#   - HICLAW_AI_GATEWAY_DOMAIN env var

set -euo pipefail
source /opt/hiclaw/scripts/lib/hiclaw-env.sh

# ============================================================
# Parse arguments
# ============================================================
SERVER_NAME=""
MCP_URL=""
TRANSPORT=""
HEADERS=()

POSITIONAL=()
while [ $# -gt 0 ]; do
    case "$1" in
        --header)
            HEADERS+=("${2:-}")
            shift 2
            ;;
        *)
            POSITIONAL+=("$1")
            shift
            ;;
    esac
done

SERVER_NAME="${POSITIONAL[0]:-}"
MCP_URL="${POSITIONAL[1]:-}"
TRANSPORT="${POSITIONAL[2]:-}"

if [ -z "${SERVER_NAME}" ] || [ -z "${MCP_URL}" ] || [ -z "${TRANSPORT}" ]; then
    echo "Usage: $0 <server-name> <url> <transport> [--header \"Key: Value\"] ..."
    echo ""
    echo "  server-name   e.g., sentry, notion, asana"
    echo "  url           Backend MCP server URL"
    echo "  transport     http (StreamableHTTP) or sse (Server-Sent Events)"
    echo "  --header      Repeatable. Auth header for backend (e.g., \"Authorization: Bearer xxx\")"
    exit 1
fi

# Validate transport
if [ "${TRANSPORT}" != "http" ] && [ "${TRANSPORT}" != "sse" ]; then
    log "ERROR: transport must be 'http' or 'sse', got '${TRANSPORT}'"
    log "  http = StreamableHTTP protocol"
    log "  sse  = Server-Sent Events protocol"
    log "  stdio is not supported (cannot be proxied through the gateway)"
    exit 1
fi

# Validate URL
if ! echo "${MCP_URL}" | grep -qE '^https?://'; then
    log "ERROR: URL must start with http:// or https://, got '${MCP_URL}'"
    exit 1
fi

MCP_SERVER_NAME="mcp-${SERVER_NAME}"

# Cloud mode check
if [ "${HICLAW_RUNTIME:-}" = "aliyun" ]; then
    log "ERROR: MCP Server management via this script is not yet supported in cloud mode (HICLAW_RUNTIME=aliyun)."
    log "Please manage MCP Servers through the Alibaba Cloud AI Gateway console instead."
    exit 1
fi

if [ -z "${HIGRESS_COOKIE_FILE:-}" ]; then
    log "ERROR: HIGRESS_COOKIE_FILE not set"
    exit 1
fi

AI_GATEWAY_DOMAIN="${HICLAW_AI_GATEWAY_DOMAIN:-aigw-local.hiclaw.io}"
CONSOLE_URL="http://127.0.0.1:8001"

# ============================================================
# Helper functions (same pattern as setup-mcp-server.sh)
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
# Step 1: Register DNS service source
# ============================================================
log "Step 1: Registering service source for ${MCP_SERVER_NAME}..."

URL_STRIPPED="${MCP_URL#https://}"
URL_PROTO="https"
URL_PORT=443
if echo "${MCP_URL}" | grep -q '^http://'; then
    URL_PROTO="http"
    URL_PORT=80
    URL_STRIPPED="${MCP_URL#http://}"
fi

# Extract domain (strip path)
API_DOMAIN="${URL_STRIPPED%%/*}"
# Handle explicit port in domain (e.g., example.com:8443)
if echo "${API_DOMAIN}" | grep -q ':'; then
    URL_PORT="${API_DOMAIN##*:}"
    API_DOMAIN="${API_DOMAIN%:*}"
fi
log "  Backend: ${URL_PROTO}://${API_DOMAIN}:${URL_PORT}"

SVC_SOURCE_NAME="${SERVER_NAME}-proxy"
higress_api POST /v1/service-sources "Registering ${SVC_SOURCE_NAME} DNS service source (${API_DOMAIN}:${URL_PORT})" \
    '{"type":"dns","name":"'"${SVC_SOURCE_NAME}"'","domain":"'"${API_DOMAIN}"'","port":'"${URL_PORT}"',"protocol":"'"${URL_PROTO}"'"}'
SERVICE_REF='[{"name":"'"${SVC_SOURCE_NAME}"'.dns","port":'"${URL_PORT}"',"weight":100}]'

# ============================================================
# Step 2: Generate mcp-proxy YAML and create MCP Server
# ============================================================
log "Step 2: Configuring ${MCP_SERVER_NAME} (mcp-proxy, transport=${TRANSPORT})..."

TIMEOUT=5000
if [ "${TRANSPORT}" = "sse" ]; then
    TIMEOUT=10000
fi

# Build base YAML
MCP_YAML="server:
  name: ${SERVER_NAME}-mcp-server
  type: mcp-proxy
  transport: ${TRANSPORT}
  mcpServerURL: \"${MCP_URL}\"
  timeout: ${TIMEOUT}"

# Process --header flags into securitySchemes + defaultUpstreamSecurity
if [ ${#HEADERS[@]} -gt 0 ]; then
    SCHEMES_YAML=""
    SCHEME_INDEX=0
    for hdr in "${HEADERS[@]}"; do
        HDR_KEY="${hdr%%:*}"
        HDR_VAL="${hdr#*: }"
        # Trim leading space if separator was ":"
        HDR_VAL="${HDR_VAL# }"
        SCHEME_ID="UpstreamAuth${SCHEME_INDEX}"

        if [ "${HDR_KEY}" = "Authorization" ]; then
            if echo "${HDR_VAL}" | grep -qi '^Bearer '; then
                TOKEN="${HDR_VAL#* }"
                SCHEMES_YAML="${SCHEMES_YAML}
  - id: ${SCHEME_ID}
    type: http
    scheme: bearer
    defaultCredential: \"${TOKEN}\""
            elif echo "${HDR_VAL}" | grep -qi '^Basic '; then
                CRED="${HDR_VAL#* }"
                SCHEMES_YAML="${SCHEMES_YAML}
  - id: ${SCHEME_ID}
    type: http
    scheme: basic
    defaultCredential: \"${CRED}\""
            else
                # Unknown Authorization scheme, treat as apiKey
                SCHEMES_YAML="${SCHEMES_YAML}
  - id: ${SCHEME_ID}
    type: apiKey
    in: header
    name: ${HDR_KEY}
    defaultCredential: \"${HDR_VAL}\""
            fi
        else
            # Non-Authorization header (e.g., X-API-Key)
            SCHEMES_YAML="${SCHEMES_YAML}
  - id: ${SCHEME_ID}
    type: apiKey
    in: header
    name: ${HDR_KEY}
    defaultCredential: \"${HDR_VAL}\""
        fi
        SCHEME_INDEX=$((SCHEME_INDEX + 1))
    done

    # Use the first scheme as defaultUpstreamSecurity
    MCP_YAML="${MCP_YAML}
  securitySchemes:${SCHEMES_YAML}
  defaultUpstreamSecurity:
    id: UpstreamAuth0"
fi

RAW_CONFIG=$(printf '%s' "${MCP_YAML}" | jq -Rs .)

MCP_BODY=$(jq -n \
    --arg name "${MCP_SERVER_NAME}" \
    --arg desc "${MCP_SERVER_NAME} MCP Proxy Server (${TRANSPORT})" \
    --argjson raw "${RAW_CONFIG}" \
    --arg domain "${AI_GATEWAY_DOMAIN}" \
    --argjson services "${SERVICE_REF}" \
    '{
        name: $name,
        description: $desc,
        type: "OPEN_API",
        rawConfigurations: $raw,
        mcpServerName: $name,
        domains: [$domain],
        services: $services,
        consumerAuthInfo: {type: "key-auth", enable: true, allowedConsumers: ["manager"]}
    }')

higress_api PUT /v1/mcpServer "Configuring ${MCP_SERVER_NAME}" "${MCP_BODY}"

# ============================================================
# Step 3: Authorize Manager consumer
# ============================================================
log "Step 3: Authorizing Manager for ${MCP_SERVER_NAME}..."
consumer_check=$(higress_get "/v1/mcpServer/consumers?mcpServerName=${MCP_SERVER_NAME}&consumerName=manager")
consumer_count=$(echo "${consumer_check}" | jq '.total // 0' 2>/dev/null)
if [ "${consumer_count}" = "0" ] || [ -z "${consumer_count}" ]; then
    higress_api PUT /v1/mcpServer/consumers "Authorizing Manager for ${MCP_SERVER_NAME}" \
        '{"mcpServerName":"'"${MCP_SERVER_NAME}"'","consumers":["manager"]}'
else
    log "  Manager already authorized, skipping"
fi

# ============================================================
# Step 4: Update Manager's own mcporter config
# ============================================================
log "Step 4: Updating Manager mcporter config..."
MANAGER_KEY="${HICLAW_MANAGER_GATEWAY_KEY:-}"
MANAGER_MCPORTER_DIR="${HOME}/config"
MANAGER_MCPORTER="${MANAGER_MCPORTER_DIR}/mcporter.json"
MANAGER_MCPORTER_COMPAT="${HOME}/mcporter-servers.json"
if [ -n "${MANAGER_KEY}" ]; then
    mkdir -p "${MANAGER_MCPORTER_DIR}"
    if [ -f "${MANAGER_MCPORTER}" ]; then
        UPDATED=$(jq --arg name "${MCP_SERVER_NAME}" --arg domain "${AI_GATEWAY_DOMAIN}" --arg key "${MANAGER_KEY}" \
            '.mcpServers[$name] = {
                url: ("http://" + $domain + ":8080/mcp-servers/" + $name + "/mcp"),
                transport: "http",
                headers: {Authorization: ("Bearer " + $key)}
            }' "${MANAGER_MCPORTER}" 2>/dev/null)
        echo "${UPDATED}" | jq . > "${MANAGER_MCPORTER}"
    else
        jq -n --arg name "${MCP_SERVER_NAME}" --arg domain "${AI_GATEWAY_DOMAIN}" --arg key "${MANAGER_KEY}" \
            '{mcpServers: {($name): {
                url: ("http://" + $domain + ":8080/mcp-servers/" + $name + "/mcp"),
                transport: "http",
                headers: {Authorization: ("Bearer " + $key)}
            }}}' > "${MANAGER_MCPORTER}"
    fi
    ln -sfn "${MANAGER_MCPORTER}" "${MANAGER_MCPORTER_COMPAT}"
    log "  Manager config/mcporter.json updated"
else
    log "  WARNING: HICLAW_MANAGER_GATEWAY_KEY not set, skipping Manager mcporter update"
fi

# ============================================================
# Step 5: Authorize existing Workers and update their configs
# ============================================================
log "Step 5: Authorizing existing Workers for ${MCP_SERVER_NAME}..."
REGISTRY_FILE="${HOME}/workers-registry.json"
if [ -f "${REGISTRY_FILE}" ]; then
    CONSUMER_LIST='["manager"'
    WORKER_NAMES=$(jq -r '.workers | keys[]' "${REGISTRY_FILE}" 2>/dev/null || true)
    for wname in ${WORKER_NAMES}; do
        CONSUMER_LIST="${CONSUMER_LIST},\"worker-${wname}\""
    done
    CONSUMER_LIST="${CONSUMER_LIST}]"

    higress_api PUT /v1/mcpServer/consumers "Authorizing all consumers for ${MCP_SERVER_NAME}" \
        '{"mcpServerName":"'"${MCP_SERVER_NAME}"'","consumers":'"${CONSUMER_LIST}"'}'

    for wname in ${WORKER_NAMES}; do
        WORKER_AGENT_DIR="/root/hiclaw-fs/agents/${wname}"
        MCPORTER_DIR="${WORKER_AGENT_DIR}/config"
        MCPORTER_FILE="${MCPORTER_DIR}/mcporter.json"
        MCPORTER_COMPAT="${WORKER_AGENT_DIR}/mcporter-servers.json"
        WORKER_CREDS="/data/worker-creds/${wname}.env"
        WORKER_KEY=""
        if [ -f "${WORKER_CREDS}" ]; then
            WORKER_KEY=$(grep '^WORKER_GATEWAY_KEY=' "${WORKER_CREDS}" | sed 's/^WORKER_GATEWAY_KEY="//;s/"$//')
        fi
        if [ -z "${WORKER_KEY}" ]; then
            log "  WARNING: No gateway key for ${wname} (creds file missing), skipping mcporter update"
            continue
        fi
        mkdir -p "${MCPORTER_DIR}"
        if [ -f "${MCPORTER_FILE}" ]; then
            UPDATED=$(jq --arg name "${MCP_SERVER_NAME}" --arg domain "${AI_GATEWAY_DOMAIN}" --arg key "${WORKER_KEY}" \
                '.mcpServers[$name] = {
                    url: ("http://" + $domain + ":8080/mcp-servers/" + $name + "/mcp"),
                    transport: "http",
                    headers: {Authorization: ("Bearer " + $key)}
                }' "${MCPORTER_FILE}" 2>/dev/null)
            if [ -n "${UPDATED}" ] && [ "${UPDATED}" != "null" ]; then
                echo "${UPDATED}" | jq . > "${MCPORTER_FILE}"
                log "  Updated config/mcporter.json for ${wname}"
            else
                log "  WARNING: Failed to update config/mcporter.json for ${wname}"
                continue
            fi
        else
            jq -n --arg name "${MCP_SERVER_NAME}" --arg domain "${AI_GATEWAY_DOMAIN}" --arg key "${WORKER_KEY}" \
                '{mcpServers: {($name): {
                    url: ("http://" + $domain + ":8080/mcp-servers/" + $name + "/mcp"),
                    transport: "http",
                    headers: {Authorization: ("Bearer " + $key)}
                }}}' > "${MCPORTER_FILE}"
            log "  Created config/mcporter.json for ${wname}"
        fi
        ln -sfn "${MCPORTER_FILE}" "${MCPORTER_COMPAT}"
        ensure_mc_credentials 2>/dev/null || true
        mc cp "${MCPORTER_FILE}" "${HICLAW_STORAGE_PREFIX}/agents/${wname}/config/mcporter.json" 2>/dev/null \
            && log "  Pushed config/mcporter.json to MinIO for ${wname}" \
            || log "  WARNING: Failed to push config/mcporter.json to MinIO for ${wname}"
    done
else
    log "  No workers-registry.json found, skipping Worker authorization"
fi

log "${MCP_SERVER_NAME} proxy setup complete"
log "NOTE: The auth plugin needs ~10s to activate."
