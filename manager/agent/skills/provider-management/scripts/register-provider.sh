#!/bin/bash
# register-provider.sh - Chat-driven onboarding of an extra OpenAI-compatible
# LLM provider on the Higress AI Gateway (plan v2.3 Phase 2b step 4, decision #7).
#
# Reuses the exact request-body shapes of setup-higress.sh's 5c extra-provider
# loop (manager/scripts/init/setup-higress.sh:288-386), driven interactively
# from the Manager's chat session instead of an env-gated boot loop. Registers
# (or deletes) a DNS service-source, an `openai`-type provider, and the
# provider's OWN AI route named `hiclaw-<name>-route`. This script NEVER reads
# or writes `default-ai-route` — the boot-time rewrite of that route
# (setup-higress.sh:253-281) would clobber any edits made here.
#
# Usage:
#   bash register-provider.sh <name> --url <base-url> (--key <API-key> | --key-env <VAR>) \
#       [--models "id1,id2"] [--delete]
#
# Arguments:
#   name          Provider name. Becomes the Higress service-source/provider
#                 name and the model-prefix match "^<name>/". Must not
#                 contain "/" (docs/faq.md:550-552).
#
# Options:
#   --url <base-url>     Required (unless --delete). OpenAI-compatible base
#                         URL, e.g. https://api.example.com/v1
#   --key <API-key>       API key for the provider. NEVER echoed to stdout/logs.
#   --key-env <VAR>       Name of an already-exported env var holding the key.
#                         Preferred over --key: a key passed as --key transits
#                         the Manager's chat history (Matrix, E2EE off locally)
#                         before reaching this script's argv.
#   --models "id1,id2"    Informational only (documented in the console note);
#                         does not restrict the route's modelPredicate, which
#                         is always the "^<name>/" prefix match.
#   --delete              Remove the route, provider, and service-source for
#                         <name> instead of creating/updating them.
#
# Exactly one of --key / --key-env is required unless --delete is given.
#
# Prerequisites:
#   - HIGRESS_COOKIE_FILE env var (session cookie for Higress Console;
#     minted at manager init, start-manager-agent.sh:316,344,382-383)
#   - HICLAW_ADMIN_USER / HICLAW_ADMIN_PASSWORD env vars (re-login fallback,
#     injected via hiclaw-controller config.go:727-728 ManagerAgentEnv)
#   - HICLAW_AI_GATEWAY_DOMAIN env var (falls back to aigw-local.hiclaw.io)
#
# Examples:
#   bash register-provider.sh ollama --url https://ollama.com/v1 --key-env OLLAMA_KEY
#   bash register-provider.sh mimo --url https://platform.xiaomimimo.com/v1 --key sk-xxxx
#   bash register-provider.sh ollama --delete

set -uo pipefail
source /opt/hiclaw/scripts/lib/base.sh

CONSOLE_URL="http://127.0.0.1:8001"
AI_GATEWAY_DOMAIN="${HICLAW_AI_GATEWAY_DOMAIN:-aigw-local.hiclaw.io}"
AI_GATEWAY_LOCAL_DOMAIN="aigw-local.hiclaw.io"
AI_ROUTE_DOMAINS='["'"${AI_GATEWAY_DOMAIN}"'"]'
if [ "${AI_GATEWAY_DOMAIN}" != "${AI_GATEWAY_LOCAL_DOMAIN}" ]; then
    AI_ROUTE_DOMAINS='["'"${AI_GATEWAY_DOMAIN}"'","'"${AI_GATEWAY_LOCAL_DOMAIN}"'"]'
fi

# ============================================================
# Parse arguments
# ============================================================
PROVIDER_NAME=""
PROVIDER_URL=""
PROVIDER_KEY=""
KEY_ENV_VAR=""
MODELS_CSV=""
DO_DELETE="false"

POSITIONAL=()
while [ $# -gt 0 ]; do
    case "$1" in
        --url)
            PROVIDER_URL="${2:-}"
            shift 2
            ;;
        --key)
            PROVIDER_KEY="${2:-}"
            shift 2
            ;;
        --key-env)
            KEY_ENV_VAR="${2:-}"
            shift 2
            ;;
        --models)
            MODELS_CSV="${2:-}"
            shift 2
            ;;
        --delete)
            DO_DELETE="true"
            shift
            ;;
        *)
            POSITIONAL+=("$1")
            shift
            ;;
    esac
done

PROVIDER_NAME="${POSITIONAL[0]:-}"

