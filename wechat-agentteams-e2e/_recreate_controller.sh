#!/usr/bin/env bash
set -e

echo "=== remove orphaned manager ==="
docker rm -f agentteams-manager 2>/dev/null && echo "manager removed" || echo "no manager to remove"

echo "=== create controller from fixed image ==="
docker run -d --name agentteams-controller \
  --network agentteams-net \
  --network-alias matrix-local.agentteams.io \
  --network-alias aigw-local.agentteams.io \
  --network-alias fs-local.agentteams.io \
  -e AGENTTEAMS_ADMIN_USER=admin \
  -e AGENTTEAMS_ADMIN_PASSWORD=AgentTeams2026 \
  -e AGENTTEAMS_MANAGER_PASSWORD=0572443aeb095227a441444d3c2db4567c336a2df22f67701ffcb7cc2dce4df7 \
  -e AGENTTEAMS_REGISTRATION_TOKEN=5644a62a272674cf994aee97e886eadb7d41b4ed286f2619c995d7c132da2f4f \
  -e AGENTTEAMS_MINIO_USER=admin \
  -e AGENTTEAMS_MINIO_PASSWORD=AgentTeams2026 \
  -e AGENTTEAMS_LLM_PROVIDER=openai-compat \
  -e AGENTTEAMS_LLM_API_KEY=sk-8ba3875e2dec4a6789a041b107b92230 \
  -e AGENTTEAMS_DEFAULT_MODEL=qwen3.5-plus \
  -e AGENTTEAMS_OPENAI_BASE_URL=https://dashscope.aliyuncs.com/compatible-mode/v1 \
  -e AGENTTEAMS_MANAGER_GATEWAY_KEY=e2ff44937fa5f7387cefd6e6e0d59a2599d4ce070bbc82a9463452b51e5496bc \
  -e AGENTTEAMS_MANAGER_RUNTIME=copaw \
  -e AGENTTEAMS_MANAGER_IMAGE=higress-registry.cn-hangzhou.cr.aliyuncs.com/agentteams/agentteams-manager-copaw:latest \
  -e AGENTTEAMS_DEFAULT_WORKER_RUNTIME=copaw \
  -e AGENTTEAMS_WORKER_IMAGE=higress-registry.cn-hangzhou.cr.aliyuncs.com/agentteams/agentteams-worker:latest \
  -e AGENTTEAMS_COPAW_WORKER_IMAGE=higress-registry.cn-hangzhou.cr.aliyuncs.com/agentteams/agentteams-copaw-worker:latest \
  -e AGENTTEAMS_QWENPAW_WORKER_IMAGE=higress-registry.cn-hangzhou.cr.aliyuncs.com/agentteams/agentteams-qwenpaw-worker:latest \
  -e AGENTTEAMS_HERMES_WORKER_IMAGE=higress-registry.cn-hangzhou.cr.aliyuncs.com/agentteams/agentteams-hermes-worker:latest \
  -e AGENTTEAMS_MATRIX_DOMAIN=matrix-local.agentteams.io:18080 \
  -e AGENTTEAMS_ELEMENT_HOMESERVER_URL=http://127.0.0.1:18080 \
  -e AGENTTEAMS_MATRIX_URL=http://127.0.0.1:6167 \
  -e AGENTTEAMS_MATRIX_E2EE=0 \
  -e AGENTTEAMS_MINIO_ENDPOINT=http://127.0.0.1:9000 \
  -e AGENTTEAMS_MINIO_BUCKET=agentteams-storage \
  -e AGENTTEAMS_STORAGE_PREFIX=agentteams/agentteams-storage \
  -e AGENTTEAMS_FS_ENDPOINT=http://127.0.0.1:9000 \
  -e AGENTTEAMS_AI_GATEWAY_URL=http://aigw-local.agentteams.io:8080 \
  -e AGENTTEAMS_CONTROLLER_URL=http://agentteams-controller:8090 \
  -e AGENTTEAMS_DOCKER_NETWORK=agentteams-net \
  -e AGENTTEAMS_WORKSPACE_DIR=/c/Users/20145/agentteams-manager \
  -e AGENTTEAMS_HOST_SHARE_DIR=/c/Users/20145 \
  -e AGENTTEAMS_MANAGER_ENABLED=true \
  -e AGENTTEAMS_PORT_MANAGER_CONSOLE=18888 \
  -e AGENTTEAMS_LANGUAGE=zh \
  -e AGENTTEAMS_MATRIX_APPSERVICE_ENABLED=false \
  -e TZ=Asia/Shanghai \
  -v //var/run/docker.sock:/var/run/docker.sock \
  --security-opt label=disable \
  -v agentteams-data:/data \
  -v /c/Users/20145/agentteams-manager:/root/agentteams-fs/agents/manager \
  -v /c/Users/20145:/host-share \
  -p 18080:8080 `\n  -p 6167:6167 `
  -p 18001:8001 \
  -p 18088:8088 \
  --restart unless-stopped \
  agentteams/agentteams-embedded:fixed

echo "CONTROLLER_CREATE_DONE"
