#!/bin/bash
# Select the storage paths mirrored by the embedded Controller or legacy Manager.

agentteams_mirror_initial() {
    local scope="$1"
    local storage_prefix="$2"
    local local_root="$3"

    if [ "${scope}" = "controller" ]; then
        mc mirror \
            "${storage_prefix}/agentteams-config/" \
            "${local_root}/agentteams-config/" \
            --overwrite
        return
    fi

    mc mirror "${storage_prefix}/" "${local_root}/" --overwrite
}

agentteams_mirror_fallback() {
    local scope="$1"
    local storage_prefix="$2"
    local local_root="$3"

    [ "${scope}" != "controller" ] || return 0
    mc mirror "${storage_prefix}/" "${local_root}/" --overwrite --newer-than "5m"
}
