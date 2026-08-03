#!/bin/bash
# migrate-copaw-state.sh - One-shot .copaw → .qwenpaw runtime state migration
#
# Extracted from start-qwenpaw-manager.sh §10b so that CI can E2E-test the
# legacy CoPaw upgrade path directly. Supports an existing legacy install
# (AgentTeams with CoPaw runtime) upgrading in place to QwenPaw 2.0
# WITHOUT losing runtime state (sessions, memory, history, config, channels,
# plugins, credentials).
#
# Migration is a copy-then-verify flow:
#   * Every critical artifact that exists in the legacy location MUST be
#     verified in the target (content-equality via cmp) before the marker
#     is written.
#   * Conflict policy: the legacy location is authoritative on upgrade, so
#     legacy artifacts OVERWRITE any pre-existing target file (cp -a, not
#     cp -an — no-clobber would silently skip a pre-existing target and
#     permanently lose the legacy value).
#   * A partial copy (missing credentials or sessions, or a content
#     mismatch) must NOT be marked complete, otherwise the upgrade
#     permanently loses runtime state and retries never run.
#   * Idempotent: once .copaw-migrated exists, migration is skipped.
#
# Usage (environment overrides make this directly testable):
#   HOME=... QWENPAW_WORKING_DIR=... migrate-copaw-state.sh
#
# Environment:
#   HOME                 legacy state root (~/.copaw, ~/.copaw.secret)
#                        default: $HOME
#   QWENPAW_WORKING_DIR  target root (default: ${HOME}/.qwenpaw)
#   QWENPAW_SECRET_DIR   target secret root (default: ${QWENPAW_WORKING_DIR}.secret)
#   WORKSPACE_DIR        target workspace dir (default: ${QWENPAW_WORKING_DIR}/workspaces/default)
#
# Exit codes:
#   0  migration completed (marker written) OR no legacy state OR already migrated
#   1  migration incomplete (partial copy) — marker NOT written, retry on next boot

source /opt/agentteams/scripts/lib/agentteams-env.sh

QWENPAW_WORKING_DIR="${QWENPAW_WORKING_DIR:-${HOME}/.qwenpaw}"
QWENPAW_SECRET_DIR="${QWENPAW_SECRET_DIR:-${QWENPAW_WORKING_DIR}.secret}"
WORKSPACE_DIR="${WORKSPACE_DIR:-${QWENPAW_WORKING_DIR}/workspaces/default}"

LEGACY_COPAW_DIR="${HOME}/.copaw"
LEGACY_COPAW_SECRET="${HOME}/.copaw.secret"
MIGRATION_FLAG="${QWENPAW_WORKING_DIR}/.copaw-migrated"

# No legacy state → nothing to do (fresh install).
if [ ! -d "${LEGACY_COPAW_DIR}" ]; then
    log "No legacy .copaw state found — migration not needed"
    exit 0
fi

# Already migrated → idempotent skip.
if [ -f "${MIGRATION_FLAG}" ]; then
    log "Migration already completed, skipping"
    exit 0
fi

log "Migrating runtime state from ${LEGACY_COPAW_DIR} to ${QWENPAW_WORKING_DIR}..."

# Target root must exist before top-level files are copied into it.
mkdir -p "${QWENPAW_WORKING_DIR}"

_migration_failed=false

# Conflict policy: on upgrade the legacy location is the authoritative
# source of user data, so state/secret artifacts OVERWRITE any pre-existing
# target file (cp -a, not cp -an — no-clobber would silently skip a
# pre-existing target and permanently lose the legacy value). Every copied
# artifact is then content-verified with cmp before the marker is written.
_migrate_file() {
    _src="$1"
    _dst_dir="$2"
    if [ ! -e "${_src}" ]; then
        return 0
    fi
    _name=$(basename "${_src}")
    if ! cp -a "${_src}" "${_dst_dir}/" 2>/dev/null; then
        log "WARNING: failed to copy ${_src} — migration incomplete, will retry"
        _migration_failed=true
        return 1
    fi
    if ! cmp -s "${_src}" "${_dst_dir}/${_name}"; then
        log "WARNING: ${_name} content mismatch after copy — migration incomplete, will retry"
        _migration_failed=true
        return 1
    fi
    return 0
}

