#!/usr/bin/env bash
# QwenPaw runtime.yaml update integration test.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/qwenpaw-image-e2e.sh"

qwenpaw_e2e_require_enabled
qwenpaw_e2e_require_docker
qwenpaw_e2e_init "update-runtime"
qwenpaw_e2e_build_or_use_image

runtime_yaml="${QWENPAW_E2E_TMP_DIR}/seed/agents/${QWENPAW_E2E_WORKER_NAME}/runtime/runtime.yaml"
mkdir -p "$(dirname "${runtime_yaml}")" "${QWENPAW_E2E_TMP_DIR}/seed/agents/${QWENPAW_E2E_WORKER_NAME}"
cat >"${runtime_yaml}" <<EOF
metadata:
  generation: "1"
team:
  name: qwenpaw-update-e2e
member:
  name: ${QWENPAW_E2E_WORKER_NAME}
  runtimeName: ${QWENPAW_E2E_WORKER_NAME}
  runtime: qwenpaw
  role: worker
desired:
  model:
    providerId: hiclaw-update-e2e
    providerName: HiClaw Update E2E
    model: qwen-fake-v1
    baseUrl: https://dashscope.aliyuncs.com/compatible-mode/v1
    apiKeyEnv: AGENTTEAMS_WORKER_GATEWAY_KEY
  mcpServers:
    docs:
      url: https://gateway.example.com/mcp/docs-v1
      transport: http
  channelPolicy:
    groupAllowExtra:
      - "!team-v1:matrix.local"
    dmAllowExtra:
      - "@admin-v1:matrix.local"
    groupDenyExtra:
      - "!blocked-v1:matrix.local"
    dmDenyExtra:
      - "@blocked-v1:matrix.local"
credentials:
  gatewayKeyEnv: AGENTTEAMS_WORKER_GATEWAY_KEY
EOF
cat >"${QWENPAW_E2E_TMP_DIR}/seed/agents/${QWENPAW_E2E_WORKER_NAME}/SOUL.md" <<EOF
# QwenPaw Update E2E

Apply runtime.yaml desired state.
EOF

qwenpaw_e2e_create_network
qwenpaw_e2e_start_minio
qwenpaw_e2e_wait_for_minio
qwenpaw_e2e_seed_storage
qwenpaw_e2e_start_worker -e AGENTTEAMS_WORKER_GATEWAY_KEY=fake-gateway-key

qwenpaw_e2e_wait_worker_http /api/version 240
qwenpaw_e2e_wait_worker_http /api/teamharness/health 240

qwenpaw_e2e_exec /opt/venv/qwenpaw/bin/python - <<'PY'
import json
import os
from pathlib import Path

from qwenpaw.app.channels.access_control import get_access_control_store
from qwenpaw.config.config import load_agent_config
from qwenpaw.providers.provider_manager import ProviderManager

workspace = Path(os.environ["QWENPAW_WORKING_DIR"]) / "workspaces" / "default"

manager = ProviderManager.get_instance()
assert manager.active_model.provider_id == "hiclaw-update-e2e", manager.active_model
assert manager.active_model.model == "qwen-fake-v1", manager.active_model
provider = manager.custom_providers["hiclaw-update-e2e"]
assert provider.base_url == "https://dashscope.aliyuncs.com/compatible-mode/v1", provider.base_url
assert provider.api_key == "fake-gateway-key"

mcp = json.loads((workspace / "config" / "mcporter.json").read_text(encoding="utf-8"))
assert mcp["mcpServers"]["docs"]["url"] == "https://gateway.example.com/mcp/docs-v1"
assert mcp["mcpServers"]["docs"]["headers"]["Authorization"] == "Bearer fake-gateway-key"
assert not (workspace / "mcporter-servers.json").exists()

agent_config = load_agent_config("default")
assert agent_config.channels.matrix.access_control_group is True
assert agent_config.channels.matrix.access_control_dm is True

acl = get_access_control_store(workspace).get_acl("matrix")
assert "!team-v1:matrix.local" in acl["whitelist"], acl
assert "@admin-v1:matrix.local" in acl["whitelist"], acl
assert "!blocked-v1:matrix.local" in acl["blacklist"], acl
assert "@blocked-v1:matrix.local" in acl["blacklist"], acl
PY

started_before="$(docker inspect -f '{{.State.StartedAt}}' "${QWENPAW_E2E_WORKER_CONTAINER}")"

qwenpaw_e2e_exec /opt/venv/qwenpaw/bin/python - <<'PY'
import json
from pathlib import Path

