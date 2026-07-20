import importlib.util
import json
import os
from pathlib import Path
import shutil
import sys
import types

import pytest


REPO_ROOT = Path(__file__).resolve().parents[5]
PLUGIN = REPO_ROOT / "plugins" / "teamharness" / "adapters" / "qwenpaw" / "plugin.py"


@pytest.fixture(autouse=True)
def _clear_agent_workspace_env():
    original = os.environ.pop("AGENT_WORKSPACE", None)
    try:
        yield
    finally:
        if original is None:
            os.environ.pop("AGENT_WORKSPACE", None)
        else:
            os.environ["AGENT_WORKSPACE"] = original


def _load_plugin():
    spec = importlib.util.spec_from_file_location("teamharness_qwenpaw_plugin_test", PLUGIN)
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


def _runtime_yaml(path: Path) -> None:
    path.write_text(
        """
kind: MemberRuntimeConfig
metadata:
  generation: 3
team:
  name: demo-team
  teamRoomId: "!team:matrix.local"
  leaderName: leader
  leaderRuntimeName: leader-runtime
  admin:
    name: admin
    matrixUserId: "@admin:matrix.local"
  members:
    - name: leader
      runtimeName: leader-runtime
      role: team_leader
      matrixUserId: "@leader-runtime:matrix.local"
      personalRoomId: "!leader-dm:matrix.local"
    - name: worker-a
      runtimeName: worker-a
      role: worker
      matrixUserId: "@worker-a:matrix.local"
      personalRoomId: "!worker-dm:matrix.local"
    - name: alpha-qa
      runtimeName: qa-runtime
      role: worker
      matrixUserId: "@qa-runtime:matrix.local"
member:
  name: worker-a
  runtimeName: worker-a
  role: worker
  runtime: qwenpaw
  matrixUserId: "@worker-a:matrix.local"
  personalRoomId: "!worker-dm:matrix.local"
desired:
  agentPackage:
    name: dev-worker
    version: 1.2.0
  outputSanitize:
    keywords: [internal-token]
    envRefs: [EXTRA_SECRET]
credentials:
  matrixTokenEnv: AGENTTEAMS_WORKER_MATRIX_TOKEN
""",
        encoding="utf-8",
    )


def _leader_runtime_yaml(path: Path) -> None:
    path.write_text(
        """
kind: MemberRuntimeConfig
metadata:
  generation: 4
team:
  name: demo-team
  teamRoomId: "!team:matrix.local"
member:
  name: leader-a
  runtimeName: leader-a
  role: team_leader
  runtime: qwenpaw
""",
        encoding="utf-8",
    )


def test_mcp_client_env_includes_sts_refresh_inputs(monkeypatch, tmp_path: Path) -> None:
    module = _load_plugin()
    shared_dir = tmp_path / "shared"
    monkeypatch.setenv("TEAMHARNESS_SHARED_DIR", str(shared_dir))
    monkeypatch.setenv("AGENTTEAMS_CONTROLLER_URL", "http://controller.example.test")
    monkeypatch.setenv("AGENTTEAMS_AUTH_TOKEN_FILE", "/var/run/secrets/agentteams/token")
    monkeypatch.setenv("AGENTTEAMS_FS_ENDPOINT", "https://oss.example.test")
    monkeypatch.setenv("AGENTTEAMS_FS_ACCESS_KEY", "static-ak")
    monkeypatch.setenv("AGENTTEAMS_FS_SECRET_KEY", "static-sk")
    monkeypatch.setenv("MC_HOST_agentteams", "https://temporary@example.test")
    monkeypatch.setenv("QWENPAW_WORKING_DIR", str(tmp_path / ".qwenpaw"))

    env = module._mcp_client_env()

    assert env["TEAMHARNESS_SHARED_DIR"] == str(shared_dir)
    assert env["AGENTTEAMS_CONTROLLER_URL"] == "http://controller.example.test"
    assert env["AGENTTEAMS_AUTH_TOKEN_FILE"] == "/var/run/secrets/agentteams/token"
    assert env["AGENTTEAMS_FS_ENDPOINT"] == "https://oss.example.test"
    assert env["QWENPAW_WORKING_DIR"] == str(tmp_path / ".qwenpaw")
    assert "AGENTTEAMS_FS_ACCESS_KEY" not in env
    assert "AGENTTEAMS_FS_SECRET_KEY" not in env
    assert "MC_HOST_agentteams" not in env