_migrate_dir() {
    _src="$1"
    _dst_dir="$2"
    if [ ! -d "${_src}" ]; then
        return 0
    fi
    mkdir -p "${_dst_dir}"
    if ! cp -a "${_src}/." "${_dst_dir}/" 2>/dev/null; then
        log "WARNING: failed to copy directory ${_src} — migration incomplete, will retry"
        _migration_failed=true
        return 1
    fi
    # Verify every source entry arrived with identical content. A no-clobber
    # or partial copy must not be marked complete.
    _missing=0
    while IFS= read -r -d '' _f; do
        _rel="${_f#"${_src}"/}"
        if [ ! -e "${_dst_dir}/${_rel}" ]; then
            log "WARNING: ${_rel} missing after copy — migration incomplete, will retry"
            _missing=1
        elif [ -f "${_src}/${_rel}" ] && ! cmp -s "${_src}/${_rel}" "${_dst_dir}/${_rel}"; then
            log "WARNING: ${_rel} content mismatch after copy — migration incomplete, will retry"
            _missing=1
        fi
    done < <(find "${_src}" -type f -print0)
    if [ "${_missing}" = "1" ]; then
        _migration_failed=true
    fi
    return 0
}

# Top-level state files (chats.json, history.db, config.json)
for _state in chats.json history.db config.json; do
    _src="${LEGACY_COPAW_DIR}/${_state}"
    if [ -e "${_src}" ]; then
        _migrate_file "${_src}" "${QWENPAW_WORKING_DIR}"
    fi
done

# Top-level state directories (memory/, digest/)
for _state in memory digest; do
    _src="${LEGACY_COPAW_DIR}/${_state}"
    if [ -d "${_src}" ]; then
        _migrate_dir "${_src}" "${QWENPAW_WORKING_DIR}/${_state}"
    fi
done

# Directories that contain runtime state
for _subdir in custom_channels plugins models sessions; do
    _src="${LEGACY_COPAW_DIR}/${_subdir}"
    if [ -d "${_src}" ]; then
        _migrate_dir "${_src}" "${QWENPAW_WORKING_DIR}/${_subdir}"
    fi
done

# Secret directory (sibling: ~/.copaw.secret -> ~/.qwenpaw.secret)
# Contains master_key, providers.json, envs.json — critical for credentials
mkdir -p "${QWENPAW_SECRET_DIR}"
if [ -d "${LEGACY_COPAW_SECRET}" ]; then
    for _secret_file in master_key providers.json envs.json; do
        _src_secret="${LEGACY_COPAW_SECRET}/${_secret_file}"
        if [ -e "${_src_secret}" ]; then
            _migrate_file "${_src_secret}" "${QWENPAW_SECRET_DIR}"
        fi
    done
fi

# Workspace files (SOUL.md, AGENTS.md, skills/, agent.json, etc.)
# Same conflict policy: legacy workspace is authoritative on upgrade, so
# overwrite and verify content.
_legacy_ws="${LEGACY_COPAW_DIR}/workspaces/default"
if [ -d "${_legacy_ws}" ]; then
    mkdir -p "${WORKSPACE_DIR}"
    if ! cp -a "${_legacy_ws}/." "${WORKSPACE_DIR}/" 2>/dev/null; then
        log "WARNING: failed to copy workspace ${_legacy_ws} — migration incomplete, will retry"
        _migration_failed=true
    fi
    # Verify critical workspace artifacts arrived with identical content.
    for _ws_file in SOUL.md AGENTS.md agent.json; do
        if [ -e "${_legacy_ws}/${_ws_file}" ]; then
            if [ ! -e "${WORKSPACE_DIR}/${_ws_file}" ]; then
                log "WARNING: workspace ${_ws_file} missing after copy — migration incomplete, will retry"
                _migration_failed=true
            elif ! cmp -s "${_legacy_ws}/${_ws_file}" "${WORKSPACE_DIR}/${_ws_file}"; then
                log "WARNING: workspace ${_ws_file} content mismatch after copy — migration incomplete, will retry"
                _migration_failed=true
            fi
        fi
    done
fi

if [ "${_migration_failed}" = "false" ]; then
    touch "${MIGRATION_FLAG}"
    log "Migration complete (flag: ${MIGRATION_FLAG})"
    exit 0
else
    log "Migration will retry on next startup"
    exit 1
fi