root = Path("/tmp/qwenpaw-agent-package-v2")
(root / "config").mkdir(parents=True, exist_ok=True)
(root / "skills" / "hot-skill").mkdir(parents=True, exist_ok=True)
(root / "config" / "AGENTS.md").write_text("# Package Generation 2\n", encoding="utf-8")
(root / "config" / "SOUL.md").write_text("# Runtime Updated Soul\n", encoding="utf-8")
(root / "mcp.json").write_text(
    json.dumps(
        {
            "mcpServers": {
                "package-docs": {
                    "url": "https://package.example.com/mcp/docs",
                    "transport": "http",
                    "description": "Package docs",
                }
            }
        }
    )
    + "\n",
    encoding="utf-8",
)
(root / "skills" / "hot-skill" / "SKILL.md").write_text("# Hot Skill\n", encoding="utf-8")
PY

cat >"${runtime_yaml}" <<EOF
metadata:
  generation: "2"
team:
  name: qwenpaw-update-e2e
member:
  name: ${QWENPAW_E2E_WORKER_NAME}
  runtimeName: ${QWENPAW_E2E_WORKER_NAME}
  runtime: qwenpaw
  role: worker
desired:
  agentPackage:
    ref: file:///tmp/qwenpaw-agent-package-v2
    name: update-e2e-package
    version: 2.0.0
    digest: sha256:update-e2e-v2
  model:
    providerId: hiclaw-update-e2e
    providerName: HiClaw Update E2E
    model: qwen-fake-v2
    baseUrl: https://dashscope.aliyuncs.com/compatible-mode/v1
    apiKeyEnv: AGENTTEAMS_WORKER_GATEWAY_KEY
  mcpServers:
    docs:
      url: https://gateway.example.com/mcp/docs-v2
      transport: http
  channelPolicy:
    groupAllowExtra:
      - "!team-v2:matrix.local"
    dmAllowExtra:
      - "@admin-v2:matrix.local"
    groupDenyExtra:
      - "!blocked-v2:matrix.local"
    dmDenyExtra:
      - "@blocked-v2:matrix.local"
credentials:
  gatewayKeyEnv: AGENTTEAMS_WORKER_GATEWAY_KEY
EOF

qwenpaw_e2e_put_runtime_yaml

updated="false"
for _ in $(seq 1 45); do
    if qwenpaw_e2e_exec /opt/venv/qwenpaw/bin/python - <<'PY'
import json
import os
from pathlib import Path

from qwenpaw.app.channels.access_control import get_access_control_store
from qwenpaw.config.config import load_agent_config
from qwenpaw.providers.provider_manager import ProviderManager

workspace = Path(os.environ["QWENPAW_WORKING_DIR"]) / "workspaces" / "default"
package_root = Path(os.environ["QWENPAW_WORKING_DIR"]) / "agent-packages"

manager = ProviderManager.get_instance()
assert manager.active_model.provider_id == "hiclaw-update-e2e", manager.active_model
assert manager.active_model.model == "qwen-fake-v2", manager.active_model

mcp = json.loads((workspace / "config" / "mcporter.json").read_text(encoding="utf-8"))
assert mcp["mcpServers"]["docs"]["url"] == "https://gateway.example.com/mcp/docs-v2"
assert not (workspace / "mcporter-servers.json").exists()

agent_config = load_agent_config("default")
assert agent_config.mcp.clients["package-docs"].url == "https://package.example.com/mcp/docs"
assert not (workspace / "mcp.json").exists()

acl = get_access_control_store(workspace).get_acl("matrix")
assert "!team-v2:matrix.local" in acl["whitelist"], acl
assert "@admin-v2:matrix.local" in acl["whitelist"], acl
assert "!blocked-v2:matrix.local" in acl["blacklist"], acl
assert "@blocked-v2:matrix.local" in acl["blacklist"], acl

assert "Package Generation 2" in (workspace / "AGENTS.md").read_text(encoding="utf-8")
assert (workspace / "skills" / "hot-skill" / "SKILL.md").is_file()
identity = (package_root / "current.identity").read_text(encoding="utf-8").splitlines()
assert identity[:4] == [
    "file:///tmp/qwenpaw-agent-package-v2",
    "update-e2e-package",
    "2.0.0",
    "sha256:update-e2e-v2",
], identity
PY
    then
        updated="true"
        break
    fi
    sleep 2
done

[ "${updated}" = "true" ] || qwenpaw_e2e_fail "runtime.yaml update was not applied"

started_after="$(docker inspect -f '{{.State.StartedAt}}' "${QWENPAW_E2E_WORKER_CONTAINER}")"
[ "${started_before}" = "${started_after}" ] || qwenpaw_e2e_fail "worker container restarted during runtime update"

qwenpaw_e2e_log "PASS: runtime.yaml model, MCP, channel policy, and AgentSpec package hot update verified"
