#!/bin/bash
# bootstrap/local.sh - Embedded/local infrastructure setup

bootstrap_setup_local() {
    if ! is_local_runtime; then
        bootstrap_wait_cloud_tuwunel
        return 0
    fi

    if [ -d "/host-share" ]; then
        ORIGINAL_HOST_HOME="${HOST_ORIGINAL_HOME:-$HOME}"
        case "${ORIGINAL_HOST_HOME}" in
            /*)
                if [ ! -e "${ORIGINAL_HOST_HOME}" ] && [ "${ORIGINAL_HOST_HOME}" != "/" ] && [ "${ORIGINAL_HOST_HOME}" != "/root" ] && [ "${ORIGINAL_HOST_HOME}" != "/data" ] && [ "${ORIGINAL_HOST_HOME}" != "/host-share" ]; then
                    mkdir -p "$(dirname "${ORIGINAL_HOST_HOME}")"
                    ln -sfn /host-share "${ORIGINAL_HOST_HOME}"
                    log "Created symlink: ${ORIGINAL_HOST_HOME} -> /host-share"
                else
                    ln -sfn /host-share /root/host-home
                    log "Created fallback symlink: /root/host-home -> /host-share"
                fi
                ;;
            *)
                ln -sfn /host-share /root/host-home
                log "HOST_ORIGINAL_HOME ('${ORIGINAL_HOST_HOME}') is not an absolute POSIX path; created fallback symlink: /root/host-home -> /host-share"
                ;;
        esac
    fi

    local HOSTS_DOMAINS="${MATRIX_DOMAIN%%:*} ${AGENTTEAMS_MATRIX_CLIENT_DOMAIN:-matrix-client-local.agentteams.io} ${AI_GATEWAY_DOMAIN} ${AGENTTEAMS_FS_DOMAIN:-fs-local.agentteams.io}"
    if ! grep -q "${AI_GATEWAY_DOMAIN}" /etc/hosts 2>/dev/null; then
        echo "127.0.0.1 ${HOSTS_DOMAINS}" >> /etc/hosts
        log "Added local domains to /etc/hosts"
    fi

    waitForService "Higress Gateway" "127.0.0.1" 8080 180
    waitForService "Higress Console" "127.0.0.1" 8001 180
    waitForService "Tuwunel" "127.0.0.1" 6167 120
    waitForHTTP "Tuwunel Matrix API" "${AGENTTEAMS_MATRIX_URL}/_tuwunel/server_version" 120
    waitForService "MinIO" "127.0.0.1" 9000 120
}
