#!/bin/bash
# agentteams-env.sh - Unified environment bootstrap for AgentTeams scripts
#
# Single source of truth for both Manager and Worker containers.
# Source this file instead of manually setting up Matrix/storage variables.
#
# Provides:
#   AGENTTEAMS_RUNTIME         — "aliyun" | "k8s" | "docker" | "none"
#   AGENTTEAMS_MATRIX_URL      — Matrix server URL (works in both local and cloud)
#   AGENTTEAMS_AI_GATEWAY_URL  — AI Gateway base URL
#   AGENTTEAMS_FS_BUCKET       — bucket name for mc commands
#   AGENTTEAMS_STORAGE_ALIAS   — mc alias name used in AGENTTEAMS_STORAGE_PREFIX
#   AGENTTEAMS_STORAGE_PREFIX  — "<mc-alias>/<bucket>" ready for mc paths
#   ensure_mc_credentials  — callable function (no-op in local mode)
#
# Usage:
#   source /opt/agentteams/scripts/lib/agentteams-env.sh

# ── Optional dependencies ─────────────────────────────────────────────────────
# Prepare canonical AgentTeams paths and legacy aliases before other scripts use them.
source /opt/agentteams/scripts/lib/agentteams-paths.sh 2>/dev/null || true
agentteams_prepare_paths 2>/dev/null || true

# base.sh provides log(), waitForService(), generateKey() — Manager-only.
# Worker images don't ship base.sh; the silent fallback is intentional.
source /opt/agentteams/scripts/lib/base.sh 2>/dev/null || true

# ── Worker deps env projection ────────────────────────────────────────────────
# SandboxSet pool workers start from identityless warm instances. The claim-time
# dynamic mount projects Worker-specific env into this directory. In the sandbox
# pool path, AGENTTEAMS_WORKER_ENV_MOUNT_REQUIRED=1 makes this file a startup
# prerequisite.
_agentteams_truthy() {
    case "$(printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]')" in
        1|true|yes|on) return 0 ;;
        *) return 1 ;;
    esac
}

_agentteams_fail_source() {
    echo "[agentteams-env] ERROR: $1" >&2
    exit 1
}

_agentteams_wait_for_file() {
    local file="$1"
    local label="$2"
    local timeout="${AGENTTEAMS_WORKER_ENV_MOUNT_TIMEOUT_SECONDS:-300}"
    local elapsed=0

    while [ ! -f "${file}" ]; do
        if [ "${elapsed}" -ge "${timeout}" ]; then
            _agentteams_fail_source "timed out waiting for ${label}: ${file}"
        fi
        sleep 1
        elapsed=$((elapsed + 1))
    done
}

_agentteams_source_env_file() {
    local file="$1"
    local had_errexit=false

    case "$-" in
        *e*)
            had_errexit=true
            set +e
            ;;
    esac

    set -a
    # The file is controller-generated shell export syntax.
    # shellcheck disable=SC1090
    . "${file}"
    local rc=$?
    set +a

    if [ "${had_errexit}" = "true" ]; then
        set -e
    fi

    return "${rc}"
}

_agentteams_required_worker_env_ready() {
    [ -n "${AGENTTEAMS_WORKER_NAME:-}" ] && [ -n "${AGENTTEAMS_AUTH_TOKEN_FILE:-}" ]
}

_agentteams_wait_for_worker_env_ready() {
    local file="$1"
    local timeout="${AGENTTEAMS_WORKER_ENV_MOUNT_TIMEOUT_SECONDS:-300}"
    local elapsed=0
    local last_error="worker env file not found"

    while true; do
        if [ -f "${file}" ]; then
            if _agentteams_source_env_file "${file}"; then
                if _agentteams_required_worker_env_ready; then
                    return 0
                fi
                last_error="missing AGENTTEAMS_WORKER_NAME or AGENTTEAMS_AUTH_TOKEN_FILE"
            else
                last_error="failed to source worker env file"
            fi
        fi

        if [ "${elapsed}" -ge "${timeout}" ]; then
            _agentteams_fail_source "timed out waiting for worker env values in ${file}: ${last_error}"
        fi
        sleep 1
        elapsed=$((elapsed + 1))
    done
}

_agentteams_env_mount_required=false
if _agentteams_truthy "${AGENTTEAMS_WORKER_ENV_MOUNT_REQUIRED:-}"; then
    _agentteams_env_mount_required=true
fi

