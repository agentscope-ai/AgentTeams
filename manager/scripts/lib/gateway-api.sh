#!/bin/bash
# gateway-api.sh - Unified gateway consumer/route/MCP authorization abstraction
#
# Dispatches to Higress Console REST API (local) or controller API (cloud).
#
# Provides:
#   gateway_ensure_session()                  — ensure Higress cookie (local) / no-op (cloud)
#   gateway_create_consumer(name, key)        — create consumer, returns JSON {status, api_key, consumer_id}
#   gateway_authorize_routes(consumer_name)   — authorize all AI routes
#   gateway_authorize_mcp(consumer_name, csv) — authorize MCP servers
#   higress_request(method, path, body)       — low-level Console call w/ cookie auth + 1 re-login retry
#
# Prerequisites:
#   - source hiclaw-env.sh (for HICLAW_RUNTIME)
#   - source container-api.sh (for _orch_api)

# ── Backend detection ─────────────────────────────────────────────────────────

# Higress Console URL: k8s mode uses cluster-internal service, docker uses localhost
_HIGRESS_CONSOLE_URL="${HIGRESS_CONSOLE_URL:-${HICLAW_HIGRESS_CONSOLE_URL:-http://127.0.0.1:8001}}"

_detect_gateway_backend() {
    if [ "${HICLAW_RUNTIME:-}" = "aliyun" ]; then
        echo "aliyun"
    else
        echo "higress"
    fi
}

# ── Low-level request primitive (cookie auth + 1 re-login retry) ─────────────
#
# Shared by register-provider.sh's higress_api/higress_get/higress_delete and
# setup-higress.sh's higress_api/higress_get: all four were independent copies
# of the same "curl with cookie auth, retry once via a re-login on an
# HTML/401/403 (expired-session) response" loop. Consolidated here so the
# retry/detection/status-capture semantics live in exactly one place.
#
# _higress_relogin — POST admin credentials to /session/login, refreshing
# HIGRESS_COOKIE_FILE in place. Requires HICLAW_ADMIN_USER/HICLAW_ADMIN_PASSWORD
# and a `log` function (base.sh) to already be available to the caller.
# Uses CONSOLE_URL if the caller has set it (both register-provider.sh and
# setup-higress.sh hardcode CONSOLE_URL="http://127.0.0.1:8001"), falling
# back to _HIGRESS_CONSOLE_URL otherwise.
_higress_relogin() {
    local console_url="${CONSOLE_URL:-${_HIGRESS_CONSOLE_URL}}"
    local admin_user="${HICLAW_ADMIN_USER:-}"
    local admin_password="${HICLAW_ADMIN_PASSWORD:-}"

    if [ -z "${admin_user}" ] || [ -z "${admin_password}" ]; then
        log "ERROR: Higress session expired and HICLAW_ADMIN_USER/HICLAW_ADMIN_PASSWORD are not set — cannot re-login"
        return 1
    fi

    log "Higress session expired, re-logging in..."
    local body
    body=$(jq -nc --arg u "${admin_user}" --arg p "${admin_password}" '{username:$u,password:$p}')
    printf '%s' "${body}" | curl -sf -o /dev/null -X POST "${console_url}/session/login" \
        -H 'Content-Type: application/json' \
        -c "${HIGRESS_COOKIE_FILE}" \
        --data @- 2>/dev/null \
        || { log "ERROR: re-login to Higress Console failed"; return 1; }
    return 0
}

