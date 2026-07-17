#!/bin/bash
# bootstrap/container-runtime.sh - Detect Worker creation backend

bootstrap_detect_container_runtime() {
    source /opt/hiclaw/scripts/lib/container-api.sh
    if container_api_available; then
        log "Container runtime socket detected at ${CONTAINER_SOCKET} — direct Worker creation enabled"
        export AGENTTEAMS_CONTAINER_RUNTIME="socket"
    elif is_cloud_runtime; then
        log "Cloud/K8s mode — Workers created via controller API"
        export AGENTTEAMS_CONTAINER_RUNTIME="cloud"
    else
        log "No container runtime found — Worker creation will output install commands"
        export AGENTTEAMS_CONTAINER_RUNTIME="none"
    fi
}
