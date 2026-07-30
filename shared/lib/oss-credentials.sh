#!/bin/bash
# oss-credentials.sh - STS credential management for mc (MinIO Client)
#
# Controller-mediated STS (cloud mode):
#    AGENTTEAMS_CONTROLLER_URL + bearer token (AGENTTEAMS_AUTH_TOKEN or token
#    file at AGENTTEAMS_AUTH_TOKEN_FILE) →
#    call controller /api/v1/credentials/sts. The controller obtains STS
#    tokens from its credential-provider sidecar.
#
# 2. No controller creds configured → no-op (local mode, mc alias
#    configured with static credentials against MinIO/self-hosted S3).
#
# STS tokens expire after 1 hour. Credentials are cached and lazy-refreshed.
#
# Usage:
#   source /opt/agentteams/scripts/lib/oss-credentials.sh
#   ensure_mc_credentials   # call before any mc command

_OSS_CRED_FILE="/tmp/mc-oss-credentials.env"
_OSS_CRED_REFRESH_MARGIN=600  # refresh if less than 10 minutes remaining

# --------------------------------------------------------------------------
# Bearer token resolution
# --------------------------------------------------------------------------

# Resolve the bearer token used to authenticate against the controller.
# Priority: active contract AUTH_TOKEN > active contract AUTH_TOKEN_FILE.
# Emits the token on stdout; empty string if none found.
_oss_resolve_bearer() {
    if [ -n "${AGENTTEAMS_AUTH_TOKEN:-}" ]; then
        printf '%s' "${AGENTTEAMS_AUTH_TOKEN}"
        return 0
    fi
    if [ -n "${AGENTTEAMS_AUTH_TOKEN_FILE:-}" ] && [ -f "${AGENTTEAMS_AUTH_TOKEN_FILE}" ]; then
        cat "${AGENTTEAMS_AUTH_TOKEN_FILE}"
        return 0
    fi
    return 0
}

_oss_use_agentteams_env() {
    [ -n "${AGENTTEAMS_WORKER_NAME:-}" ] || [ -n "${AGENTTEAMS_CONTROLLER_URL:-}" ] || \
        [ -n "${AGENTTEAMS_AUTH_TOKEN:-}" ] || [ -n "${AGENTTEAMS_AUTH_TOKEN_FILE:-}" ]
}

_oss_controller_url() {
    printf '%s' "${AGENTTEAMS_CONTROLLER_URL:-}"
}