if [ "${_agentteams_env_mount_required}" = "true" ] && [ -z "${AGENTTEAMS_WORKER_ENV_MOUNT_DIR:-}" ]; then
    _agentteams_fail_source "AGENTTEAMS_WORKER_ENV_MOUNT_DIR is required when AGENTTEAMS_WORKER_ENV_MOUNT_REQUIRED=1"
fi

if [ -n "${AGENTTEAMS_WORKER_ENV_MOUNT_DIR:-}" ]; then
    _agentteams_env_file="${AGENTTEAMS_WORKER_ENV_MOUNT_DIR%/}/env"
    if [ "${_agentteams_env_mount_required}" = "true" ]; then
        _agentteams_wait_for_worker_env_ready "${_agentteams_env_file}"
    elif [ -f "${_agentteams_env_file}" ]; then
        if ! _agentteams_source_env_file "${_agentteams_env_file}"; then
            _agentteams_fail_source "failed to source worker env file: ${_agentteams_env_file}"
        fi
    fi
    unset _agentteams_env_file
fi

if [ "${_agentteams_env_mount_required}" = "true" ]; then
    if [ -z "${AGENTTEAMS_AUTH_TOKEN_FILE:-}" ]; then
        _agentteams_fail_source "AGENTTEAMS_AUTH_TOKEN_FILE is required after loading worker env"
    fi
    _agentteams_wait_for_file "${AGENTTEAMS_AUTH_TOKEN_FILE}" "worker auth token file"
fi
unset _agentteams_env_mount_required

# ── Runtime detection ─────────────────────────────────────────────────────────
# AGENTTEAMS_RUNTIME is normally pre-set by the deployment (Helm sets "k8s",
# Dockerfile.aliyun sets "aliyun", local scripts leave it unset).
# Only a minimal fallback is done here; cloud mode must be set explicitly.
AGENTTEAMS_RUNTIME="${AGENTTEAMS_RUNTIME:-}"
AGENTTEAMS_CONTAINER_SOCKET="${AGENTTEAMS_CONTAINER_SOCKET:-}"

if [ -z "${AGENTTEAMS_RUNTIME:-}" ]; then
    if [ -S "${AGENTTEAMS_CONTAINER_SOCKET:-/var/run/docker.sock}" ]; then
        AGENTTEAMS_RUNTIME="docker"
    else
        AGENTTEAMS_RUNTIME="none"
    fi
fi

