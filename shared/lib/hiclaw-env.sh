#!/bin/bash
# hiclaw-env.sh - Unified environment bootstrap for HiClaw scripts
#
# Single source of truth for both Manager and Worker containers.
# Source this file instead of manually setting up Matrix/storage variables.
#
# Provides:
#   HICLAW_RUNTIME / AGENTTEAMS_RUNTIME  — "aliyun" | "k8s" | "docker" | "none"
#   HICLAW_MATRIX_URL / AGENTTEAMS_MATRIX_URL — Matrix server URL
#   HICLAW_AI_GATEWAY_URL / AGENTTEAMS_AI_GATEWAY_URL — AI Gateway base URL
#   HICLAW_FS_BUCKET / AGENTTEAMS_FS_BUCKET — bucket name for mc commands
#   HICLAW_STORAGE_PREFIX / AGENTTEAMS_STORAGE_PREFIX — "hiclaw/<bucket>" for mc paths
#   ensure_mc_credentials — callable function (no-op in local mode)
#
# Usage:
#   source /opt/hiclaw/scripts/lib/hiclaw-env.sh

# ── Optional dependencies ─────────────────────────────────────────────────────
# base.sh provides log(), waitForService(), generateKey() — Manager-only.
# Worker images don't ship base.sh; the silent fallback is intentional.
source /opt/hiclaw/scripts/lib/base.sh 2>/dev/null || true

# resolve-env.sh provides resolve_env() for AGENTTEAMS_* / HICLAW_* dual-prefix resolution.
source /opt/hiclaw/scripts/lib/resolve-env.sh 2>/dev/null || source "$(dirname "${BASH_SOURCE[0]}")/resolve-env.sh" 2>/dev/null || true

# ── Runtime detection ─────────────────────────────────────────────────────────
# HICLAW_RUNTIME is normally pre-set by the deployment (Helm sets "k8s",
# Dockerfile.aliyun sets "aliyun", local scripts leave it unset).
# Only a minimal fallback is done here; cloud mode must be set explicitly.
_runtime=$(resolve_env "RUNTIME" "")
if [ -z "$_runtime" ]; then
    if [ -S "$(resolve_env "CONTAINER_SOCKET" "/var/run/docker.sock")" ]; then
        _runtime="docker"
    else
        _runtime="none"
    fi
fi
HICLAW_RUNTIME="$_runtime"
AGENTTEAMS_RUNTIME="$_runtime"
unset _runtime

# ── Normalized variables ──────────────────────────────────────────────────────
# Runtime-neutral infra contract with local defaults.
HICLAW_MATRIX_URL=$(resolve_env "MATRIX_URL" "http://127.0.0.1:6167")
HICLAW_AI_GATEWAY_URL=$(resolve_env "AI_GATEWAY_URL" "http://$(resolve_env "AI_GATEWAY_DOMAIN" "aigw-local.hiclaw.io"):8080")
HICLAW_FS_BUCKET=$(resolve_env "FS_BUCKET" "hiclaw-storage")
HICLAW_STORAGE_PREFIX=$(resolve_env "STORAGE_PREFIX" "hiclaw/${HICLAW_FS_BUCKET}")

AGENTTEAMS_MATRIX_URL="$HICLAW_MATRIX_URL"
AGENTTEAMS_AI_GATEWAY_URL="$HICLAW_AI_GATEWAY_URL"
AGENTTEAMS_FS_BUCKET="$HICLAW_FS_BUCKET"
AGENTTEAMS_STORAGE_PREFIX="$HICLAW_STORAGE_PREFIX"

# ── Credential management ────────────────────────────────────────────────────
# In cloud mode, provides ensure_mc_credentials() for STS token refresh.
# In local mode, ensure_mc_credentials() is a no-op.
source /opt/hiclaw/scripts/lib/oss-credentials.sh 2>/dev/null || true

# Embedding model: default to Qwen3-Embedding (text-embedding-v4), overridable via env.
# An explicit empty string in env means "disabled" — distinct from unset.
# resolve_env treats empty as unset, so handle this case explicitly here.
# AGENTTEAMS_ takes precedence over HICLAW_ when set (even when set to empty).
if [ "${AGENTTEAMS_EMBEDDING_MODEL+set}" = "set" ]; then
    HICLAW_EMBEDDING_MODEL="$AGENTTEAMS_EMBEDDING_MODEL"
elif [ "${HICLAW_EMBEDDING_MODEL+set}" = "set" ]; then
    AGENTTEAMS_EMBEDDING_MODEL="$HICLAW_EMBEDDING_MODEL"
else
    HICLAW_EMBEDDING_MODEL="text-embedding-v4"
    AGENTTEAMS_EMBEDDING_MODEL="text-embedding-v4"
fi

export HICLAW_RUNTIME HICLAW_MATRIX_URL HICLAW_AI_GATEWAY_URL HICLAW_FS_BUCKET HICLAW_STORAGE_PREFIX HICLAW_EMBEDDING_MODEL
export AGENTTEAMS_RUNTIME AGENTTEAMS_MATRIX_URL AGENTTEAMS_AI_GATEWAY_URL AGENTTEAMS_FS_BUCKET AGENTTEAMS_STORAGE_PREFIX AGENTTEAMS_EMBEDDING_MODEL