_oss_storage_alias() {
    local prefix
    if [ -n "${AGENTTEAMS_STORAGE_ALIAS:-}" ]; then
        printf '%s' "${AGENTTEAMS_STORAGE_ALIAS}"
        return
    fi
    prefix="${AGENTTEAMS_STORAGE_PREFIX:-}"
    case "${prefix}" in
        */*) printf '%s' "${prefix%%/*}" ;;
        *) printf '%s' "agentteams" ;;
    esac
}

_oss_auth_error_vars() {
    printf '%s' "AGENTTEAMS_AUTH_TOKEN / AGENTTEAMS_AUTH_TOKEN_FILE"
}

_oss_export_mc_host() {
    export "MC_HOST_$(_oss_storage_alias)"
}

_oss_mc_host_var() {
    printf 'MC_HOST_%s' "$(_oss_storage_alias)"
}

_oss_has_controller_url() {
    [ -n "$(_oss_controller_url)" ]
}

# --------------------------------------------------------------------------
# Path 1: STS via Controller
# --------------------------------------------------------------------------

_oss_refresh_sts_via_controller() {
    local _controller_url="$(_oss_controller_url)"
    local bearer resp http_code
    local sts_ak sts_sk sts_token oss_endpoint mc_host_value mc_host_var

    bearer=$(_oss_resolve_bearer)
    if [ -z "${bearer}" ]; then
        echo "[oss-credentials] ERROR: no bearer token available ($(_oss_auth_error_vars) both empty)" >&2
        return 1
    fi

    resp=$(curl -s -w "\n%{http_code}" -X POST "${_controller_url}/api/v1/credentials/sts" \
        -H "Authorization: Bearer ${bearer}" \
        --connect-timeout 10 --max-time 30 2>&1)

    http_code=$(echo "${resp}" | tail -1)
    resp=$(echo "${resp}" | sed '$d')

    if [ "${http_code}" != "200" ]; then
        echo "[oss-credentials] ERROR: controller STS request failed (HTTP ${http_code})" >&2
        echo "[oss-credentials] Response: ${resp}" >&2
        return 1
    fi

    sts_ak=$(echo "${resp}" | jq -r '.access_key_id')
    sts_sk=$(echo "${resp}" | jq -r '.access_key_secret')
    sts_token=$(echo "${resp}" | jq -r '.security_token')
    oss_endpoint=$(echo "${resp}" | jq -r '.oss_endpoint')

    if [ -z "${sts_ak}" ] || [ "${sts_ak}" = "null" ]; then
        echo "[oss-credentials] ERROR: Failed to parse STS credentials from controller" >&2
        echo "[oss-credentials] Response: ${resp}" >&2
        return 1
    fi

    # Ensure endpoint has a scheme (real providers often return bare hostnames).
    case "${oss_endpoint}" in
        http://*|https://*) ;;
        *) oss_endpoint="https://${oss_endpoint}" ;;
    esac

    # IMPORTANT: pass the STS triple RAW (no percent-encoding). mc
    # (RELEASE.2025-08-13) forwards the MC_HOST userinfo verbatim into
    # the X-Amz-Security-Token header without URL-decoding, so any
    # encoding here turns '+' / '/' inside base64 tokens into literal
    # '%2B' / '%2F' and OSS rejects the request with InvalidSecurityToken.
    # Alibaba Cloud STS outputs stay within the base64 alphabet plus
    # '+ / =', which Go's url.Parse and mc's parser both accept inside
    # userinfo without encoding.
    local creds="${sts_ak}:${sts_sk}"
    if [ -n "${sts_token}" ] && [ "${sts_token}" != "null" ]; then
        creds="${creds}:${sts_token}"
    fi
    mc_host_value="${oss_endpoint%%://*}://${creds}@${oss_endpoint#*://}"

    local expires_at
    expires_at=$(( $(date +%s) + 3600 ))

    mc_host_var="$(_oss_mc_host_var)"
    cat > "${_OSS_CRED_FILE}" <<EOF
${mc_host_var}="${mc_host_value}"
_OSS_CRED_EXPIRES_AT=${expires_at}
EOF
    chmod 600 "${_OSS_CRED_FILE}"

    echo "[oss-credentials] STS credentials refreshed via controller (AK prefix: $(printf '%s' "${sts_ak}" | cut -c1-8)..., endpoint: ${oss_endpoint})" >&2
}

# --------------------------------------------------------------------------
# Public API
# --------------------------------------------------------------------------

ensure_mc_credentials() {
    # Cloud mode: Controller URL + any resolvable bearer token → controller-mediated STS
    if _oss_has_controller_url; then
        local bearer
        bearer=$(_oss_resolve_bearer)
        if [ -n "${bearer}" ]; then
            _oss_ensure_refresh _oss_refresh_sts_via_controller
            return $?
        fi
    fi

    # Local mode: mc alias already configured with static credentials
    return 0
}

# Shared lazy-refresh logic: call the given refresh function only if needed.
_oss_ensure_refresh() {
    local refresh_fn="$1"
    local now needs_refresh=false
    now=$(date +%s)

    if [ -f "${_OSS_CRED_FILE}" ]; then
        . "${_OSS_CRED_FILE}"
        if [ -z "${_OSS_CRED_EXPIRES_AT:-}" ] || [ $(( _OSS_CRED_EXPIRES_AT - now )) -lt ${_OSS_CRED_REFRESH_MARGIN} ]; then
            needs_refresh=true
        fi
    else
        needs_refresh=true
    fi

    if [ "${needs_refresh}" = true ]; then
        ${refresh_fn} || return 1
        . "${_OSS_CRED_FILE}"
    fi

    _oss_export_mc_host
}
