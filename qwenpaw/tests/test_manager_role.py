import importlib.util
import os
from pathlib import Path

from qwenpaw_worker.config import WorkerConfig
from qwenpaw_worker.heartbeat import ControllerHeartbeatReporter
from qwenpaw_worker.update import MemberRuntimeConfig
from qwenpaw_worker.worker import Worker


def test_manager_preserves_workspace_and_resource_identity(tmp_path, monkeypatch):
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "manager")
    monkeypatch.delenv("AGENTTEAMS_WORKER_ROLE", raising=False)
    monkeypatch.setenv("AGENTTEAMS_WORKER_HOME", str(tmp_path / "manager-workspace"))
    config = WorkerConfig("manager", "http://minio:9000", "default", "secret", worker_cr_name="default")
    assert config.worker_home == tmp_path / "manager-workspace"
    assert config.worker_cr_name == "default"
    assert config.storage_prefix.endswith("agents/manager")
    assert config.runtime_config_path == config.worker_home / "runtime/runtime.yaml"


def test_manager_health_report_uses_manager_resource(monkeypatch):
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "manager")
    reporter = ControllerHeartbeatReporter.from_env("default")
    assert reporter.resource_kind == "managers"


def test_manager_loads_management_prompt_and_only_management_skills(monkeypatch):
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "manager")
    path = Path(__file__).resolve().parents[2] / "plugins/teamharness/adapters/qwenpaw/plugin.py"
    spec = importlib.util.spec_from_file_location("manager_test_plugin", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    assert module._team_prompt(None) == ""
    prompt = (module.ASSET_DIR / "prompts/manager/AGENTS.md").read_text()
    assert "You are the TeamHarness Manager" in prompt
    assert "agt" in prompt
    assert module._skill_dir() == module.ASSET_DIR / "skills/manager"


def test_manager_credentials_reach_shared_matrix_tools(tmp_path, monkeypatch):
    for key in ("AGENTTEAMS_AGENT_ROLE", "AGENTTEAMS_WORKER_ROLE", "AGENTTEAMS_WORKER_MATRIX_TOKEN", "AGENTTEAMS_WORKER_GATEWAY_KEY"):
        monkeypatch.setenv(key, "")
    monkeypatch.setenv("AGENTTEAMS_MANAGER_MATRIX_TOKEN", "manager-test-token")
    monkeypatch.setenv("AGENTTEAMS_MANAGER_GATEWAY_KEY", "manager-test-key")
    path = tmp_path / "runtime.yaml"
    path.write_text("""member:
  name: default
  runtimeName: manager
  role: manager
  runtime: qwenpaw
credentials:
  matrixTokenEnv: AGENTTEAMS_MANAGER_MATRIX_TOKEN
  gatewayKeyEnv: AGENTTEAMS_MANAGER_GATEWAY_KEY
""")
    config = WorkerConfig("manager", "http://minio:9000", "default", "secret")
    worker = Worker(config)
    worker._apply_runtime_identity(MemberRuntimeConfig.load(path))
    assert os.environ["AGENTTEAMS_WORKER_MATRIX_TOKEN"] == "manager-test-token"
    assert os.environ["AGENTTEAMS_WORKER_GATEWAY_KEY"] == "manager-test-key"
    assert worker._builtin_plugin_ids() == ("teamharness",)


def test_manager_prompt_selection_tracks_new_inline_instructions(tmp_path, monkeypatch):
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "manager")
    monkeypatch.setenv("AGENTTEAMS_WORKER_ROLE", "manager")
    worker = Worker(WorkerConfig("manager", "http://minio:9000", "default", "secret"))
    assert "MANAGER.md" in worker._system_prompt_files()
    assert "AGENTS.md" not in worker._system_prompt_files()
    path = tmp_path / "runtime.yaml"
    path.write_text("member:\n  role: manager\ndesired:\n  inlineConfig:\n    agents: Follow the current admin instructions.\n")
    assert "AGENTS.md" in worker._system_prompt_files(MemberRuntimeConfig.load(path))


def test_manager_materializes_native_role_prompt_and_preserves_identity(tmp_path, monkeypatch):
    home = tmp_path / "manager-workspace"
    builtin = tmp_path / "builtin"
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "manager")
    monkeypatch.setenv("AGENTTEAMS_WORKER_ROLE", "manager")
    monkeypatch.setenv("AGENTTEAMS_WORKER_HOME", str(home))
    monkeypatch.setenv("AGENTTEAMS_BUILTIN_QWENPAW_PLUGINS_DIR", str(builtin))
    worker = Worker(WorkerConfig("manager", "http://minio:9000", "default", "secret"))
    worker.config.default_workspace_dir.mkdir(parents=True)
    (home / "SOUL.md").write_text("Existing admin identity.\n")
    for name in ("teamharness", "agentteams-matrix-channel"):
        source = builtin / name
        source.mkdir(parents=True)
        for filename, content in (("plugin.json", "{}"), ("plugin.py", ""), (".agentteams-builtin-plugin.sha256", "fixture")):
            (source / filename).write_text(content)
    role = builtin / "teamharness/teamharness/prompts/manager"
    role.mkdir(parents=True)
    (role / "AGENTS.md").write_text("You manage resources and forward projects.\n")
    (role / "HEARTBEAT.md").write_text("Observe resource health.\n")
    worker._prepare_default_plugins()
    workspace = worker.config.default_workspace_dir
    assert (workspace / "MANAGER.md").read_text() == (role / "AGENTS.md").read_text()
    assert (workspace / "SOUL.md").read_text() == "Existing admin identity.\n"
    assert not (worker.config.qwenpaw_working_dir / "plugins/workerflow").exists()
