#!/bin/bash
# setup-mcp-server.sh - Create or update an MCP Server on Higress
#
# Unified script for all MCP server types. Handles:
#   1. DNS service source registration (auto-extracted from YAML or explicit)
#   2. YAML credential substitution (unified key: accessToken)
#   3. MCP Server creation/update via PUT (upsert)
#   4. Manager config/mcporter.json update
#   5. Worker consumer authorization + config/mcporter.json update/creation
#
# Usage:
#   bash setup-mcp-server.sh <server-name> <credential-value> [--yaml-file <path>] [--api-domain <domain>]
#
# Arguments:
#   server-name       MCP server name (e.g., "github", "weather").
#                     For built-in templates, looks up references/mcp-<server-name>.yaml.
#                     For custom services, use --yaml-file to provide the YAML config.
#   credential-value  The credential value (e.g., GitHub PAT, API key)
#
# Options:
#   --yaml-file <path>      Path to a user-provided YAML config file. Required when no
#                           built-in template exists for the given server-name.
#   --api-domain <domain>   Explicit API domain for DNS service source (e.g., "api.github.com").
#                           If provided, skips auto-extraction from YAML template.
#                           Required when the template URLs use variables instead of literal domains.
#
# All YAML configs use a unified credential key: "accessToken" in server.config.
# The script substitutes accessToken: "" with the real credential value.
#
# Examples:
#   bash setup-mcp-server.sh github "ghp_xxxxxxxxxxxx"
#   bash setup-mcp-server.sh weather "my-key" --yaml-file /tmp/mcp-weather.yaml
#
# Prerequisites:
#   - HIGRESS_COOKIE_FILE env var (session cookie for Higress Console)
#   - HICLAW_AI_GATEWAY_DOMAIN env var
#   - Higress Console running at http://127.0.0.1:8001

set -euo pipefail
source /opt/hiclaw/scripts/lib/hiclaw-env.sh

# ============================================================
# Parse arguments
# ============================================================
SERVER_NAME=""
CREDENTIAL_VALUE=""
EXPLICIT_API_DOMAIN=""
EXPLICIT_YAML_FILE=""

# Parse positional args and options
POSITIONAL=()
while [ $# -gt 0 ]; do
    case "$1" in
        --api-domain)
            EXPLICIT_API_DOMAIN="${2:-}"
            shift 2
            ;;
        --yaml-file)
            EXPLICIT_YAML_FILE="${2:-}"
            shift 2
            ;;
        *)
            POSITIONAL+=("$1")
            shift
            ;;
    esac
done

SERVER_NAME="${POSITIONAL[0]:-}"
CREDENTIAL_VALUE="${POSITIONAL[1]:-}"

if [ -z "${SERVER_NAME}" ] || [ -z "${CREDENTIAL_VALUE}" ]; then
    echo "Usage: $0 <server-name> <credential-value> [--yaml-file <path>] [--api-domain <domain>]"
    echo ""
    echo "  server-name       e.g., github, weather"
    echo "  credential-value  e.g., ghp_xxx, your-api-key"
    echo "  --yaml-file       Path to user-provided YAML config (required if no built-in template)"
    echo "  --api-domain      Explicit API domain (e.g., api.github.com)"
    exit 1
fi

MCP_SERVER_NAME="mcp-${SERVER_NAME}"
REFERENCES_DIR="/opt/hiclaw/agent/skills/mcp-server-management/references"
BUILTIN_YAML="${REFERENCES_DIR}/mcp-${SERVER_NAME}.yaml"

# Cloud mode: MCP Server management via script is not yet supported
if [ "${HICLAW_RUNTIME:-}" = "aliyun" ]; then
    log "ERROR: MCP Server management via this script is not yet supported in cloud mode (HICLAW_RUNTIME=aliyun)."
    log "Please manage MCP Servers through the Alibaba Cloud AI Gateway console instead."
    log "Cloud MCP Server support will be added in a future release."
    exit 1
fi

if [ -z "${HIGRESS_COOKIE_FILE:-}" ]; then
    log "ERROR: HIGRESS_COOKIE_FILE not set"
    exit 1
fi

# Resolve YAML file: --yaml-file > built-in template
MCP_YAML_FILE=""
if [ -n "${EXPLICIT_YAML_FILE}" ]; then
    if [ ! -f "${EXPLICIT_YAML_FILE}" ]; then
        log "ERROR: Specified YAML file not found: ${EXPLICIT_YAML_FILE}"
        exit 1
    fi
    MCP_YAML_FILE="${EXPLICIT_YAML_FILE}"
    log "Using user-provided YAML: ${MCP_YAML_FILE}"
elif [ -f "${BUILTIN_YAML}" ]; then
    MCP_YAML_FILE="${BUILTIN_YAML}"
    log "Using built-in template: ${MCP_YAML_FILE}"
else
    log "ERROR: No built-in template found for '${SERVER_NAME}' (looked for ${BUILTIN_YAML})"
    log "For custom MCP services, provide the YAML config via --yaml-file:"
    log ""
    log "  bash $0 ${SERVER_NAME} <credential> --yaml-file <path> [--api-domain <domain>]"
    log ""
    log "Available built-in templates:"
    ls "${REFERENCES_DIR}"/mcp-*.yaml 2>/dev/null | sed 's|.*/mcp-||;s|\.yaml||;s|^|  |' || echo "  (none)"
    exit 1
fi

AI_GATEWAY_DOMAIN="${HICLAW_AI_GATEWAY_DOMAIN:-aigw-local.hiclaw.io}"
CONSOLE_URL="http://127.0.0.1:8001"

# Unified credential key — all YAML configs use accessToken in server.config
CREDENTIAL_KEY="accessToken"