if [ -z "${PROVIDER_NAME}" ]; then
    echo "Usage: $0 <name> --url <base-url> (--key <API-key> | --key-env <VAR>) [--models \"id1,id2\"] [--delete]"
    exit 1
fi

if echo "${PROVIDER_NAME}" | grep -q '/'; then
    log "ERROR: provider name '${PROVIDER_NAME}' must not contain '/'"
    exit 1
fi

if [ "${DO_DELETE}" != "true" ]; then
    if [ -z "${PROVIDER_URL}" ]; then
        log "ERROR: --url is required (unless --delete)"
        exit 1
    fi

    if [ -n "${PROVIDER_KEY}" ] && [ -n "${KEY_ENV_VAR}" ]; then
        log "ERROR: pass only one of --key / --key-env, not both"
        exit 1
    fi
    if [ -z "${PROVIDER_KEY}" ] && [ -z "${KEY_ENV_VAR}" ]; then
        log "ERROR: one of --key or --key-env is required (unless --delete)"
        exit 1
    fi
    if [ -n "${KEY_ENV_VAR}" ]; then
        PROVIDER_KEY="${!KEY_ENV_VAR:-}"
        if [ -z "${PROVIDER_KEY}" ]; then
            log "ERROR: env var '${KEY_ENV_VAR}' (--key-env) is unset or empty"
            exit 1
        fi
    fi
fi

if [ -z "${HIGRESS_COOKIE_FILE:-}" ]; then
    log "ERROR: HIGRESS_COOKIE_FILE not set"
    exit 1
fi

_ROUTE_NAME="hiclaw-${PROVIDER_NAME}-route"

# ============================================================
# Session helpers: cookie auth with ONE re-login on stale/expired session.
# (setup-mcp-proxy.sh:128-180 idiom: HTML response = expired session;
#  gateway-api.sh:36-50 re-login body shape.)
# ============================================================
_relogin() {
    local admin_user="${HICLAW_ADMIN_USER:-}"
    local admin_password="${HICLAW_ADMIN_PASSWORD:-}"

    if [ -z "${admin_user}" ] || [ -z "${admin_password}" ]; then
        log "ERROR: Higress session expired and HICLAW_ADMIN_USER/HICLAW_ADMIN_PASSWORD are not set — cannot re-login"
        return 1
    fi

    log "Higress session expired, re-logging in..."
    local body
    body=$(jq -nc --arg u "${admin_user}" --arg p "${admin_password}" '{username:$u,password:$p}')
    curl -sf -o /dev/null -X POST "${CONSOLE_URL}/session/login" \
        -H 'Content-Type: application/json' \
        -c "${HIGRESS_COOKIE_FILE}" \
        -d "${body}" 2>/dev/null \
        || { log "ERROR: re-login to Higress Console failed"; return 1; }
    return 0
}

# higress_api <method> <path> <desc> <body>
# Never echoes <body> (it may carry the provider API key). Retries exactly
# once after a re-login if the response looks like the session-expired HTML
# page. Returns non-zero on hard failure (auth/relogin failure); the caller
# decides whether that is fatal.
higress_api() {
    local method="$1"
    local path="$2"
    local desc="$3"
    local body="$4"
    local attempt

    for attempt in 1 2; do
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
            if [ "${attempt}" -eq 1 ]; then
                _relogin || return 1
                continue
            fi
            log "ERROR: ${desc} ... got HTML page after re-login (session still invalid)"
            return 1
        fi

        if [ "${http_code}" = "401" ] || [ "${http_code}" = "403" ]; then
            if [ "${attempt}" -eq 1 ]; then
                _relogin || return 1
                continue
            fi
            log "ERROR: ${desc} ... HTTP ${http_code} auth failed after re-login"
            return 1
        fi

        if echo "${response}" | grep -q '"success":true' 2>/dev/null; then
            log "${desc} ... OK"
        elif [ "${http_code}" = "409" ]; then
            log "${desc} ... already exists, skipping"
        elif echo "${response}" | grep -q '"success":false' 2>/dev/null; then
            log "WARNING: ${desc} ... FAILED (HTTP ${http_code})"
        elif [ "${http_code}" = "200" ] || [ "${http_code}" = "201" ] || [ "${http_code}" = "204" ]; then
            log "${desc} ... OK (HTTP ${http_code})"
        else
            log "WARNING: ${desc} ... unexpected (HTTP ${http_code})"
        fi
        return 0
    done
}