def test_teamharness_installs_workspace_teams_md_without_overwriting_agentspec(tmp_path: Path, monkeypatch) -> None:
    module = _load_plugin()
    runtime_config = tmp_path / "runtime.yaml"
    shared_dir = tmp_path / "shared"
    workspace_dir = tmp_path / "workspace"
    agent_home = tmp_path / "agent-home"
    _runtime_yaml(runtime_config)
    workspace_dir.mkdir()
    agent_home.mkdir()
    (workspace_dir / "AGENTS.md").write_text("agentspec agents\n", encoding="utf-8")
    (workspace_dir / "SOUL.md").write_text("agentspec soul\n", encoding="utf-8")
    (agent_home / "SOUL.md").write_text("outer soul should not be copied\n", encoding="utf-8")

    monkeypatch.setenv("TEAMHARNESS_RUNTIME_CONFIG", str(runtime_config))
    monkeypatch.setenv("TEAMHARNESS_SHARED_DIR", str(shared_dir))
    monkeypatch.setenv("QWENPAW_WORKSPACE_DIR", str(workspace_dir))
    monkeypatch.setenv("AGENTTEAMS_AGENT_HOME", str(agent_home))
    monkeypatch.setenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "matrix-secret-value")

    result = module.apply_teamharness()

    teams_md = workspace_dir / "TEAMS.md"
    text = teams_md.read_text(encoding="utf-8")
    assert result["ok"] is True
    assert "demo-team" in text
    assert "worker-a" in text
    assert "leader" in text
    assert "qa-runtime" in text
    assert "!team:matrix.local" in text
    assert "dev-worker" in text
    assert module.TEAMS_INTERNAL_CONTROL_MARKER in text
    assert "Worker Role" in text
    assert "Matrix Reply Discipline" in text
    assert "Long Matrix Messages" in text
    assert "roomflow describe_room" in text
    assert "TASK：<projectId>" in text
    assert "Matrix Mentions" in text
    assert "artifact publish_file" in text
    assert "matrix-secret-value" not in text
    assert not (shared_dir / "TEAMS.md").exists()
    assert (workspace_dir / "AGENTS.md").read_text(encoding="utf-8") == "agentspec agents\n"
    assert (workspace_dir / "SOUL.md").read_text(encoding="utf-8") == "agentspec soul\n"


def test_team_leader_role_uses_leader_prompt(tmp_path: Path, monkeypatch) -> None:
    module = _load_plugin()
    runtime_config = tmp_path / "runtime.yaml"
    workspace_dir = tmp_path / "workspace"
    _leader_runtime_yaml(runtime_config)
    workspace_dir.mkdir()

    monkeypatch.setenv("TEAMHARNESS_RUNTIME_CONFIG", str(runtime_config))
    monkeypatch.setenv("QWENPAW_WORKSPACE_DIR", str(workspace_dir))

    result = module.apply_teamharness()

    teams_md = workspace_dir / "TEAMS.md"
    text = teams_md.read_text(encoding="utf-8")
    assert result["ok"] is True
    assert "Leader Role" in text
    assert "member.role: team_leader" in text


