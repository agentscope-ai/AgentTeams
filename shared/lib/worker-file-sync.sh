#!/bin/bash
# Runtime-neutral Local -> Remote Worker workspace synchronization.

worker_sync_init() {
    local state_dir="$1"
    local reference_marker="${2:-}"

    mkdir -p "${state_dir}"
    if [ -n "${reference_marker}" ] && [ -e "${reference_marker}" ]; then
        touch -r "${reference_marker}" "${state_dir}/last-successful-push"
    else
        touch "${state_dir}/last-successful-push"
    fi
}

worker_sync_mark_remote_pull() {
    local state_dir="$1"

    mkdir -p "${state_dir}"
    touch "${state_dir}/last-successful-push"
}

worker_sync_should_push() {
    local relative_path="$1"

    case "${relative_path}" in
        openclaw.json | config/mcporter.json | mcporter-servers.json | \
            credentials/* | .agents/* | .cache/* | .npm/* | .local/* | .mc/* | \
            .last-pull | .openclaw/matrix/* | .openclaw/canvas/* | *.lock)
            return 1
            ;;
    esac
    return 0
}

worker_sync_mirror_all() {
    local workspace="$1"
    local remote_prefix="$2"

    mc mirror "${workspace}/" "${remote_prefix}/" --overwrite \
        --exclude "openclaw.json" \
        --exclude "config/mcporter.json" --exclude "mcporter-servers.json" --exclude ".agents/**" \
        --exclude "credentials/**" \
        --exclude ".cache/**" --exclude ".npm/**" \
        --exclude ".local/**" --exclude ".mc/**" --exclude "*.lock" \
        --exclude ".last-pull" \
        --exclude ".openclaw/matrix/**" --exclude ".openclaw/canvas/**"
}

worker_sync_push_once() {
    local workspace="$1"
    local remote_prefix="$2"
    local state_dir="$3"
    local push_marker="${state_dir}/last-successful-push"
    local cycle_start manifest upload_manifest file relative_path
    local changed_count=0
    local mirror_threshold="${AGENTTEAMS_WORKER_SYNC_MIRROR_THRESHOLD:-128}"

    case "${mirror_threshold}" in
        '' | *[!0-9]*) mirror_threshold=128 ;;
    esac

    cycle_start="$(mktemp "${state_dir}/cycle-start.XXXXXX")" || return 1
    if ! manifest="$(mktemp "${state_dir}/changed.XXXXXX")"; then
        rm -f "${cycle_start}"
        return 1
    fi
    if ! upload_manifest="$(mktemp "${state_dir}/upload.XXXXXX")"; then
        rm -f "${cycle_start}" "${manifest}"
        return 1
    fi
    if ! find "${workspace}" -type f -newer "${push_marker}" ! -newer "${cycle_start}" -print0 \
        > "${manifest}"; then
        rm -f "${cycle_start}" "${manifest}" "${upload_manifest}"
        return 1
    fi

    while IFS= read -r -d '' file; do
        [ -f "${file}" ] || continue
        relative_path="${file#"${workspace}"/}"
        worker_sync_should_push "${relative_path}" || continue
        printf '%s\0' "${file}" >> "${upload_manifest}"
        changed_count=$((changed_count + 1))
    done < "${manifest}"

    if [ "${changed_count}" -gt 0 ] && type ensure_mc_credentials >/dev/null 2>&1; then
        ensure_mc_credentials 2>/dev/null || true
    fi

    if [ "${changed_count}" -gt "${mirror_threshold}" ]; then
        if ! worker_sync_mirror_all "${workspace}" "${remote_prefix}"; then
            rm -f "${cycle_start}" "${manifest}" "${upload_manifest}"
            return 1
        fi
    else
        while IFS= read -r -d '' file; do
            [ -f "${file}" ] || continue
            relative_path="${file#"${workspace}"/}"
            if ! mc cp "${file}" "${remote_prefix}/${relative_path}"; then
                rm -f "${cycle_start}" "${manifest}" "${upload_manifest}"
                return 1
            fi
        done < "${upload_manifest}"
    fi

    if [ "${cycle_start}" -nt "${push_marker}" ]; then
        mv "${cycle_start}" "${push_marker}"
    else
        rm -f "${cycle_start}"
    fi
    rm -f "${manifest}" "${upload_manifest}"
}