# higress_get <path> — returns body if 200, empty otherwise. Re-logins once
# on an HTML (expired-session) response, same contract as higress_api.
higress_get() {
    local path="$1"
    local attempt

    for attempt in 1 2; do
        local tmpfile
        tmpfile=$(mktemp)
        local http_code
        http_code=$(curl -s -o "${tmpfile}" -w '%{http_code}' -X GET "${CONSOLE_URL}${path}" \
            -b "${HIGRESS_COOKIE_FILE}" 2>/dev/null) || true
        local body
        body=$(cat "${tmpfile}" 2>/dev/null)
        rm -f "${tmpfile}"

        if echo "${body}" | grep -q '<!DOCTYPE html>' 2>/dev/null; then
            if [ "${attempt}" -eq 1 ]; then
                _relogin || return 1
                continue
            fi
            return 1
        fi

        if [ "${http_code}" = "200" ]; then
            echo "${body}"
        fi
        return 0
    done
}

# higress_delete <path> <desc> — session-aware DELETE, mirrors higress_api /
# higress_get: retries exactly once after a re-login if the response looks
# like the session-expired HTML page (or a 401/403), else hard-fails after
# the re-login attempt. Treats HTTP 200/204/404 as success (404 means the
# resource is already absent, which is a fine outcome for --delete). Returns
# non-zero only on a genuine failure (auth failure, or an unexpected status
# after a successful re-login attempt was already exhausted).
higress_delete() {
    local path="$1"
    local desc="$2"
    local attempt

    for attempt in 1 2; do
        local tmpfile
        tmpfile=$(mktemp)
        local http_code
        http_code=$(curl -s -o "${tmpfile}" -w '%{http_code}' -X DELETE "${CONSOLE_URL}${path}" \
            -b "${HIGRESS_COOKIE_FILE}" 2>/dev/null) || true
        local response
        response=$(cat "${tmpfile}" 2>/dev/null)
        rm -f "${tmpfile}"

        if echo "${response}" | grep -q '<!DOCTYPE html>' 2>/dev/null; then
            if [ "${attempt}" -eq 1 ]; then
                _relogin || return 1
                continue
            fi
            log "ERROR: ${desc} ... got HTML page after re-login (session still invalid)"
            return 1
        fi

        if [ "${http_code}" = "401" ] || [ "${http_code}" = "403" ]; then
            if [ "${attempt}" -eq 1 ]; then
                _relogin || return 1
                continue
            fi
            log "ERROR: ${desc} ... HTTP ${http_code} auth failed after re-login"
            return 1
        fi

        if [ "${http_code}" = "200" ] || [ "${http_code}" = "204" ]; then
            log "${desc} ... OK (HTTP ${http_code})"
            return 0
        fi

        if [ "${http_code}" = "404" ]; then
            log "${desc} ... already absent (HTTP 404)"
            return 0
        fi

        log "ERROR: ${desc} ... unexpected (HTTP ${http_code})"
        return 1
    done
}

# ============================================================
# Delete path
# ============================================================
if [ "${DO_DELETE}" = "true" ]; then
    log "Deleting extra LLM provider '${PROVIDER_NAME}' (route, provider, service-source)..."

    _del_failed="false"

    higress_delete "/v1/ai/routes/${_ROUTE_NAME}" "Deleting AI route ${_ROUTE_NAME}" || _del_failed="true"
    higress_delete "/v1/ai/providers/${PROVIDER_NAME}" "Deleting LLM provider ${PROVIDER_NAME}" || _del_failed="true"
    higress_delete "/v1/service-sources/${PROVIDER_NAME}" "Deleting DNS service source ${PROVIDER_NAME}" || _del_failed="true"

    if [ "${_del_failed}" = "true" ]; then
        log "ERROR: one or more deletes failed for provider '${PROVIDER_NAME}' — session/auth issue or unexpected response; provider may NOT be fully removed. Check HIGRESS_COOKIE_FILE / HICLAW_ADMIN_USER / HICLAW_ADMIN_PASSWORD and retry."
        exit 1
    fi

    log "Provider '${PROVIDER_NAME}' removed."
    exit 0
fi

# ============================================================
# Parse domain, port, protocol from the provider's base URL
# (same idiom as setup-higress.sh's 5c loop, :331-337)
# ============================================================
_ep_proto="https"
_ep_port="443"
_ep_url_strip="${PROVIDER_URL#https://}"
_ep_url_strip="${_ep_url_strip#http://}"
echo "${PROVIDER_URL}" | grep -q '^http://' && { _ep_proto="http"; _ep_port="80"; }
_ep_domain="${_ep_url_strip%%/*}"
echo "${_ep_domain}" | grep -q ':' && { _ep_port="${_ep_domain##*:}"; _ep_domain="${_ep_domain%:*}"; }

