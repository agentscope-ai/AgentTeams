#!/bin/bash
# bootstrap/cloud-validate.sh - Cloud/K8s env validation and Tuwunel wait

bootstrap_validate_cloud() {
    if ! is_cloud_runtime; then
        return 0
    fi

    : "${AGENTTEAMS_MATRIX_URL:?AGENTTEAMS_MATRIX_URL is required}"
    : "${AGENTTEAMS_MATRIX_DOMAIN:?AGENTTEAMS_MATRIX_DOMAIN is required}"
    : "${AGENTTEAMS_AI_GATEWAY_URL:?AGENTTEAMS_AI_GATEWAY_URL is required}"
    if [ "${AGENTTEAMS_RUNTIME}" = "aliyun" ]; then
        : "${AGENTTEAMS_MANAGER_GATEWAY_KEY:?AGENTTEAMS_MANAGER_GATEWAY_KEY is required}"
        : "${AGENTTEAMS_MANAGER_PASSWORD:?AGENTTEAMS_MANAGER_PASSWORD is required (cloud containers are stateless, password must be injected)}"
    fi
    if [ "${AGENTTEAMS_RUNTIME}" = "k8s" ]; then
        : "${AGENTTEAMS_MANAGER_GATEWAY_KEY:?AGENTTEAMS_MANAGER_GATEWAY_KEY is required (injected by controller)}"
    else
        : "${AGENTTEAMS_REGISTRATION_TOKEN:?AGENTTEAMS_REGISTRATION_TOKEN is required}"
        : "${AGENTTEAMS_ADMIN_USER:?AGENTTEAMS_ADMIN_USER is required}"
        : "${AGENTTEAMS_ADMIN_PASSWORD:?AGENTTEAMS_ADMIN_PASSWORD is required}"
    fi
    log "${AGENTTEAMS_RUNTIME} mode: validating environment... OK"
    log "  Matrix: ${AGENTTEAMS_MATRIX_URL}, AI Gateway: ${AGENTTEAMS_AI_GATEWAY_URL}, Storage: ${AGENTTEAMS_FS_BUCKET}"
    if [ "${AGENTTEAMS_RUNTIME}" = "aliyun" ]; then
        ensure_mc_credentials || { log "FATAL: Initial STS credential fetch failed"; exit 1; }
    fi
}

bootstrap_wait_cloud_tuwunel() {
    log "Waiting for Tuwunel Matrix server at ${AGENTTEAMS_MATRIX_URL}..."
    local _retry=0
    while [ "${_retry}" -lt 30 ]; do
        if curl -sf "${AGENTTEAMS_MATRIX_URL}/_matrix/client/versions" > /dev/null 2>&1; then
            log "Tuwunel is ready"
            return 0
        fi
        _retry=$((_retry + 1))
        log "  Waiting for Tuwunel (attempt ${_retry}/30)..."
        sleep 5
    done
    log "ERROR: Tuwunel not reachable at ${AGENTTEAMS_MATRIX_URL}"
    exit 1
}