def test_teamharness_enables_role_skills_in_workspace(tmp_path: Path, monkeypatch) -> None:
    module = _load_plugin()
    runtime_config = tmp_path / "runtime.yaml"
    workspace_dir = tmp_path / "workspace"
    skill_pool_dir = tmp_path / "skill-pool"
    _runtime_yaml(runtime_config)
    workspace_dir.mkdir()

    monkeypatch.setenv("TEAMHARNESS_RUNTIME_CONFIG", str(runtime_config))
    monkeypatch.setenv("QWENPAW_WORKSPACE_DIR", str(workspace_dir))

    registry_module = types.ModuleType("qwenpaw.agents.skill_system.registry")
    registry_module.ensure_skill_pool_initialized = lambda: None
    registry_module.reconcile_pool_manifest = lambda: None

    store_module = types.ModuleType("qwenpaw.agents.skill_system.store")
    store_module.get_skill_pool_dir = lambda: skill_pool_dir

    class SkillPoolService:
        def download_to_workspace(self, skill_name, target_workspace, *, overwrite=False):
            source = skill_pool_dir / skill_name
            target = Path(target_workspace) / "skills" / skill_name
            target.parent.mkdir(parents=True, exist_ok=True)
            if target.exists():
                shutil.rmtree(target)
            shutil.copytree(source, target)
            manifest_path = Path(target_workspace) / "skill.json"
            manifest_path.write_text(
                json.dumps(
                    {
                        "schema_version": "workspace-skill-manifest.v1",
                        "skills": {name: {"enabled": True} for name in sorted(p.name for p in target.parent.iterdir())},
                    }
                ),
                encoding="utf-8",
            )
            return {"success": True, "name": skill_name}

    pool_service_module = types.ModuleType("qwenpaw.agents.skill_system.pool_service")
    pool_service_module.SkillPoolService = SkillPoolService

    monkeypatch.setitem(sys.modules, "qwenpaw", types.ModuleType("qwenpaw"))
    monkeypatch.setitem(sys.modules, "qwenpaw.agents", types.ModuleType("qwenpaw.agents"))
    monkeypatch.setitem(sys.modules, "qwenpaw.agents.skill_system", types.ModuleType("qwenpaw.agents.skill_system"))
    monkeypatch.setitem(sys.modules, "qwenpaw.agents.skill_system.registry", registry_module)
    monkeypatch.setitem(sys.modules, "qwenpaw.agents.skill_system.store", store_module)
    monkeypatch.setitem(sys.modules, "qwenpaw.agents.skill_system.pool_service", pool_service_module)

    result = module.apply_teamharness()

    assert result["ok"] is True
    assert (workspace_dir / "skills" / "teamharness-task-execution" / "SKILL.md").is_file()
    assert (workspace_dir / "skills" / "teamharness-communication" / "SKILL.md").is_file()
    assert not (workspace_dir / "skills" / "teamharness-task-delegation").exists()
    task_execution_text = (
        workspace_dir / "skills" / "teamharness-task-execution" / "SKILL.md"
    ).read_text(encoding="utf-8")
    assert "automatically publishes" in task_execution_text
    assert "publishedArtifacts" in task_execution_text
    file_sharing_text = (
        workspace_dir / "skills" / "teamharness-file-sharing" / "SKILL.md"
    ).read_text(encoding="utf-8")
    assert "artifact publish_file" in file_sharing_text
    assert "workspace-relative file path" in file_sharing_text
    manifest = json.loads((workspace_dir / "skill.json").read_text(encoding="utf-8"))
    assert manifest["skills"]["teamharness-task-execution"]["enabled"] is True
    assert "teamharness-task-delegation" not in manifest["skills"]


def test_sanitizer_redacts_keywords_and_env_values(tmp_path: Path, monkeypatch) -> None:
    module = _load_plugin()
    runtime_config = tmp_path / "runtime.yaml"
    _runtime_yaml(runtime_config)

    monkeypatch.setenv("TEAMHARNESS_RUNTIME_CONFIG", str(runtime_config))
    monkeypatch.setenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "matrix-secret-value")
    monkeypatch.setenv("EXTRA_SECRET", "extra-secret-value")

    text = module.sanitize_text(
        "tool saw internal-token plus matrix-secret-value and extra-secret-value"
    )

    assert text == "tool saw [REDACTED] plus [REDACTED] and [REDACTED]"


def test_sanitizer_restores_builtin_cloud_secret_patterns(monkeypatch) -> None:
    module = _load_plugin()
    monkeypatch.delenv("TEAMHARNESS_RUNTIME_CONFIG", raising=False)
    monkeypatch.delenv("QWENPAW_WORKSPACE_DIR", raising=False)

    raw_secret = "abcdefghijklmnopqrstuvwxyz123456"
    text = module.sanitize_text(
        f"access_key_secret={raw_secret}\n"
        "aliyun id LTAIabcdefghijklmnop\n"
        f"token={raw_secret}"
    )

    assert raw_secret not in text
    assert "access_key_secret=********" in text
    assert "LTAI****" in text
    assert "token=********" in text