# higress_request <method> <path> <body>
#
# Cookie-authenticated Higress Console call. <body> (may be empty, e.g. for
# GET/DELETE) is always sent via stdin (--data @-), NEVER argv, so secrets
# (provider API keys, etc.) never appear on the command line / in `ps`.
# Retries exactly once, via _higress_relogin, if the response looks like the
# session-expired HTML login page or comes back 401/403 — the same retry
# budget as before consolidation (one retry, not a loop).
#
# On return, the caller reads:
#   HIGRESS_REQUEST_CODE — HTTP status code (or "" if curl itself failed)
#   HIGRESS_REQUEST_BODY — response body
#   HIGRESS_REQUEST_AUTH_FAILED — "true" if still HTML/401/403 after the
#                                 retry was exhausted (or relogin failed),
#                                 "false" otherwise.
#
# Return code: 0 = got a response (caller inspects the vars above to decide
# success/failure semantics, since each of the three original callers logs
# differently); 1 = _higress_relogin itself failed (unrecoverable — no admin
# creds, or the login POST failed) and no further retry is possible.
higress_request() {
    local method="$1"
    local path="$2"
    local body="${3:-}"
    local console_url="${CONSOLE_URL:-${_HIGRESS_CONSOLE_URL}}"
    local attempt

    HIGRESS_REQUEST_CODE=""
    HIGRESS_REQUEST_BODY=""
    HIGRESS_REQUEST_AUTH_FAILED="false"

    for attempt in 1 2; do
        local tmpfile
        tmpfile=$(mktemp)
        local http_code
        http_code=$(printf '%s' "${body}" | curl -s -o "${tmpfile}" -w '%{http_code}' -X "${method}" "${console_url}${path}" \
            -b "${HIGRESS_COOKIE_FILE}" \
            -H 'Content-Type: application/json' \
            --data @- 2>/dev/null) || true
        local response
        response=$(cat "${tmpfile}" 2>/dev/null)
        rm -f "${tmpfile}"

        if echo "${response}" | grep -q '<!DOCTYPE html>' 2>/dev/null; then
            if [ "${attempt}" -eq 1 ]; then
                _higress_relogin || { HIGRESS_REQUEST_AUTH_FAILED="true"; return 1; }
                continue
            fi
            HIGRESS_REQUEST_CODE="${http_code}"
            HIGRESS_REQUEST_BODY="${response}"
            HIGRESS_REQUEST_AUTH_FAILED="true"
            return 0
        fi

        if [ "${http_code}" = "401" ] || [ "${http_code}" = "403" ]; then
            if [ "${attempt}" -eq 1 ]; then
                _higress_relogin || { HIGRESS_REQUEST_AUTH_FAILED="true"; return 1; }
                continue
            fi
            HIGRESS_REQUEST_CODE="${http_code}"
            HIGRESS_REQUEST_BODY="${response}"
            HIGRESS_REQUEST_AUTH_FAILED="true"
            return 0
        fi

        HIGRESS_REQUEST_CODE="${http_code}"
        HIGRESS_REQUEST_BODY="${response}"
        return 0
    done
}

# ── Session management ────────────────────────────────────────────────────────

gateway_ensure_session() {
    local backend
    backend=$(_detect_gateway_backend)
    [ "${backend}" != "higress" ] && return 0

    if [ -n "${HIGRESS_COOKIE_FILE:-}" ] && [ -s "${HIGRESS_COOKIE_FILE:-}" ]; then
        return 0
    fi

    HIGRESS_COOKIE_FILE="/tmp/higress-session-cookie-gateway"
    local admin_user="${HICLAW_ADMIN_USER:-admin}"
    local admin_password="${HICLAW_ADMIN_PASSWORD:-admin}"

    curl -sf -o /dev/null -X POST "${_HIGRESS_CONSOLE_URL}/session/login" \
        -H 'Content-Type: application/json' \
        -c "${HIGRESS_COOKIE_FILE}" \
        -d '{"username":"'"${admin_user}"'","password":"'"${admin_password}"'"}' 2>/dev/null \
        || { echo "[gateway-api] ERROR: Failed to login to Higress Console" >&2; return 1; }

    export HIGRESS_COOKIE_FILE
}

# ── Consumer creation ─────────────────────────────────────────────────────────

# gateway_create_consumer <consumer_name> <credential_key>
# Returns JSON: {"status": "created"|"exists", "api_key": "...", "consumer_id": "..."}
gateway_create_consumer() {
    local consumer_name="$1"
    local credential_key="$2"
    local backend
    backend=$(_detect_gateway_backend)

    case "${backend}" in
        aliyun)
            _gateway_cloud_create_consumer "${consumer_name}" "${credential_key}"
            ;;
        higress)
            _gateway_higress_create_consumer "${consumer_name}" "${credential_key}"
            ;;
    esac
}

_gateway_cloud_create_consumer() {
    local consumer_name="$1"
    local credential_key="$2"

    local resp
    resp=$(_orch_api POST /gateway/consumers "{\"name\":\"${consumer_name}\"}") || true
    local status
    status=$(echo "${resp}" | jq -r '.status // "error"' 2>/dev/null)

    if [ "${status}" = "created" ] || [ "${status}" = "exists" ]; then
        local api_key consumer_id
        api_key=$(echo "${resp}" | jq -r '.api_key // empty' 2>/dev/null)
        consumer_id=$(echo "${resp}" | jq -r '.consumer_id // empty' 2>/dev/null)
        jq -cn --arg s "${status}" \
               --arg k "${api_key:-${credential_key}}" \
               --arg id "${consumer_id}" \
            '{status: $s, api_key: $k, consumer_id: $id}'
    else
        echo "[gateway-api] ERROR: Cloud consumer creation failed: ${resp}" >&2
        return 1
    fi
}

