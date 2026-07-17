#!/bin/bash
# bootstrap/secrets.sh - Auto-generate and persist Manager secrets

bootstrap_manage_secrets() {
    SECRETS_FILE="/data/hiclaw-secrets.env"
    if [ -f "${SECRETS_FILE}" ]; then
        # shellcheck disable=SC1090
        source "${SECRETS_FILE}"
        log "Loaded persisted secrets from ${SECRETS_FILE}"
    fi

    if [ -z "${AGENTTEAMS_MANAGER_GATEWAY_KEY}" ]; then
        export AGENTTEAMS_MANAGER_GATEWAY_KEY="$(generateKey 32)"
        log "Auto-generated AGENTTEAMS_MANAGER_GATEWAY_KEY"
    fi
    if [ -z "${AGENTTEAMS_MANAGER_PASSWORD}" ]; then
        export AGENTTEAMS_MANAGER_PASSWORD="$(generateKey 16)"
        log "Auto-generated AGENTTEAMS_MANAGER_PASSWORD"
    fi

    mkdir -p /data
    cat > "${SECRETS_FILE}" <<EOF
export AGENTTEAMS_MANAGER_GATEWAY_KEY="${AGENTTEAMS_MANAGER_GATEWAY_KEY}"
export AGENTTEAMS_MANAGER_PASSWORD="${AGENTTEAMS_MANAGER_PASSWORD}"
EOF
    chmod 600 "${SECRETS_FILE}"
}

bootstrap_pull_cloud_workspace() {
    if [ "${AGENTTEAMS_RUNTIME}" = "aliyun" ]; then
        AGENTTEAMS_FS="/root/hiclaw-fs"
        mkdir -p "${AGENTTEAMS_FS}/shared" "${AGENTTEAMS_FS}/agents"
        log "Pulling workspace from OSS..."
        ensure_mc_credentials
        mc mirror "${AGENTTEAMS_STORAGE_PREFIX}/manager/" /root/manager-workspace/ --overwrite 2>/dev/null || true
        mc mirror "${AGENTTEAMS_STORAGE_PREFIX}/shared/" "${AGENTTEAMS_FS}/shared/" --overwrite 2>/dev/null || true
        mc mirror "${AGENTTEAMS_STORAGE_PREFIX}/agents/" "${AGENTTEAMS_FS}/agents/" --overwrite 2>/dev/null || true
        ln -sfn "${AGENTTEAMS_FS}" /root/manager-workspace/hiclaw-fs
    fi

    if [ "${AGENTTEAMS_RUNTIME}" = "k8s" ]; then
        AGENTTEAMS_FS="/root/hiclaw-fs"
        mkdir -p "${AGENTTEAMS_FS}/shared" "${AGENTTEAMS_FS}/agents" "${AGENTTEAMS_FS}/agentteams-config"
        log "Configuring mc alias for cluster MinIO..."
        mc alias set agentteams "${AGENTTEAMS_FS_ENDPOINT}" "${AGENTTEAMS_FS_ACCESS_KEY}" "${AGENTTEAMS_FS_SECRET_KEY}"
        log "Syncing workspace from MinIO..."
        mc mirror "${AGENTTEAMS_STORAGE_PREFIX}/manager/" /root/manager-workspace/ --overwrite 2>/dev/null || true
        mc mirror "${AGENTTEAMS_STORAGE_PREFIX}/" "${AGENTTEAMS_FS}/" --overwrite 2>/dev/null || true
        ln -sfn "${AGENTTEAMS_FS}" /root/manager-workspace/hiclaw-fs
        touch "${AGENTTEAMS_FS}/.initialized"
    fi
}
