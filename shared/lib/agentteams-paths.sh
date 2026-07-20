#!/bin/bash
# Compatibility migration for filesystem paths renamed from HiClaw to AgentTeams.

agentteams_migrate_legacy_path() {
    local legacy="$1"
    local current="$2"

    if [ -e "${legacy}" ] && [ ! -L "${legacy}" ]; then
        # Images may pre-create the canonical directory while an existing user
        # still mounts data at the legacy path. Remove only an empty canonical
        # directory so the old mount can remain the source of truth.
        if [ -d "${current}" ] && [ -d "${legacy}" ] && [ -z "$(ls -A "${current}" 2>/dev/null)" ]; then
            rmdir "${current}" 2>/dev/null || true
        fi

        if [ ! -e "${current}" ]; then
            mkdir -p "$(dirname "${current}")"
            if mv "${legacy}" "${current}" 2>/dev/null; then
                ln -s "${current}" "${legacy}"
            else
                # A bind mount cannot be moved. Point the canonical path at it.
                ln -s "${legacy}" "${current}"
            fi
            return 0
        fi
    fi

    if [ -e "${current}" ] && [ ! -e "${legacy}" ]; then
        mkdir -p "$(dirname "${legacy}")"
        ln -s "${current}" "${legacy}"
    fi
}

agentteams_prepare_paths() {
    [ "$(id -u)" -eq 0 ] || return 0

    agentteams_migrate_legacy_path /opt/hiclaw /opt/agentteams
    agentteams_migrate_legacy_path /root/hiclaw-fs /root/agentteams-fs
    agentteams_migrate_legacy_path /var/log/hiclaw /var/log/agentteams
    agentteams_migrate_legacy_path /var/run/hiclaw /var/run/agentteams
    agentteams_migrate_legacy_path /data/hiclaw-secrets.env /data/agentteams-secrets.env
}