# ============================================================
# Helper functions (same as setup-higress.sh)
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
# Step 1: Service source registration
# Priority: --api-domain flag > auto-extract from YAML template URL.
# If neither yields a domain, exit with error.
# ============================================================
log "Step 1: Registering service source for ${MCP_SERVER_NAME}..."

API_DOMAIN=""
URL_PROTO="https"
URL_PORT=443

if [ -n "${EXPLICIT_API_DOMAIN}" ]; then
    # Use explicit domain provided via --api-domain
    API_DOMAIN="${EXPLICIT_API_DOMAIN}"
    # Handle explicit port in domain (e.g., api.example.com:8443)
    if echo "${API_DOMAIN}" | grep -q ':'; then
        URL_PORT="${API_DOMAIN##*:}"
        API_DOMAIN="${API_DOMAIN%:*}"
    fi
    log "  Using explicit API domain: ${API_DOMAIN}:${URL_PORT}"
else
    # Auto-extract from the first requestTemplate URL in the YAML
    # e.g., "https://api.github.com/repos/..." → domain=api.github.com
    FIRST_URL=$(grep -m1 'url:' "${MCP_YAML_FILE}" | sed 's/.*url: *"//;s/".*//' | head -1)
    if [ -n "${FIRST_URL}" ]; then
        URL_STRIPPED="${FIRST_URL#https://}"
        if echo "${FIRST_URL}" | grep -q '^http://'; then
            URL_PROTO="http"
            URL_PORT=80
            URL_STRIPPED="${FIRST_URL#http://}"
        fi
        # Extract domain (strip path and template expressions)
        CANDIDATE="${URL_STRIPPED%%/*}"
        CANDIDATE="${CANDIDATE%%\{*}"
        # Validate: must look like a real domain (contains a dot, no template syntax)
        if echo "${CANDIDATE}" | grep -q '\.' && ! echo "${CANDIDATE}" | grep -q '{{'; then
            API_DOMAIN="${CANDIDATE}"
            # Handle explicit port
            if echo "${API_DOMAIN}" | grep -q ':'; then
                URL_PORT="${API_DOMAIN##*:}"
                API_DOMAIN="${API_DOMAIN%:*}"
            fi
            log "  Auto-extracted API domain: ${API_DOMAIN}:${URL_PORT}"
        fi
    fi
fi

if [ -z "${API_DOMAIN}" ]; then
    log "ERROR: Could not determine API domain for ${MCP_SERVER_NAME}."
    log "The YAML template URLs may use variables instead of literal domains."
    log "Please re-run with --api-domain to specify the domain explicitly:"
    log ""
    log "  bash $0 ${SERVER_NAME} <credential> --api-domain <domain>"
    log ""
    log "  Example: bash $0 ${SERVER_NAME} \"your-key\" --api-domain \"api.example.com\""
    exit 1
fi

SVC_SOURCE_NAME="${SERVER_NAME}-api"
higress_api POST /v1/service-sources "Registering ${SVC_SOURCE_NAME} DNS service source (${API_DOMAIN}:${URL_PORT})" \
    '{"type":"dns","name":"'"${SVC_SOURCE_NAME}"'","domain":"'"${API_DOMAIN}"'","port":'"${URL_PORT}"',"protocol":"'"${URL_PROTO}"'"}'
SERVICE_REF='[{"name":"'"${SVC_SOURCE_NAME}"'.dns","port":'"${URL_PORT}"',"weight":100}]'

# ============================================================
# Step 2: Read template, substitute credential, create/update MCP Server
# ============================================================
log "Step 2: Configuring ${MCP_SERVER_NAME}..."
MCP_YAML=$(sed "s|${CREDENTIAL_KEY}: \"\"|${CREDENTIAL_KEY}: \"${CREDENTIAL_VALUE}\"|" "${MCP_YAML_FILE}")
RAW_CONFIG=$(printf '%s' "${MCP_YAML}" | jq -Rs .)

MCP_BODY=$(jq -n \
    --arg name "${MCP_SERVER_NAME}" \
    --arg desc "${MCP_SERVER_NAME} MCP Server" \
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
# Config at ./config/mcporter.json (mcporter default path, no --config needed)
# Symlink at ~/mcporter-servers.json for backward compatibility
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
    # Backward-compatible symlink
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
        # Read worker gateway key from persisted credentials
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
            # Update existing config/mcporter.json
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
            # Create new config/mcporter.json for worker
            jq -n --arg name "${MCP_SERVER_NAME}" --arg domain "${AI_GATEWAY_DOMAIN}" --arg key "${WORKER_KEY}" \
                '{mcpServers: {($name): {
                    url: ("http://" + $domain + ":8080/mcp-servers/" + $name + "/mcp"),
                    transport: "http",
                    headers: {Authorization: ("Bearer " + $key)}
                }}}' > "${MCPORTER_FILE}"
            log "  Created config/mcporter.json for ${wname}"
        fi
        # Backward-compatible symlink
        ln -sfn "${MCPORTER_FILE}" "${MCPORTER_COMPAT}"
        # Push to MinIO immediately (don't rely on mc mirror --watch)
        ensure_mc_credentials 2>/dev/null || true
        mc cp "${MCPORTER_FILE}" "${HICLAW_STORAGE_PREFIX}/agents/${wname}/config/mcporter.json" 2>/dev/null \
            && log "  Pushed config/mcporter.json to MinIO for ${wname}" \
            || log "  WARNING: Failed to push config/mcporter.json to MinIO for ${wname}"
    done
else
    log "  No workers-registry.json found, skipping Worker authorization"
fi

log "${MCP_SERVER_NAME} setup complete"
log "NOTE: The auth plugin needs ~10s to activate."
