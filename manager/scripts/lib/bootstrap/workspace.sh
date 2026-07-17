#!/bin/bash
# bootstrap/workspace.sh - Manager workspace init/upgrade

bootstrap_init_workspace() {
    mkdir -p /root/manager-workspace

    IMAGE_VERSION=$(cat /opt/hiclaw/agent/.builtin-version 2>/dev/null || echo "unknown")
    INSTALLED_VERSION=$(cat /root/manager-workspace/.builtin-version 2>/dev/null || echo "")

    if [ ! -f /root/manager-workspace/.initialized ]; then
        log "First boot: initializing manager workspace..."
        bash /opt/hiclaw/scripts/init/upgrade-builtins.sh
        touch /root/manager-workspace/.initialized
        log "Manager workspace initialized (version: ${IMAGE_VERSION})"
    elif [ "${IMAGE_VERSION}" != "${INSTALLED_VERSION}" ] || [ "${IMAGE_VERSION}" = "latest" ]; then
        log "Upgrade detected: ${INSTALLED_VERSION} -> ${IMAGE_VERSION}${IMAGE_VERSION:+ (latest: always upgrade)}"
        bash /opt/hiclaw/scripts/init/upgrade-builtins.sh
        log "Manager workspace upgraded to version: ${IMAGE_VERSION}"
    else
        log "Workspace up to date (version: ${IMAGE_VERSION})"
    fi

    if is_local_runtime; then
        log "Waiting for MinIO storage initialization..."
        local _minio_wait=0
        while [ ! -f /root/hiclaw-fs/.initialized ]; do
            sleep 2
            _minio_wait=$(( _minio_wait + 1 ))
            if [ "${_minio_wait}" -ge 60 ]; then
                log "ERROR: MinIO storage initialization timed out after 120s"
                exit 1
            fi
        done
        log "MinIO storage initialized"
    fi
}