_gateway_higress_create_consumer() {
    local consumer_name="$1"
    local credential_key="$2"

    curl -sf -X POST ${_HIGRESS_CONSOLE_URL}/v1/consumers \
        -b "${HIGRESS_COOKIE_FILE}" \
        -H 'Content-Type: application/json' \
        -d '{
            "name": "'"${consumer_name}"'",
            "credentials": [{
                "type": "key-auth",
                "source": "BEARER",
                "values": ["'"${credential_key}"'"]
            }]
        }' > /dev/null 2>&1 \
        || { echo "[gateway-api] ERROR: Failed to create Higress consumer ${consumer_name}" >&2; return 1; }

    jq -cn --arg s "created" --arg k "${credential_key}" \
        '{status: $s, api_key: $k, consumer_id: ""}'
}

# ── Route authorization ───────────────────────────────────────────────────────

gateway_authorize_routes() {
    local consumer_name="$1"
    local backend
    backend=$(_detect_gateway_backend)

    case "${backend}" in
        aliyun)
            _gateway_cloud_authorize_routes "${consumer_name}"
            ;;
        higress)
            _gateway_higress_authorize_routes "${consumer_name}"
            ;;
    esac
}

_gateway_cloud_authorize_routes() {
    local consumer_name="$1"
    local consumer_id="${GATEWAY_CONSUMER_ID:-}"

    if [ -n "${consumer_id}" ] && [ -n "${HICLAW_GW_MODEL_API_ID:-}" ] && [ -n "${HICLAW_GW_ENV_ID:-}" ]; then
        _orch_api POST "/gateway/consumers/${consumer_id}/bind" \
            "{\"model_api_id\":\"${HICLAW_GW_MODEL_API_ID}\",\"env_id\":\"${HICLAW_GW_ENV_ID}\"}" > /dev/null 2>&1 || true
    else
        local skip_reason=""
        [ -z "${consumer_id}" ] && skip_reason="consumer_id empty"
        [ -z "${HICLAW_GW_MODEL_API_ID:-}" ] && skip_reason="${skip_reason:+${skip_reason}, }HICLAW_GW_MODEL_API_ID not set"
        [ -z "${HICLAW_GW_ENV_ID:-}" ] && skip_reason="${skip_reason:+${skip_reason}, }HICLAW_GW_ENV_ID not set"
        echo "[gateway-api] Skipping cloud route binding (${skip_reason})" >&2
    fi
}

_gateway_higress_authorize_routes() {
    local consumer_name="$1"
    local max_retries=5

    local ai_routes
    ai_routes=$(curl -sf ${_HIGRESS_CONSOLE_URL}/v1/ai/routes \
        -b "${HIGRESS_COOKIE_FILE}" 2>/dev/null) \
        || { echo "[gateway-api] ERROR: Failed to list AI routes" >&2; return 1; }

    local route_names
    route_names=$(echo "${ai_routes}" | jq -r '.data[]?.name // empty' 2>/dev/null || true)
    for route_name in ${route_names}; do
        [ -z "${route_name}" ] && continue

        local attempt=0
        while [ "${attempt}" -lt "${max_retries}" ]; do
            local route_resp route
            route_resp=$(curl -sf "${_HIGRESS_CONSOLE_URL}/v1/ai/routes/${route_name}" \
                -b "${HIGRESS_COOKIE_FILE}" 2>/dev/null) || break
            route=$(echo "${route_resp}" | jq '.data // .' 2>/dev/null)

            local already
            already=$(echo "${route}" | jq -r '.authConfig.allowedConsumers[]? // empty' 2>/dev/null | grep -c "^${consumer_name}$" || true)
            if [ "${already}" -gt 0 ]; then
                break
            fi

            local updated
            updated=$(echo "${route}" | jq --arg c "${consumer_name}" '.authConfig.allowedConsumers += [$c]')

            local http_code
            http_code=$(curl -s -o /dev/null -w '%{http_code}' -X PUT \
                "${_HIGRESS_CONSOLE_URL}/v1/ai/routes/${route_name}" \
                -b "${HIGRESS_COOKIE_FILE}" \
                -H 'Content-Type: application/json' \
                -d "${updated}")

            if [ "${http_code}" = "200" ]; then
                break
            elif [ "${http_code}" = "409" ]; then
                attempt=$((attempt + 1))
                echo "[gateway-api] Conflict updating route ${route_name}, retrying (${attempt}/${max_retries})..." >&2
                sleep "$((RANDOM % 3 + 1))"
            else
                echo "[gateway-api] WARNING: Failed to update route ${route_name} (HTTP ${http_code})" >&2
                break
            fi
        done

        if [ "${attempt}" -ge "${max_retries}" ]; then
            echo "[gateway-api] ERROR: Failed to update route ${route_name} after ${max_retries} retries" >&2
        fi
    done
}