# ── Normalized variables ──────────────────────────────────────────────────────
# AgentTeams runtime contract with local defaults.
AGENTTEAMS_MATRIX_URL="${AGENTTEAMS_MATRIX_URL:-http://127.0.0.1:6167}"
AGENTTEAMS_MATRIX_DOMAIN="${AGENTTEAMS_MATRIX_DOMAIN:-matrix-local.agentteams.io:8080}"
AGENTTEAMS_AI_GATEWAY_DOMAIN="${AGENTTEAMS_AI_GATEWAY_DOMAIN:-aigw-local.agentteams.io}"
AGENTTEAMS_AI_GATEWAY_URL="${AGENTTEAMS_AI_GATEWAY_URL:-http://${AGENTTEAMS_AI_GATEWAY_DOMAIN}:8080}"
AGENTTEAMS_FS_DOMAIN="${AGENTTEAMS_FS_DOMAIN:-fs-local.agentteams.io}"
AGENTTEAMS_FS_ENDPOINT="${AGENTTEAMS_FS_ENDPOINT:-}"
AGENTTEAMS_FS_ACCESS_KEY="${AGENTTEAMS_FS_ACCESS_KEY:-}"
AGENTTEAMS_FS_SECRET_KEY="${AGENTTEAMS_FS_SECRET_KEY:-}"
AGENTTEAMS_FS_BUCKET="${AGENTTEAMS_FS_BUCKET:-agentteams-storage}"
_AGENTTEAMS_STORAGE_ALIAS_ENV="${AGENTTEAMS_STORAGE_ALIAS:-}"
_AGENTTEAMS_STORAGE_PREFIX_ENV="${AGENTTEAMS_STORAGE_PREFIX:-}"
AGENTTEAMS_STORAGE_ALIAS="${_AGENTTEAMS_STORAGE_ALIAS_ENV:-agentteams}"
AGENTTEAMS_STORAGE_PREFIX="${_AGENTTEAMS_STORAGE_PREFIX_ENV:-${AGENTTEAMS_STORAGE_ALIAS}/${AGENTTEAMS_FS_BUCKET}}"
if [ -z "${_AGENTTEAMS_STORAGE_ALIAS_ENV}" ]; then
    case "${AGENTTEAMS_STORAGE_PREFIX}" in
        */*) AGENTTEAMS_STORAGE_ALIAS="${AGENTTEAMS_STORAGE_PREFIX%%/*}" ;;
    esac
fi
AGENTTEAMS_STORAGE_PROVIDER="${AGENTTEAMS_STORAGE_PROVIDER:-}"
AGENTTEAMS_CONTROLLER_URL="${AGENTTEAMS_CONTROLLER_URL:-}"
AGENTTEAMS_YOLO="${AGENTTEAMS_YOLO:-}"
AGENTTEAMS_MATRIX_DEBUG="${AGENTTEAMS_MATRIX_DEBUG:-}"
AGENTTEAMS_CMS_TRACES_ENABLED="${AGENTTEAMS_CMS_TRACES_ENABLED:-}"
AGENTTEAMS_CMS_METRICS_ENABLED="${AGENTTEAMS_CMS_METRICS_ENABLED:-}"
AGENTTEAMS_CMS_ENDPOINT="${AGENTTEAMS_CMS_ENDPOINT:-}"
AGENTTEAMS_CMS_LICENSE_KEY="${AGENTTEAMS_CMS_LICENSE_KEY:-}"
AGENTTEAMS_CMS_PROJECT="${AGENTTEAMS_CMS_PROJECT:-}"
AGENTTEAMS_CMS_WORKSPACE="${AGENTTEAMS_CMS_WORKSPACE:-}"
AGENTTEAMS_CMS_SERVICE_NAME="${AGENTTEAMS_CMS_SERVICE_NAME:-}"

# ── Credential management ────────────────────────────────────────────────────
# In cloud mode, provides ensure_mc_credentials() for STS token refresh.
# In local mode, ensure_mc_credentials() is a no-op.
source /opt/agentteams/scripts/lib/oss-credentials.sh 2>/dev/null || true

agentteams_mc_host_var() {
    printf 'MC_HOST_%s' "${AGENTTEAMS_STORAGE_ALIAS:-agentteams}"
}

agentteams_mc_host_configured() {
    local var
    var="$(agentteams_mc_host_var)"
    [ -n "${!var:-}" ]
}

# Embedding model: default to Qwen3-Embedding (text-embedding-v4), overridable via env.
# Use - (not :-) so AGENTTEAMS_EMBEDDING_MODEL="" means "disabled" instead of falling back to default.
AGENTTEAMS_EMBEDDING_MODEL="${AGENTTEAMS_EMBEDDING_MODEL-text-embedding-v4}"

export AGENTTEAMS_RUNTIME AGENTTEAMS_CONTAINER_SOCKET
export AGENTTEAMS_WORKER_NAME AGENTTEAMS_WORKER_CR_NAME
export AGENTTEAMS_WORKER_GATEWAY_KEY AGENTTEAMS_WORKER_MATRIX_TOKEN
export AGENTTEAMS_FS_ENDPOINT AGENTTEAMS_FS_ACCESS_KEY AGENTTEAMS_FS_SECRET_KEY AGENTTEAMS_FS_BUCKET
export AGENTTEAMS_STORAGE_ALIAS AGENTTEAMS_STORAGE_PREFIX AGENTTEAMS_STORAGE_PROVIDER AGENTTEAMS_CONTROLLER_URL
export AGENTTEAMS_AUTH_TOKEN AGENTTEAMS_AUTH_TOKEN_FILE AGENTTEAMS_CONSOLE_PORT
export AGENTTEAMS_WORKER_ENV_MOUNT_DIR AGENTTEAMS_WORKER_ENV_MOUNT_REQUIRED
export AGENTTEAMS_MATRIX_URL AGENTTEAMS_MATRIX_DOMAIN AGENTTEAMS_AI_GATEWAY_URL AGENTTEAMS_AI_GATEWAY_DOMAIN
export AGENTTEAMS_FS_DOMAIN AGENTTEAMS_EMBEDDING_MODEL
export AGENTTEAMS_YOLO AGENTTEAMS_MATRIX_DEBUG
export AGENTTEAMS_CMS_TRACES_ENABLED AGENTTEAMS_CMS_METRICS_ENABLED AGENTTEAMS_CMS_ENDPOINT
export AGENTTEAMS_CMS_LICENSE_KEY AGENTTEAMS_CMS_PROJECT AGENTTEAMS_CMS_WORKSPACE AGENTTEAMS_CMS_SERVICE_NAME

unset _AGENTTEAMS_STORAGE_ALIAS_ENV _AGENTTEAMS_STORAGE_PREFIX_ENV