log "Registering extra LLM provider '${PROVIDER_NAME}' (${PROVIDER_URL})..."

# ============================================================
# Step 1: DNS service source — GET -> PUT if exists, POST if not
# (body shape verbatim from setup-higress.sh:349-353)
# ============================================================
existing_svc=$(higress_get "/v1/service-sources/${PROVIDER_NAME}") || exit 1
SVC_BODY='{"type":"dns","name":"'"${PROVIDER_NAME}"'","port":'"${_ep_port}"',"protocol":"'"${_ep_proto}"'","proxyName":"","domain":"'"${_ep_domain}"'"}'
if [ -n "${existing_svc}" ]; then
    higress_api PUT "/v1/service-sources/${PROVIDER_NAME}" "Updating ${PROVIDER_NAME} DNS service source" "${SVC_BODY}" || exit 1
else
    higress_api POST /v1/service-sources "Registering ${PROVIDER_NAME} DNS service source" "${SVC_BODY}" || exit 1
fi

# ============================================================
# Step 2: LLM provider — GET -> PUT if exists, POST if not
# (body shape verbatim from setup-higress.sh:355-361; key is interpolated
#  directly into the body and is NEVER echoed separately)
# ============================================================
PROVIDER_BODY='{"type":"openai","name":"'"${PROVIDER_NAME}"'","tokens":["'"${PROVIDER_KEY}"'"],"version":0,"protocol":"openai/v1","tokenFailoverConfig":{"enabled":false},"rawConfigs":{"openaiCustomUrl":"'"${PROVIDER_URL}"'","openaiCustomServiceName":"'"${PROVIDER_NAME}"'.dns","openaiCustomServicePort":'"${_ep_port}"',"hiclawMode":true}}'
existing_provider=$(higress_get "/v1/ai/providers/${PROVIDER_NAME}") || exit 1
if [ -n "${existing_provider}" ]; then
    higress_api PUT "/v1/ai/providers/${PROVIDER_NAME}" "Updating LLM provider (${PROVIDER_NAME})" "${PROVIDER_BODY}" || exit 1
else
    higress_api POST /v1/ai/providers "Creating LLM provider (${PROVIDER_NAME})" "${PROVIDER_BODY}" || exit 1
fi

# ============================================================
# Step 3: own AI route hiclaw-<name>-route, model-prefix "<name>/".
# NEVER default-ai-route (setup-higress.sh:364-386 idiom).
# ============================================================
ROUTE_BODY='{"name":"'"${_ROUTE_NAME}"'","domains":'"${AI_ROUTE_DOMAINS}"',"pathPredicate":{"matchType":"PRE","matchValue":"/","caseSensitive":false},"modelPredicate":{"matchType":"PRE","matchValue":"'"${PROVIDER_NAME}"'/"},"upstreams":[{"provider":"'"${PROVIDER_NAME}"'","weight":100,"modelMapping":{}}],"authConfig":{"enabled":true,"allowedCredentialTypes":["key-auth"],"allowedConsumers":["manager"]}}'

existing_route=$(higress_get "/v1/ai/routes/${_ROUTE_NAME}") || exit 1
if [ -n "${existing_route}" ]; then
    patched_route=$(echo "${existing_route}" | jq --argjson domains "${AI_ROUTE_DOMAINS}" '
        .data
        | .upstreams[0].provider = "'"${PROVIDER_NAME}"'"
        | .domains = $domains
    ' 2>/dev/null)
    if [ -n "${patched_route}" ] && [ "${patched_route}" != "null" ]; then
        higress_api PUT "/v1/ai/routes/${_ROUTE_NAME}" "Updating AI Gateway route (${_ROUTE_NAME})" "${patched_route}" || exit 1
    fi
else
    higress_api POST /v1/ai/routes "Creating AI Gateway route (${_ROUTE_NAME}, model-prefix ${PROVIDER_NAME}/)" "${ROUTE_BODY}" || exit 1
fi

log "Provider '${PROVIDER_NAME}' registered. Route: ${_ROUTE_NAME} (model-prefix '${PROVIDER_NAME}/')."
if [ -n "${MODELS_CSV}" ]; then
    log "Models (informational): ${MODELS_CSV}"
fi
log "Next: pin agents to this provider via spec.modelProvider (per-agent) or Team.spec.modelProvider (team-wide, milestone-3 step 4) set to '${PROVIDER_NAME}'."
