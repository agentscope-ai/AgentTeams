#!/bin/bash
# bootstrap/runtime.sh - Manager runtime selection and early env setup

bootstrap_select_runtime() {
    MANAGER_RUNTIME="${AGENTTEAMS_MANAGER_RUNTIME:-openclaw}"
    case "${MANAGER_RUNTIME}" in
        copaw)
            log "Manager runtime: CoPaw (Python workspace)"
            ;;
        *)
            log "Manager runtime: OpenClaw (Node.js gateway)"
            MANAGER_RUNTIME="openclaw"
            ;;
    esac
    export MANAGER_RUNTIME
}

bootstrap_set_timezone() {
    if [ -n "${TZ}" ] && [ -f "/usr/share/zoneinfo/${TZ}" ]; then
        ln -sf "/usr/share/zoneinfo/${TZ}" /etc/localtime
        echo "${TZ}" > /etc/timezone
        log "Timezone set to ${TZ}"
    fi

    export MATRIX_DOMAIN="${AGENTTEAMS_MATRIX_DOMAIN:-matrix-local.agentteams.io:8080}"
    AI_GATEWAY_DOMAIN="${AGENTTEAMS_AI_GATEWAY_DOMAIN:-aigw-local.agentteams.io}"
    export AI_GATEWAY_DOMAIN
}

bootstrap_promote_yolo() {
    if [ -z "${AGENTTEAMS_YOLO:-}" ] && [ -f /root/manager-workspace/yolo-mode ]; then
        export AGENTTEAMS_YOLO=1
        log "YOLO mode marker detected at /root/manager-workspace/yolo-mode; AGENTTEAMS_YOLO=1 exported"
    fi
}
