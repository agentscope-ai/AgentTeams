#!/usr/bin/env bash
set -euo pipefail
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INSTALLER="${ROOT_DIR}/install/agentteams-install.sh"
eval "$(sed -n '/^resolve_image_tags()/,/^}/p' "${INSTALLER}")"
_normalize_version() { printf '%s' "$1"; }
_ver_lt() { return 1; }
_supports_deepseek_harness() { return 0; }
AGENTTEAMS_VERSION=v9.9.9
AGENTTEAMS_REGISTRY=example.test
AGENTTEAMS_DEEPSEEK_HARNESS_WORKER_VERSION=v0.1.1
AGENTTEAMS_CODEX_WORKER_VERSION=v0.1.0
unset AGENTTEAMS_INSTALL_CODEX_WORKER_IMAGE AGENTTEAMS_CODEX_WORKER_IMAGE
resolve_image_tags
[[ "${CODEX_WORKER_IMAGE}" == example.test/agentteams/agentteams-harness-codex:v0.1.0 ]]
AGENTTEAMS_CODEX_WORKER_IMAGE=example.test/codex:saved
resolve_image_tags
[[ "${CODEX_WORKER_IMAGE}" == example.test/codex:saved ]]
AGENTTEAMS_INSTALL_CODEX_WORKER_IMAGE=example.test/codex:override
resolve_image_tags
[[ "${CODEX_WORKER_IMAGE}" == example.test/codex:override ]]
echo "PASS: Codex resolves without an image, preserves saved images and honors overrides"
