#!/usr/bin/env bash
set -euo pipefail
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
for fn in _normalize_version _ver_lt _supports_deepseek_harness resolve_image_tags; do
    eval "$(sed -n "/^${fn}()/,/^}/p" "$ROOT_DIR/install/agentteams-install.sh")"
done
AGENTTEAMS_REGISTRY=registry.example
AGENTTEAMS_KNOWN_STABLE_VERSION=v1.2.6
AGENTTEAMS_DEEPSEEK_HARNESS_MIN_VERSION=v1.2.4
_INSTALL_ENV_KEYS=' '
AGENTTEAMS_DEEPSEEK_HARNESS_WORKER_VERSION=v1.2.7
AGENTTEAMS_CODEX_WORKER_VERSION=v1.2.7
AGENTTEAMS_VERSION=v1.2.7
resolve_image_tags
for image in "$WORKER_IMAGE" "$QWENPAW_WORKER_IMAGE" "$HERMES_WORKER_IMAGE" "$OPENHUMAN_WORKER_IMAGE" "$DEEPSEEK_HARNESS_WORKER_IMAGE" "$CODEX_WORKER_IMAGE"; do
    [[ "$image" == registry.example/agentteams/agentteams-harness-*:v1.2.7 ]]
done
AGENTTEAMS_VERSION=v1.2.6
resolve_image_tags
[[ "$DEEPSEEK_HARNESS_WORKER_IMAGE" == registry.example/agentteams/agentteams-deepseek-harness-worker:v0.1.1 ]]
[[ "$CODEX_WORKER_IMAGE" == registry.example/agentteams/agentteams-harness-codex:v0.1.0 ]]
_INSTALL_ENV_KEYS=' AGENTTEAMS_DEEPSEEK_HARNESS_WORKER_VERSION AGENTTEAMS_CODEX_WORKER_VERSION '
AGENTTEAMS_DEEPSEEK_HARNESS_WORKER_VERSION=v0.1.2
AGENTTEAMS_CODEX_WORKER_VERSION=custom
AGENTTEAMS_VERSION=v1.2.7
resolve_image_tags
[[ "$DEEPSEEK_HARNESS_WORKER_IMAGE" == *:v0.1.2 ]]
[[ "$CODEX_WORKER_IMAGE" == *:custom ]]
echo 'PASS: six v1.2.7 tags, historical fallback and explicit runtime overrides'
