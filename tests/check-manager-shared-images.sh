#!/usr/bin/env bash
set -euo pipefail
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
for fn in _normalize_version _ver_lt _supports_deepseek_harness resolve_image_tags manager_image_for_runtime; do
    eval "$(sed -n "/^${fn}()/,/^}/p" "$ROOT_DIR/.installer-source/public/install.sh")"
done
AGENTTEAMS_REGISTRY=registry.example
AGENTTEAMS_KNOWN_STABLE_VERSION=v1.2.7
AGENTTEAMS_DEEPSEEK_HARNESS_MIN_VERSION=v1.2.4
AGENTTEAMS_DEEPSEEK_HARNESS_WORKER_VERSION=v0.1.2
AGENTTEAMS_CODEX_WORKER_VERSION=v0.1.0
unset AGENTTEAMS_INSTALL_MANAGER_IMAGE AGENTTEAMS_INSTALL_MANAGER_QWENPAW_IMAGE
unset AGENTTEAMS_INSTALL_WORKER_IMAGE AGENTTEAMS_INSTALL_QWENPAW_WORKER_IMAGE
for AGENTTEAMS_VERSION in v1.2.7 v1.2.7-rc.1 latest local-test sha-test; do
    resolve_image_tags
    [[ "$(manager_image_for_runtime openclaw)" == "$WORKER_IMAGE" ]]
    [[ "$(manager_image_for_runtime qwenpaw)" == "$QWENPAW_WORKER_IMAGE" ]]
    [[ "$MANAGER_IMAGE" == */agentteams-harness-openclaw:* ]]
    [[ "$MANAGER_QWENPAW_IMAGE" == */agentteams-harness-qwenpaw:* ]]
done
AGENTTEAMS_VERSION=v1.2.6
resolve_image_tags
[[ "$MANAGER_IMAGE" == registry.example/agentteams/agentteams-manager:v1.2.6 ]]
[[ "$MANAGER_QWENPAW_IMAGE" == registry.example/agentteams/agentteams-manager-qwenpaw:v1.2.6 ]]
AGENTTEAMS_VERSION=v1.2.7
AGENTTEAMS_INSTALL_WORKER_IMAGE=local/openclaw:custom
AGENTTEAMS_INSTALL_QWENPAW_WORKER_IMAGE=local/qwenpaw:custom
resolve_image_tags
[[ "$MANAGER_IMAGE" == local/openclaw:custom ]]
[[ "$MANAGER_QWENPAW_IMAGE" == local/qwenpaw:custom ]]
AGENTTEAMS_INSTALL_MANAGER_IMAGE=private/openclaw:manager
AGENTTEAMS_INSTALL_MANAGER_QWENPAW_IMAGE=private/qwenpaw:manager
resolve_image_tags
[[ "$MANAGER_IMAGE" == private/openclaw:manager ]]
[[ "$MANAGER_QWENPAW_IMAGE" == private/qwenpaw:manager ]]
echo 'PASS: Managers use shared Harness images; historical releases and overrides are preserved'
