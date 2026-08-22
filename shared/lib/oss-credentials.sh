#!/bin/bash
# oss-credentials.sh - Shared STS credential management for mc (MinIO Client)
#
# In cloud SAE mode, mc requires STS temporary credentials via MC_HOST_hiclaw.
# STS tokens expire after 1 hour. This library provides lazy-refresh: credentials
# are cached in a file and refreshed only when they are about to expire.
#
# Usage:
#   source /opt/hiclaw/scripts/lib/oss-credentials.sh
#   ensure_mc_credentials   # call before any mc command
#   mc mirror ...
#
# In local mode (no OIDC env vars), ensure_mc_credentials is a no-op.

_OSS_CRED_FILE="/tmp/mc-oss-credentials.env"
_OSS_CRED_REFRESH_MARGIN=600  # refresh if less than 10 minutes remaining

# Internal: build an inline STS policy that restricts OSS access to the
# worker's own prefix (agents/<name>/*) and the shared prefix (shared/*).
# Called only when HICLAW_WORKER_NAME is set (i.e. worker context).
# Manager does not set HICLAW_WORKER_NAME, so it gets unrestricted access.
_oss_build_worker_policy() {
    local worker="$1"
    local bucket="${HICLAW_OSS_BUCKET:-hiclaw-cloud-storage}"

    cat <<POLICY
{"Version":"1","Statement":[{"Effect":"Allow","Action":["oss:ListObjects"],"Resource":["acs:oss:*:*:${bucket}"],"Condition":{"StringLike":{"oss:Prefix":["agents/${worker}/*","shared/*"]}}},{"Effect":"Allow","Action":["oss:GetObject","oss:PutObject","oss:DeleteObject"],"Resource":["acs:oss:*:*:${bucket}/agents/${worker}/*","acs:oss:*:*:${bucket}/shared/*"]}]}
POLICY
}

# Internal: call STS AssumeRoleWithOIDC and write credentials to file
_oss_refresh_sts() {
    local oidc_token region sts_resp http_code
    local sts_ak sts_sk sts_token expires_at

    oidc_token=$(cat "${ALIBABA_CLOUD_OIDC_TOKEN_FILE}")
    region="${HICLAW_REGION:-cn-hangzhou}"

    local timestamp nonce
    timestamp=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
    nonce=$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')

    # Build inline policy arg for workers (restricts STS token to own prefix)
    local policy_args=()
    if [ -n "${HICLAW_WORKER_NAME:-}" ]; then
        local policy
        policy=$(_oss_build_worker_policy "${HICLAW_WORKER_NAME}")
        policy_args=(--data-urlencode "Policy=${policy}")
        echo "[oss-credentials] Applying worker inline policy for '${HICLAW_WORKER_NAME}'" >&2
    fi

    sts_resp=$(curl -s -w "\n%{http_code}" -X POST "https://sts-vpc.${region}.aliyuncs.com" \
        -d "Action=AssumeRoleWithOIDC" \
        -d "Format=JSON" \
        -d "Version=2015-04-01" \
        --data-urlencode "Timestamp=${timestamp}" \
        -d "SignatureNonce=${nonce}" \
        --data-urlencode "RoleArn=${ALIBABA_CLOUD_ROLE_ARN}" \
        --data-urlencode "OIDCProviderArn=${ALIBABA_CLOUD_OIDC_PROVIDER_ARN}" \
        --data-urlencode "OIDCToken=${oidc_token}" \
        -d "RoleSessionName=hiclaw-oss-session" \
        -d "DurationSeconds=3600" \
        "${policy_args[@]}" \
        --connect-timeout 10 --max-time 30 2>&1)

    http_code=$(echo "${sts_resp}" | tail -1)
    sts_resp=$(echo "${sts_resp}" | sed '$d')

    if [ "${http_code}" != "200" ]; then
        echo "[oss-credentials] ERROR: STS request failed (HTTP ${http_code})" >&2
        echo "[oss-credentials] Response: ${sts_resp}" >&2
        return 1
    fi

    sts_ak=$(echo "${sts_resp}" | jq -r '.Credentials.AccessKeyId')
    sts_sk=$(echo "${sts_resp}" | jq -r '.Credentials.AccessKeySecret')
    sts_token=$(echo "${sts_resp}" | jq -r '.Credentials.SecurityToken')

    if [ -z "${sts_ak}" ] || [ "${sts_ak}" = "null" ]; then
        echo "[oss-credentials] ERROR: Failed to parse STS credentials" >&2
        echo "[oss-credentials] Response: ${sts_resp}" >&2
        return 1
    fi

    # expires_at = now + 3600 seconds (STS token lifetime)
    expires_at=$(( $(date +%s) + 3600 ))

    cat > "${_OSS_CRED_FILE}" <<EOF
MC_HOST_hiclaw="https://${sts_ak}:${sts_sk}:${sts_token}@oss-${region}-internal.aliyuncs.com"
_OSS_CRED_EXPIRES_AT=${expires_at}
EOF
    chmod 600 "${_OSS_CRED_FILE}"

    echo "[oss-credentials] STS credentials refreshed (AK prefix: ${sts_ak:0:8}..., expires: $(date -d @${expires_at} '+%H:%M:%S' 2>/dev/null || date -r ${expires_at} '+%H:%M:%S' 2>/dev/null || echo ${expires_at}))" >&2
}

# Public: ensure MC_HOST_hiclaw is set with valid (non-expired) STS credentials.
# In local mode (no OIDC env vars), this is a no-op.
ensure_mc_credentials() {
    # Skip in local mode — mc alias is configured with static credentials
    if [ -z "${ALIBABA_CLOUD_OIDC_TOKEN_FILE:-}" ] || [ ! -f "${ALIBABA_CLOUD_OIDC_TOKEN_FILE:-/nonexistent}" ]; then
        return 0
    fi

    local now needs_refresh=false
    now=$(date +%s)

    if [ -f "${_OSS_CRED_FILE}" ]; then
        # Source to get _OSS_CRED_EXPIRES_AT
        . "${_OSS_CRED_FILE}"
        if [ -z "${_OSS_CRED_EXPIRES_AT:-}" ] || [ $(( _OSS_CRED_EXPIRES_AT - now )) -lt ${_OSS_CRED_REFRESH_MARGIN} ]; then
            needs_refresh=true
        fi
    else
        needs_refresh=true
    fi

    if [ "${needs_refresh}" = true ]; then
        _oss_refresh_sts || return 1
        . "${_OSS_CRED_FILE}"
    fi

    export MC_HOST_hiclaw
}