def test_credential_guard_reads_workspace_credagent_and_updates_qwenpaw_security(
    tmp_path: Path,
    monkeypatch,
) -> None:
    module = _load_plugin()
    qwenpaw_dir = tmp_path / ".qwenpaw"
    workspace_dir = qwenpaw_dir / "workspaces" / "default"
    workspace_config = workspace_dir / "config"
    workspace_config.mkdir(parents=True)
    qwenpaw_dir.mkdir(exist_ok=True)
    (workspace_config / "credagent.json").write_text(
        json.dumps(
            {
                "credentials": [
                    {
                        "path": "~/hiclaw/credentials/matrix/password",
                        "programPermit": "qwenpaw",
                    },
                    {
                        "path": "/etc/hiclaw/secrets/",
                        "programPermit": ["mc", "qwenpaw"],
                        "writable": True,
                    },
                ],
                "output_sanitize": [
                    {"type": "keyword", "keywords": ["privateToken"]},
                ],
            }
        ),
        encoding="utf-8",
    )
    (qwenpaw_dir / "config.json").write_text(
        json.dumps(
            {
                "security": {
                    "tool_guard": {"enabled": False, "auto_denied_rules": ["EXISTING_RULE"]},
                    "file_guard": {
                        "enabled": False,
                        "sensitive_files": ["/old/removed-secret", "/manual/keep"],
                    },
                },
                "plugins": {
                    "teamharness": {
                        "credentialGuard": {
                            "paths": ["/old/removed-secret"],
                        },
                    },
                },
            }
        ),
        encoding="utf-8",
    )

    monkeypatch.setenv("QWENPAW_WORKING_DIR", str(qwenpaw_dir))
    monkeypatch.setenv("QWENPAW_WORKSPACE_DIR", str(workspace_dir))

    result = module.apply_credential_guard()

    assert result["ok"] is True
    assert result["credentials"] == 2
    assert result["outputSanitizeRules"] == 1
    config = json.loads((qwenpaw_dir / "config.json").read_text(encoding="utf-8"))
    security = config["security"]
    sensitive_files = security["file_guard"]["sensitive_files"]
    assert security["file_guard"]["enabled"] is True
    assert security["tool_guard"]["enabled"] is True
    assert "SENSITIVE_FILE_BLOCK" in security["tool_guard"]["auto_denied_rules"]
    assert "EXISTING_RULE" in security["tool_guard"]["auto_denied_rules"]
    assert "/manual/keep" in sensitive_files
    assert "/old/removed-secret" not in sensitive_files
    assert "/etc/hiclaw/secrets/" in sensitive_files
    assert any(path.endswith("/hiclaw/credentials/matrix/password") for path in sensitive_files)

    raw_secret = "abcdefghijklmnopqrstuvwxyz123456"
    assert raw_secret not in module.sanitize_text(f"privateToken={raw_secret}")


def test_private_tool_result_wrapper_redacts_text_blocks(tmp_path: Path, monkeypatch) -> None:
    module = _load_plugin()
    runtime_config = tmp_path / "runtime.yaml"
    _runtime_yaml(runtime_config)
    monkeypatch.setenv("TEAMHARNESS_RUNTIME_CONFIG", str(runtime_config))

    result = {
        "content": [
            {"type": "text", "text": "internal-token"},
            {"type": "image", "url": "keep"},
        ],
    }

    assert module.sanitize_tool_result(result)["redacted"] is True
    assert result["content"][0]["text"] == "[REDACTED]"


