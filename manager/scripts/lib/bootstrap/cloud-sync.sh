#!/bin/bash
# bootstrap/cloud-sync.sh - Parameterized cloud workspace sync loops (aliyun + k8s)

bootstrap_start_cloud_sync() {
    if [ "${AGENTTEAMS_RUNTIME}" != "aliyun" ] && [ "${AGENTTEAMS_RUNTIME}" != "k8s" ]; then
        return 0
    fi

    local storage_label remote_label
    if [ "${AGENTTEAMS_RUNTIME}" = "aliyun" ]; then
        storage_label="OSS"
        remote_label="OSS"
    else
        storage_label="MinIO"
        remote_label="MinIO"
    fi

    log "Syncing initial workspace to ${storage_label}..."
    if [ "${AGENTTEAMS_RUNTIME}" = "aliyun" ]; then
        ensure_mc_credentials
    fi

    local -a push_excludes=(
        "--exclude" ".cache/**"
        "--exclude" ".npm/**"
        "--exclude" ".local/**"
        "--exclude" ".mc/**"
    )
    if [ "${MANAGER_RUNTIME}" = "copaw" ]; then
        push_excludes+=("--exclude" ".copaw/**")
    else
        push_excludes+=("--exclude" ".openclaw/**")
    fi

    mc mirror /root/manager-workspace/ "${AGENTTEAMS_STORAGE_PREFIX}/manager/" --overwrite \
        "${push_excludes[@]}" 2>/dev/null || true

    (
        local -a loop_excludes=("${push_excludes[@]}")
        while true; do
            local CHANGED
            CHANGED=$(find /root/manager-workspace/ -type f -newermt "15 seconds ago" 2>/dev/null | head -1)
            if [ -n "${CHANGED}" ]; then
                if [ "${AGENTTEAMS_RUNTIME}" = "aliyun" ]; then
                    ensure_mc_credentials 2>/dev/null || true
                fi
                mc mirror /root/manager-workspace/ "${AGENTTEAMS_STORAGE_PREFIX}/manager/" --overwrite \
                    "${loop_excludes[@]}" 2>/dev/null || true
            fi
            sleep 10
        done
    ) &
    log "Local→${remote_label} sync started (PID: $!)"

    (
        while true; do
            sleep 60
            if [ "${AGENTTEAMS_RUNTIME}" = "aliyun" ]; then
                ensure_mc_credentials 2>/dev/null || true
            fi
            mc mirror "${AGENTTEAMS_STORAGE_PREFIX}/shared/" /root/hiclaw-fs/shared/ --overwrite --newer-than "1m" 2>/dev/null || true
            mc mirror "${AGENTTEAMS_STORAGE_PREFIX}/agents/" /root/hiclaw-fs/agents/ --overwrite --newer-than "1m" 2>/dev/null || true
            if [ "${AGENTTEAMS_RUNTIME}" = "k8s" ]; then
                mc mirror "${AGENTTEAMS_STORAGE_PREFIX}/agentteams-config/" /root/hiclaw-fs/agentteams-config/ --overwrite --newer-than "1m" 2>/dev/null || true
            fi
            mc cp "${AGENTTEAMS_STORAGE_PREFIX}/manager/openclaw.json" /root/manager-workspace/openclaw.json 2>/dev/null || true
        done
    ) &
    log "${remote_label}→Local sync started (every 60s, PID: $!)"
}