# ── MCP server authorization ─────────────────────────────────────────────────

gateway_authorize_mcp() {
    local consumer_name="$1"
    local mcp_servers_csv="${2:-}"
    local backend
    backend=$(_detect_gateway_backend)

    case "${backend}" in
        aliyun)
            TARGET_MCP_LIST="${mcp_servers_csv}"
            ;;
        higress)
            _gateway_higress_authorize_mcp "${consumer_name}" "${mcp_servers_csv}"
            ;;
    esac
}

_gateway_higress_authorize_mcp() {
    local consumer_name="$1"
    local mcp_servers_csv="${2:-}"

    local all_mcp_raw all_mcp
    all_mcp_raw=$(curl -sf ${_HIGRESS_CONSOLE_URL}/v1/mcpServer \
        -b "${HIGRESS_COOKIE_FILE}" 2>/dev/null) || true
    all_mcp=$(echo "${all_mcp_raw}" | jq '.data // .' 2>/dev/null || echo "${all_mcp_raw}")

    if [ -n "${mcp_servers_csv}" ]; then
        TARGET_MCP_LIST="${mcp_servers_csv}"
    else
        TARGET_MCP_LIST=$(echo "${all_mcp}" | jq -r '.[].name // empty' 2>/dev/null | tr '\n' ',' || true)
        TARGET_MCP_LIST="${TARGET_MCP_LIST%,}"
    fi

    if [ -z "${TARGET_MCP_LIST}" ]; then
        return 0
    fi

    local existing_names
    existing_names=$(echo "${all_mcp}" | jq -r '.[].name // empty' 2>/dev/null || true)

    local mcp_arr mcp_name
    IFS=',' read -ra mcp_arr <<< "${TARGET_MCP_LIST}"
    local resolved_list=""
    for mcp_name in "${mcp_arr[@]}"; do
        mcp_name=$(echo "${mcp_name}" | tr -d ' ')
        [ -z "${mcp_name}" ] && continue

        if ! echo "${existing_names}" | grep -Fqx "${mcp_name}"; then
            echo "[gateway-api] SKIPPED: MCP server '${mcp_name}' does not exist" >&2
            continue
        fi

        # NOTE: The mcpServer/consumers API does not support optimistic locking (no version field).
        # Re-fetch the latest state right before each update to minimize the race window.
        # TODO: Add version-based conflict detection to the Higress mcpServer/consumers API.
        local fresh_mcp_raw fresh_mcp
        fresh_mcp_raw=$(curl -sf ${_HIGRESS_CONSOLE_URL}/v1/mcpServer \
            -b "${HIGRESS_COOKIE_FILE}" 2>/dev/null) || true
        fresh_mcp=$(echo "${fresh_mcp_raw}" | jq '.data // .' 2>/dev/null || echo "${fresh_mcp_raw}")

        local existing_consumers consumer_list ec
        existing_consumers=$(echo "${fresh_mcp}" | jq -r --arg n "${mcp_name}" \
            '.[] | select(.name == $n) | .consumerAuthInfo.allowedConsumers // [] | .[]' 2>/dev/null || true)
        consumer_list="[\"manager\""
        for ec in ${existing_consumers}; do
            [ "${ec}" = "manager" ] && continue
            [ "${ec}" = "${consumer_name}" ] && continue
            consumer_list="${consumer_list},\"${ec}\""
        done
        consumer_list="${consumer_list},\"${consumer_name}\"]"

        curl -sf -X PUT ${_HIGRESS_CONSOLE_URL}/v1/mcpServer/consumers \
            -b "${HIGRESS_COOKIE_FILE}" \
            -H 'Content-Type: application/json' \
            -d '{"mcpServerName":"'"${mcp_name}"'","consumers":'"${consumer_list}"'}' > /dev/null 2>&1 \
            || echo "[gateway-api] WARNING: Failed to authorize MCP server ${mcp_name}" >&2

        resolved_list="${resolved_list:+${resolved_list},}${mcp_name}"
    done

    TARGET_MCP_LIST="${resolved_list}"
}