def test_teamharness_http_sync_reinstalls_runtime_hooks(monkeypatch) -> None:
    module = _load_plugin()
    calls = []

    monkeypatch.setattr(module, "apply_teamharness", lambda: {"ok": True, "applied": True})
    monkeypatch.setattr(
        module,
        "install_output_sanitizer_wrapper",
        lambda api=None: calls.append(("sanitizer", api)) or {"ok": True, "installed": True},
    )

    class APIRouter:
        def __init__(self):
            self.routes = {}

        def get(self, path):
            def decorator(func):
                self.routes[("GET", path)] = func
                return func

            return decorator

        def post(self, path):
            def decorator(func):
                self.routes[("POST", path)] = func
                return func

            return decorator

    fastapi_module = types.ModuleType("fastapi")
    fastapi_module.APIRouter = APIRouter
    monkeypatch.setitem(sys.modules, "fastapi", fastapi_module)

    class Api:
        def __init__(self):
            self.routers = {}

        def register_startup_hook(self, *_args, **_kwargs):
            pass

        def register_shutdown_hook(self, *_args, **_kwargs):
            pass

        def register_http_router(self, router, prefix, tags):
            self.routers[prefix] = router

    api = Api()
    plugin = module.TeamHarnessPlugin()
    plugin.register(api)

    result = api.routers["/teamharness"].routes[("POST", "/sync")]()

    assert result == {"ok": True, "applied": True}
    assert calls == [("sanitizer", api)]
    assert plugin.sanitizer_result == {"ok": True, "installed": True}


def test_mcp_client_receives_matrix_env_for_message_tool(tmp_path: Path, monkeypatch) -> None:
    module = _load_plugin()
    workspace_dir = tmp_path / "workspace"
    shared_dir = tmp_path / "shared"
    workspace_dir.mkdir()

    class MCPClientConfig:
        def __init__(self, **kwargs):
            self.__dict__.update(kwargs)

    class MCPConfig:
        def __init__(self):
            self.clients = {}

    class AgentConfig:
        def __init__(self):
            self.mcp = None

    agent_config = AgentConfig()
    saved = {}

    def load_agent_config(agent_id):
        saved["loaded_agent"] = agent_id
        return agent_config

    def save_agent_config(agent_id, config):
        saved["saved_agent"] = agent_id
        saved["config"] = config

    config_module = types.ModuleType("qwenpaw.config.config")
    config_module.MCPClientConfig = MCPClientConfig
    config_module.MCPConfig = MCPConfig
    config_module.load_agent_config = load_agent_config
    config_module.save_agent_config = save_agent_config

    monkeypatch.setitem(sys.modules, "qwenpaw", types.ModuleType("qwenpaw"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config", types.ModuleType("qwenpaw.config"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config.config", config_module)
    monkeypatch.setenv("TEAMHARNESS_SHARED_DIR", str(shared_dir))
    monkeypatch.setenv("TEAMHARNESS_RUNTIME_CONFIG", "/root/agentteams-fs/shared/runtime/members/worker-a/runtime.yaml")
    monkeypatch.setenv("AGENTTEAMS_MATRIX_URL", "http://matrix.local")
    monkeypatch.setenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "matrix-token")
    monkeypatch.setenv("AGENTTEAMS_WORKER_ROLE", "worker")
    monkeypatch.setenv("AGENTTEAMS_WORKER_NAME", "worker-a")
    monkeypatch.setenv("AGENTTEAMS_STORAGE_PREFIX", "agentteams/agentteams-storage")
    monkeypatch.setenv("AGENTTEAMS_FS_BUCKET", "agentteams-storage")

    result = module._ensure_mcp_client("default", workspace_dir)

    assert result == {"agent": "default", "action": "configured"}
    assert saved["loaded_agent"] == "default"
    assert saved["saved_agent"] == "default"
    client = agent_config.mcp.clients["teamharness"]
    assert client.command == sys.executable
    assert client.env["TEAMHARNESS_SHARED_DIR"] == str(shared_dir)
    assert client.env["LOONGSUITE_PYTHON_SITE_BOOTSTRAP_LOG_SUCCESS"] == "false"
    assert client.env["TEAMHARNESS_RUNTIME_CONFIG"] == "/root/agentteams-fs/shared/runtime/members/worker-a/runtime.yaml"
    assert client.env["AGENTTEAMS_MATRIX_URL"] == "http://matrix.local"
    assert client.env["AGENTTEAMS_WORKER_MATRIX_TOKEN"] == "matrix-token"
    assert client.env["AGENTTEAMS_WORKER_ROLE"] == "worker"
    assert client.env["AGENTTEAMS_WORKER_NAME"] == "worker-a"
    assert client.env["AGENTTEAMS_STORAGE_PREFIX"] == "agentteams/agentteams-storage"
    assert client.env["AGENTTEAMS_FS_BUCKET"] == "agentteams-storage"
