import asyncio
import hashlib
import importlib.util
import json
import logging
import os
from pathlib import Path
import shutil
import subprocess
import sys
import textwrap
import types
import zipfile

import pytest

from qwenpaw_worker.config import WorkerConfig
from qwenpaw_worker.update import MemberRuntimeConfig
from qwenpaw_worker.worker import BUILTIN_QWENPAW_PLUGIN_MARKER, Worker


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


def _runtime_yaml(
    path: Path,
    generation: int = 1,
    version: str = "1.0.0",
    shared_prefix: str = "shared",
) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        f"""
apiVersion: agentteams.io/v1beta1
kind: MemberRuntimeConfig
metadata:
  generation: {generation}
team:
  name: demo-team
  teamRoomId: "!team:matrix.local"
  leaderName: leader
  admin:
    name: admin
    matrixUserId: "@admin:matrix.local"
member:
  name: worker-a
  runtimeName: worker-a
  role: worker
  runtime: qwenpaw
  matrixUserId: "@worker-a:matrix.local"
desired:
  agentPackage:
    ref: file:///tmp/dev-worker.tar.gz
    name: dev-worker
    version: {version}
    digest: sha256:{version}
  outputSanitize:
    keywords: [internal-token]
storage:
  memberPrefix: agents/worker-a
  sharedPrefix: {shared_prefix}
credentials:
  matrixTokenEnv: AGENTTEAMS_WORKER_MATRIX_TOKEN
""",
        encoding="utf-8",
    )


def _config(tmp_path: Path) -> WorkerConfig:
    return WorkerConfig(
        worker_name="worker-a",
        worker_cr_name="worker-a-cr",
        fs_endpoint="http://minio:9000",
        fs_access_key="key",
        fs_secret_key="secret",
        install_dir=tmp_path / "agents",
        runtime_config_poll_interval=0.01,
    )


def _write_plugin_zip(tmp_path: Path, plugin_id: str, package_name: str, zip_name: str) -> Path:
    package_root = tmp_path / f"package-{plugin_id}" / package_name
    package_root.mkdir(parents=True)
    (package_root / "plugin.json").write_text(json.dumps({"id": plugin_id}) + "\n", encoding="utf-8")
    (package_root / "plugin.py").write_text("plugin = object()\n", encoding="utf-8")
    zip_path = tmp_path / zip_name
    with zipfile.ZipFile(zip_path, "w") as archive:
        archive.write(package_root / "plugin.json", f"{package_name}/plugin.json")
        archive.write(package_root / "plugin.py", f"{package_name}/plugin.py")
    return zip_path


def _builtin_plugin_digest(plugin_dir: Path) -> str:
    digest = hashlib.sha256()
    for path in sorted(plugin_dir.rglob("*")):
        if not path.is_file() or path.name == BUILTIN_QWENPAW_PLUGIN_MARKER:
            continue
        rel = path.relative_to(plugin_dir).as_posix()
        digest.update(rel.encode("utf-8"))
        digest.update(b"\0")
        digest.update(path.read_bytes())
        digest.update(b"\0")
    return digest.hexdigest()


def _write_builtin_plugin(root: Path, plugin_id: str, apply_name: str) -> Path:
    plugin_dir = root / plugin_id
    plugin_dir.mkdir(parents=True)
    (plugin_dir / "plugin.json").write_text(
        json.dumps({"id": plugin_id, "name": plugin_id.title(), "version": "1.0.0"}) + "\n",
        encoding="utf-8",
    )
    (plugin_dir / "plugin.py").write_text(
        f"def {apply_name}():\n    return {{'ok': True, 'plugin': {plugin_id!r}}}\n",
        encoding="utf-8",
    )
    asset_dir = plugin_dir / "assets"
    asset_dir.mkdir()
    (asset_dir / "builtin.txt").write_text(f"{plugin_id} asset\n", encoding="utf-8")
    (plugin_dir / BUILTIN_QWENPAW_PLUGIN_MARKER).write_text(
        _builtin_plugin_digest(plugin_dir) + "\n",
        encoding="utf-8",
    )
    return plugin_dir


def test_link_workspace_shared_points_to_canonical_shared(tmp_path: Path) -> None:
    config = _config(tmp_path)
    worker = Worker(config)

    worker._link_workspace_shared()

    workspace_shared = config.default_workspace_dir / "shared"
    assert workspace_shared.is_symlink()
    assert workspace_shared.resolve() == config.shared_dir.resolve()

    task_note = workspace_shared / "tasks" / "task-1" / "workspace" / "note.txt"
    task_note.parent.mkdir(parents=True)
    task_note.write_text("ready\n", encoding="utf-8")

    assert (config.shared_dir / "tasks" / "task-1" / "workspace" / "note.txt").read_text(encoding="utf-8") == "ready\n"


def test_configure_qwenpaw_runtime_uses_workspace_teams_prompt(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    root = types.SimpleNamespace(
        agents=types.SimpleNamespace(
            active_agent="",
            profiles={},
            agent_order=[],
        ),
        security=types.SimpleNamespace(
            file_guard=types.SimpleNamespace(enabled=False, sensitive_files=[]),
            tool_guard=types.SimpleNamespace(enabled=False, guarded_tools=None, auto_denied_rules=[]),
        ),
    )
    saved_agents = {}
    agent_config = types.SimpleNamespace(
        name="",
        workspace_dir="",
        system_prompt_files=["AGENTS.md"],
        running=types.SimpleNamespace(shell_command_executable="custom-shell"),
    )

    class AgentProfileRef:
        def __init__(self, id, workspace_dir, enabled):
            self.id = id
            self.workspace_dir = workspace_dir
            self.enabled = enabled

    class AgentProfileConfig:
        def __init__(self, id, name, description, workspace_dir):
            self.id = id
            self.name = name
            self.description = description
            self.workspace_dir = workspace_dir
            self.system_prompt_files = []
            self.running = types.SimpleNamespace(shell_command_executable="custom-shell")

    config_module = types.ModuleType("qwenpaw.config.config")
    config_module.AgentProfileConfig = AgentProfileConfig
    config_module.AgentProfileRef = AgentProfileRef
    config_module.load_agent_config = lambda _agent_id: agent_config
    config_module.save_agent_config = lambda agent_id, config: saved_agents.update({agent_id: config})

    utils_module = types.ModuleType("qwenpaw.config.utils")
    utils_module.load_config = lambda: root
    utils_module.save_config = lambda _root: None

    monkeypatch.setitem(sys.modules, "qwenpaw", types.ModuleType("qwenpaw"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config", types.ModuleType("qwenpaw.config"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config.config", config_module)
    monkeypatch.setitem(sys.modules, "qwenpaw.config.utils", utils_module)

    config = _config(tmp_path)
    Worker(config)._configure_qwenpaw_runtime()

    saved = saved_agents["default"]
    workspace = tmp_path / "agents" / "worker-a" / ".qwenpaw" / "workspaces" / "default"
    assert saved.workspace_dir == str(workspace)
    assert saved.approval_level == "AUTO"
    assert saved.system_prompt_files == ["AGENTS.md", "SOUL.md", "TEAMS.md"]
    assert not any("shared" in prompt for prompt in saved.system_prompt_files)
    assert saved.running.shell_command_executable == "custom-shell"

    qa_profile = root.agents.profiles["QwenPaw_QA_Agent_0.2"]
    assert qa_profile.enabled is False
    assert qa_profile.workspace_dir == str(workspace.parent / "QwenPaw_QA_Agent_0.2")


def test_configure_qwenpaw_runtime_disables_existing_builtin_qa_agent(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    class AgentProfileRef:
        def __init__(self, id, workspace_dir, enabled):
            self.id = id
            self.workspace_dir = workspace_dir
            self.enabled = enabled

    qa_workspace = (
        tmp_path / "agents" / "worker-a" / ".qwenpaw" / "workspaces" / "QwenPaw_QA_Agent_0.2"
    )
    root = types.SimpleNamespace(
        agents=types.SimpleNamespace(
            active_agent="QwenPaw_QA_Agent_0.2",
            profiles={
                "QwenPaw_QA_Agent_0.2": AgentProfileRef(
                    id="QwenPaw_QA_Agent_0.2",
                    workspace_dir=str(qa_workspace),
                    enabled=True,
                ),
            },
            agent_order=["QwenPaw_QA_Agent_0.2"],
        ),
        security=types.SimpleNamespace(
            file_guard=types.SimpleNamespace(enabled=False, sensitive_files=[]),
            tool_guard=types.SimpleNamespace(enabled=False, guarded_tools=None, auto_denied_rules=[]),
        ),
    )
    saved_agents = {}
    agent_config = types.SimpleNamespace(
        name="",
        workspace_dir="",
        system_prompt_files=[],
    )

    class AgentProfileConfig:
        def __init__(self, id, name, description, workspace_dir):
            self.id = id
            self.name = name
            self.description = description
            self.workspace_dir = workspace_dir
            self.system_prompt_files = []

    config_module = types.ModuleType("qwenpaw.config.config")
    config_module.AgentProfileConfig = AgentProfileConfig
    config_module.AgentProfileRef = AgentProfileRef
    config_module.load_agent_config = lambda _agent_id: agent_config
    config_module.save_agent_config = lambda agent_id, config: saved_agents.update({agent_id: config})

    utils_module = types.ModuleType("qwenpaw.config.utils")
    utils_module.load_config = lambda: root
    utils_module.save_config = lambda _root: None

    monkeypatch.setitem(sys.modules, "qwenpaw", types.ModuleType("qwenpaw"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config", types.ModuleType("qwenpaw.config"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config.config", config_module)
    monkeypatch.setitem(sys.modules, "qwenpaw.config.utils", utils_module)

    Worker(_config(tmp_path))._configure_qwenpaw_runtime()

    assert root.agents.active_agent == "default"
    assert root.agents.profiles["QwenPaw_QA_Agent_0.2"].enabled is False
    assert root.agents.profiles["QwenPaw_QA_Agent_0.2"].workspace_dir == str(qa_workspace)


def test_configure_qwenpaw_runtime_protects_session_directory(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    existing_sensitive = "/var/run/agentteams/credential.json"
    existing_auto_deny_rule = "CUSTOM_RULE"
    file_guard = types.SimpleNamespace(enabled=False, sensitive_files=[existing_sensitive])
    tool_guard = types.SimpleNamespace(
        enabled=False,
        guarded_tools=["execute_shell_command"],
        auto_denied_rules=[existing_auto_deny_rule],
    )
    root = types.SimpleNamespace(
        agents=types.SimpleNamespace(
            active_agent="",
            profiles={},
            agent_order=[],
        ),
        security=types.SimpleNamespace(
            file_guard=file_guard,
            tool_guard=tool_guard,
        ),
    )
    saved_roots = []
    saved_agents = {}
    agent_config = types.SimpleNamespace(
        name="",
        workspace_dir="",
        system_prompt_files=[],
    )

    class AgentProfileRef:
        def __init__(self, id, workspace_dir, enabled):
            self.id = id
            self.workspace_dir = workspace_dir
            self.enabled = enabled

    class AgentProfileConfig:
        def __init__(self, id, name, description, workspace_dir):
            self.id = id
            self.name = name
            self.description = description
            self.workspace_dir = workspace_dir
            self.system_prompt_files = []

    config_module = types.ModuleType("qwenpaw.config.config")
    config_module.AgentProfileConfig = AgentProfileConfig
    config_module.AgentProfileRef = AgentProfileRef
    config_module.load_agent_config = lambda _agent_id: agent_config
    config_module.save_agent_config = lambda agent_id, config: saved_agents.update({agent_id: config})

    utils_module = types.ModuleType("qwenpaw.config.utils")
    utils_module.load_config = lambda: root
    utils_module.save_config = lambda saved_root: saved_roots.append(saved_root)

    monkeypatch.setitem(sys.modules, "qwenpaw", types.ModuleType("qwenpaw"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config", types.ModuleType("qwenpaw.config"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config.config", config_module)
    monkeypatch.setitem(sys.modules, "qwenpaw.config.utils", utils_module)

    config = _config(tmp_path)
    Worker(config)._configure_qwenpaw_runtime()
    Worker(config)._configure_qwenpaw_runtime()

    sessions_dir = config.default_workspace_dir / "sessions"
    assert root.security.file_guard.enabled is True
    assert root.security.tool_guard.enabled is True
    assert root.security.file_guard.sensitive_files.count(existing_sensitive) == 1
    assert root.security.file_guard.sensitive_files.count(f"{sessions_dir}/") == 1
    assert root.security.tool_guard.guarded_tools == []
    assert root.security.tool_guard.auto_denied_rules.count(existing_auto_deny_rule) == 1
    assert root.security.tool_guard.auto_denied_rules.count("SENSITIVE_FILE_BLOCK") == 1
    assert len(saved_roots) == 2


def test_session_file_prompt_policy_is_appended_idempotently(tmp_path: Path) -> None:
    config = _config(tmp_path)
    config.default_workspace_dir.mkdir(parents=True)
    agents = config.default_workspace_dir / "AGENTS.md"
    soul = config.default_workspace_dir / "SOUL.md"
    agents.write_text("Existing agents prompt.\n", encoding="utf-8")
    soul.write_text("Existing soul prompt.\n", encoding="utf-8")

    worker = Worker(config)
    worker._ensure_session_file_prompt_policy()
    worker._ensure_session_file_prompt_policy()

    for prompt_file in (agents, soul):
        content = prompt_file.read_text(encoding="utf-8")
        assert "Do not read, list, grep, glob, summarize, copy, or expose files under sessions/." in content
        assert "This rule applies to all channels, users, and sessions, not only DingTalk." in content
        assert content.count("Session files are runtime-private state") == 1


def test_file_guard_blocks_explicit_session_file_paths(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("QWENPAW_WORKING_DIR", str(tmp_path / ".qwenpaw"))
    file_guardian = pytest.importorskip("qwenpaw.security.tool_guard.guardians.file_guardian")
    models = pytest.importorskip("qwenpaw.security.tool_guard.models")

    sessions_dir = tmp_path / "workspace" / "sessions"
    session_files = [
        sessions_dir / "dingtalk" / "user-a_session-a.json",
        sessions_dir / "console" / "user-b_session-b.json",
        sessions_dir / "legacy-session.json",
    ]
    for session_file in session_files:
        session_file.parent.mkdir(parents=True, exist_ok=True)
        session_file.write_text('{"marker":"fake-session-marker"}\n', encoding="utf-8")

    guard = file_guardian.FilePathToolGuardian(sensitive_files=[f"{sessions_dir}/"])

    for session_file in session_files:
        findings = guard.guard("read_file", {"file_path": str(session_file)})
        assert findings
        assert findings[0].category == models.GuardThreatCategory.SENSITIVE_FILE_ACCESS

    shell_findings = guard.guard("execute_shell_command", {"command": f"cat {session_files[0]}"})
    assert shell_findings
    assert shell_findings[0].category == models.GuardThreatCategory.SENSITIVE_FILE_ACCESS


def test_worker_configured_active_tool_guard_auto_denies_session_files(tmp_path: Path) -> None:
    if importlib.util.find_spec("qwenpaw") is None:
        pytest.skip("qwenpaw package unavailable")
    pytest.importorskip("qwenpaw.agents.tool_guard_mixin")

    script = textwrap.dedent(
        """
        import asyncio
        from pathlib import Path
        import sys

        from qwenpaw_worker.config import WorkerConfig
        from qwenpaw_worker.worker import DEFAULT_AGENT_ID, Worker

        root_dir = Path(sys.argv[1])
        config = WorkerConfig(
            worker_name="worker-a",
            worker_cr_name="worker-a-cr",
            fs_endpoint="http://minio:9000",
            fs_access_key="key",
            fs_secret_key="secret",
            install_dir=root_dir / "agents",
            runtime_config_poll_interval=0.01,
        )
        Worker(config)._configure_qwenpaw_runtime()

        from qwenpaw.agents.tool_guard_mixin import ToolGuardMixin
        from qwenpaw.config.config import load_agent_config
        from qwenpaw.config.utils import load_config
        from qwenpaw.security.tool_guard.engine import ToolGuardEngine

        root = load_config()
        agent_config = load_agent_config(DEFAULT_AGENT_ID)
        sessions_dir = config.default_workspace_dir / "sessions"
        session_file = sessions_dir / "console" / "user-a_session-a.json"
        session_file.parent.mkdir(parents=True, exist_ok=True)
        session_file.write_text('{"marker":"fake-session-marker"}\\n', encoding="utf-8")

        class GuardProbe(ToolGuardMixin):
            pass

        probe = GuardProbe()
        probe._agent_config = agent_config
        probe._request_context = {"session_id": "fake-request-session"}
        probe._tool_guard_engine = ToolGuardEngine(enabled=True)

        async def decide(tool_call):
            return await probe._decide_guard_action(tool_call)

        shell_action = asyncio.run(
            decide(
                {
                    "name": "execute_shell_command",
                    "input": {"command": "rm -rf /tmp/qwenpaw-worker-fake-target"},
                },
            ),
        )
        if shell_action is not None:
            raise AssertionError(f"expected no approval action for ordinary shell tools; got {shell_action.kind!r}")

        action = asyncio.run(decide({"name": "read_file", "input": {"file_path": str(session_file)}}))
        if action is None:
            raise AssertionError(
                "expected active tool guard action for sessions file; "
                f"approval_level={agent_config.approval_level!r} "
                f"guarded_tools={root.security.tool_guard.guarded_tools!r} "
                f"auto_denied_rules={root.security.tool_guard.auto_denied_rules!r}"
            )
        assert action.kind == "auto_denied"
        assert any(finding.rule_id == "SENSITIVE_FILE_BLOCK" for finding in action.guard_result.findings)
        assert root.security.file_guard.sensitive_files.count(f"{sessions_dir}/") == 1
        assert root.security.tool_guard.guarded_tools == []
        assert root.security.tool_guard.auto_denied_rules.count("SENSITIVE_FILE_BLOCK") == 1
        """
    )

    env = os.environ.copy()
    qwenpaw_src = str(Path(__file__).resolve().parents[1] / "src")
    env["PYTHONPATH"] = os.pathsep.join(part for part in (qwenpaw_src, env.get("PYTHONPATH", "")) if part)
    env["QWENPAW_WORKING_DIR"] = str(tmp_path / ".qwenpaw")
    env["QWENPAW_SECRET_DIR"] = str(tmp_path / ".qwenpaw.secret")
    env.pop("COPAW_WORKING_DIR", None)
    env.pop("COPAW_SECRET_DIR", None)

    result = subprocess.run(
        [sys.executable, "-c", script, str(tmp_path)],
        check=False,
        capture_output=True,
        text=True,
        env=env,
    )

    assert result.returncode == 0, f"stdout:\n{result.stdout}\nstderr:\n{result.stderr}"


def test_prepare_env_exposes_agent_workspace(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    config = _config(tmp_path)
    monkeypatch.delenv("AGENT_WORKSPACE", raising=False)

    Worker(config)._prepare_env()

    assert os.environ["AGENT_WORKSPACE"] == str(config.default_workspace_dir)


def test_runtime_updater_uses_default_workspace_for_package_materialization(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    workspace = tmp_path / "agent-workspace"
    monkeypatch.setenv("AGENT_WORKSPACE", str(workspace))

    config = _config(tmp_path)
    worker = Worker(config)

    assert worker.updater.package_manager.workspace_dir == config.default_workspace_dir


def test_install_teamharness_plugin_unpacks_zip_before_qwenpaw_install(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    zip_path = _write_plugin_zip(tmp_path, "teamharness", "teamharness-qwenpaw-0.1.0", "teamharness-qwenpaw.zip")
    calls: list[list[str]] = []

    def fake_run(command, check=True, **_kwargs):
        assert check is True
        plugin_dir = Path(command[3])
        assert plugin_dir.is_dir()
        assert (plugin_dir / "plugin.json").is_file()
        calls.append(command)

    monkeypatch.setenv("AGENTTEAMS_TEAMHARNESS_QWENPAW_PLUGIN_PACKAGE", str(zip_path))
    monkeypatch.setattr("qwenpaw_worker.plugin_bootstrap.shutil.which", lambda _name: "/usr/bin/qwenpaw")
    monkeypatch.setattr("qwenpaw_worker.plugin_install.subprocess.run", fake_run)

    Worker(_config(tmp_path))._install_teamharness_plugin()

    assert len(calls) == 1
    assert calls[0][:3] == ["/usr/bin/qwenpaw", "plugin", "install"]
    assert calls[0][4] == "--force"


def test_plugin_install_logs_package_summary_without_sensitive_values(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    caplog: pytest.LogCaptureFixture,
) -> None:
    zip_path = _write_plugin_zip(tmp_path, "teamharness", "teamharness-qwenpaw-0.1.0", "teamharness-qwenpaw.zip")
    config = _config(tmp_path)
    config.fs_secret_key = "credential-secret-value"

    def fake_run(command, check=True, **_kwargs):
        assert check is True

    monkeypatch.setenv("AGENTTEAMS_AUTH_TOKEN", "worker-auth-token")
    monkeypatch.setattr("qwenpaw_worker.plugin_bootstrap.shutil.which", lambda _name: "/usr/bin/qwenpaw")
    monkeypatch.setattr("qwenpaw_worker.plugin_install.subprocess.run", fake_run)
    caplog.set_level(logging.INFO, logger="qwenpaw_worker.worker")

    Worker(config)._install_qwenpaw_plugin_package("teamharness", zip_path, "teamharness-qwenpaw-plugin-")

    assert "component=plugin plugin=teamharness step=install event=begin" in caplog.text
    assert "component=plugin plugin=teamharness step=install event=complete" in caplog.text
    assert "package_type=zip" in caplog.text
    assert "duration_ms=" in caplog.text
    assert "credential-secret-value" not in caplog.text
    assert "worker-auth-token" not in caplog.text


def test_install_default_plugins_installs_teamharness_and_workerflow(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    teamharness_zip = _write_plugin_zip(
        tmp_path,
        "teamharness",
        "teamharness-qwenpaw-0.1.0",
        "teamharness-qwenpaw.zip",
    )
    workerflow_zip = _write_plugin_zip(
        tmp_path,
        "workerflow",
        "workerflow-qwenpaw-0.1.0",
        "workerflow-qwenpaw.zip",
    )
    installed: list[str] = []

    def fake_run(command, check=True, **_kwargs):
        assert check is True
        plugin_dir = Path(command[3])
        manifest = json.loads((plugin_dir / "plugin.json").read_text(encoding="utf-8"))
        installed.append(manifest["id"])

    monkeypatch.setenv("AGENTTEAMS_TEAMHARNESS_QWENPAW_PLUGIN_PACKAGE", str(teamharness_zip))
    monkeypatch.setenv("AGENTTEAMS_WORKERFLOW_QWENPAW_PLUGIN_PACKAGE", str(workerflow_zip))
    monkeypatch.setattr("qwenpaw_worker.plugin_bootstrap.shutil.which", lambda _name: "/usr/bin/qwenpaw")
    monkeypatch.setattr("qwenpaw_worker.plugin_install.subprocess.run", fake_run)

    Worker(_config(tmp_path))._install_default_plugins()

    assert installed == ["teamharness", "workerflow"]


@pytest.mark.anyio
async def test_start_uses_image_builtin_plugins_without_runtime_install(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    _runtime_yaml(config.runtime_config_path)
    builtin_plugins = tmp_path / "image-builtin" / "plugins"
    _write_builtin_plugin(builtin_plugins, "teamharness", "apply_teamharness")
    _write_builtin_plugin(builtin_plugins, "workerflow", "apply_workerflow")
    teamharness_zip = _write_plugin_zip(
        tmp_path,
        "teamharness",
        "teamharness-qwenpaw-0.1.0",
        "teamharness-qwenpaw.zip",
    )
    workerflow_zip = _write_plugin_zip(
        tmp_path,
        "workerflow",
        "workerflow-qwenpaw-0.1.0",
        "workerflow-qwenpaw.zip",
    )
    install_commands: list[list[str]] = []

    def fake_run(command, check=True, **_kwargs):
        assert check is True
        install_commands.append(command)

    monkeypatch.setenv("AGENTTEAMS_BUILTIN_QWENPAW_PLUGINS_DIR", str(builtin_plugins))
    monkeypatch.setenv("AGENTTEAMS_TEAMHARNESS_QWENPAW_PLUGIN_PACKAGE", str(teamharness_zip))
    monkeypatch.setenv("AGENTTEAMS_WORKERFLOW_QWENPAW_PLUGIN_PACKAGE", str(workerflow_zip))
    monkeypatch.setattr("qwenpaw_worker.plugin_bootstrap.shutil.which", lambda _name: "/usr/bin/qwenpaw")
    monkeypatch.setattr("qwenpaw_worker.plugin_install.subprocess.run", fake_run)
    monkeypatch.setattr("qwenpaw_worker.sync.FileSync.mirror_all", lambda _self: None)
    monkeypatch.setattr("qwenpaw_worker.sync.FileSync.pull_runtime_config", lambda _self, _path: True)
    monkeypatch.setattr("qwenpaw_worker.worker.Worker._configure_qwenpaw_runtime", lambda _self: None)
    monkeypatch.setattr("qwenpaw_worker.worker.Worker._apply_teamharness_assets", lambda _self: {"ok": True})
    monkeypatch.setattr("qwenpaw_worker.worker.Worker._apply_workerflow_assets", lambda _self: {"ok": True})

    def fake_apply_once(self, runtime_config=None, force=False, reapply_adapter=True):
        self.current_config = runtime_config or self.load()

    monkeypatch.setattr("qwenpaw_worker.update.RuntimeUpdater.apply_once", fake_apply_once)

    async def fake_push_loop(sync, check_interval=5, heartbeat=None):
        await asyncio.Event().wait()

    async def fake_update_loop(self):
        await asyncio.Event().wait()

    monkeypatch.setattr("qwenpaw_worker.worker.push_loop", fake_push_loop)
    monkeypatch.setattr("qwenpaw_worker.update.RuntimeUpdater.loop", fake_update_loop)

    worker = Worker(config)

    assert await worker.start() is True
    await asyncio.sleep(0)
    await worker.stop()

    assert install_commands == []
    assert (config.qwenpaw_working_dir / "plugins" / "teamharness" / "plugin.py").is_file()
    assert (config.qwenpaw_working_dir / "plugins" / "workerflow" / "plugin.py").is_file()


def test_install_teamharness_plugin_rejects_zip_path_traversal(tmp_path: Path) -> None:
    zip_path = tmp_path / "teamharness-qwenpaw.zip"
    with zipfile.ZipFile(zip_path, "w") as archive:
        archive.writestr("../target-evil/plugin.json", "{}\n")

    try:
        Worker(_config(tmp_path))._extract_qwenpaw_plugin_zip(zip_path, tmp_path / "target")
    except RuntimeError as exc:
        assert "unsafe qwenpaw plugin package path" in str(exc)
    else:
        raise AssertionError("expected unsafe plugin package path failure")

    assert not (tmp_path / "target-evil").exists()


def test_runtime_adapter_reapplies_builtin_assets_without_runtime_install(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    builtin_plugins = tmp_path / "image-builtin" / "plugins"
    _write_builtin_plugin(builtin_plugins, "teamharness", "apply_teamharness")
    _write_builtin_plugin(builtin_plugins, "workerflow", "apply_workerflow")
    install_commands: list[list[str]] = []
    applied: list[str] = []

    def fake_run(command, check=True, **_kwargs):
        assert check is True
        install_commands.append(command)

    monkeypatch.setenv("AGENTTEAMS_BUILTIN_QWENPAW_PLUGINS_DIR", str(builtin_plugins))
    monkeypatch.setattr("qwenpaw_worker.plugin_install.subprocess.run", fake_run)
    monkeypatch.setattr("qwenpaw_worker.worker.Worker._apply_teamharness_assets", lambda _self: applied.append("teamharness"))
    monkeypatch.setattr("qwenpaw_worker.worker.Worker._apply_workerflow_assets", lambda _self: applied.append("workerflow"))

    Worker(config)._apply_runtime_adapter()

    assert install_commands == []
    assert applied == ["teamharness", "workerflow"]
    assert (config.qwenpaw_working_dir / "plugins" / "teamharness" / "plugin.py").is_file()
    assert (config.qwenpaw_working_dir / "plugins" / "workerflow" / "plugin.py").is_file()


def test_prepare_builtin_plugin_repairs_partial_target_with_matching_marker(tmp_path: Path) -> None:
    config = _config(tmp_path)
    worker = Worker(config)
    source_dir = _write_builtin_plugin(tmp_path / "image-builtin" / "plugins", "teamharness", "apply_teamharness")
    target_dir = config.qwenpaw_working_dir / "plugins" / "teamharness"
    shutil.copytree(source_dir, target_dir)

    (target_dir / "assets" / "builtin.txt").unlink()

    assert worker._builtin_plugin_current(source_dir, target_dir) is False

    worker._prepare_builtin_plugin("teamharness", source_dir)

    assert (target_dir / "assets" / "builtin.txt").read_text(encoding="utf-8") == "teamharness asset\n"


def test_apply_teamharness_assets_runs_installed_adapter(tmp_path: Path) -> None:
    config = _config(tmp_path)
    plugin_dir = config.qwenpaw_working_dir / "plugins" / "teamharness"
    plugin_dir.mkdir(parents=True)
    (plugin_dir / "plugin.py").write_text(
        """
from pathlib import Path


def apply_teamharness():
    Path(__file__).with_name("applied.txt").write_text("ok\\n", encoding="utf-8")
    return {"ok": True, "source": __file__}
""".lstrip(),
        encoding="utf-8",
    )

    result = Worker(config)._apply_teamharness_assets()

    assert result["ok"] is True
    assert (plugin_dir / "applied.txt").read_text(encoding="utf-8") == "ok\n"


def test_plugin_load_and_apply_logs_safe_summary_without_result_payload(
    tmp_path: Path,
    caplog: pytest.LogCaptureFixture,
) -> None:
    config = _config(tmp_path)
    plugin_dir = config.qwenpaw_working_dir / "plugins" / "teamharness"
    plugin_dir.mkdir(parents=True)
    (plugin_dir / "plugin.py").write_text(
        """
def apply_teamharness():
    return {
        "ok": True,
        "source": __file__,
        "secret": "plugin-secret-token",
        "prompt": "private prompt body",
    }
""".lstrip(),
        encoding="utf-8",
    )
    caplog.set_level(logging.INFO, logger="qwenpaw_worker.worker")

    result = Worker(config)._apply_teamharness_assets()

    assert result["ok"] is True
    assert "component=plugin plugin=teamharness step=load event=begin" in caplog.text
    assert "component=plugin plugin=teamharness step=load event=complete" in caplog.text
    assert "entrypoint=apply_teamharness" in caplog.text
    assert "component=plugin plugin=teamharness step=apply event=complete" in caplog.text
    assert "ok=True" in caplog.text
    assert "result_key_count=4" in caplog.text
    assert "plugin-secret-token" not in caplog.text
    assert "private prompt body" not in caplog.text
    assert "prompt" not in caplog.text


def test_team_context_renderer_only_calls_render_function(tmp_path: Path) -> None:
    config = _config(tmp_path)
    plugin_dir = config.qwenpaw_working_dir / "plugins" / "teamharness"
    plugin_dir.mkdir(parents=True)
    (plugin_dir / "plugin.py").write_text(
        """
from pathlib import Path


def render_team_context(config):
    Path(__file__).with_name("rendered.txt").write_text(config["team"]["name"], encoding="utf-8")
    return "# Rendered TeamHarness Contract\\n" + config["team"]["name"] + "\\n"


def apply_teamharness():
    Path(__file__).with_name("applied.txt").write_text("bad\\n", encoding="utf-8")
    return {"ok": True}
""".lstrip(),
        encoding="utf-8",
    )
    runtime_config = MemberRuntimeConfig(
        path=config.runtime_config_path,
        raw={
            "metadata": {"generation": "1"},
            "team": {"name": "demo-team"},
            "member": {"runtime": "qwenpaw"},
        },
    )

    text = Worker(config)._render_teamharness_context(runtime_config)

    assert text == "# Rendered TeamHarness Contract\ndemo-team\n"
    assert (plugin_dir / "rendered.txt").read_text(encoding="utf-8") == "demo-team"
    assert not (plugin_dir / "applied.txt").exists()


def test_apply_workerflow_assets_runs_installed_adapter(tmp_path: Path) -> None:
    config = _config(tmp_path)
    plugin_dir = config.qwenpaw_working_dir / "plugins" / "workerflow"
    plugin_dir.mkdir(parents=True)
    (plugin_dir / "plugin.py").write_text(
        """
from pathlib import Path


def apply_workerflow():
    Path(__file__).with_name("applied.txt").write_text("ok\\n", encoding="utf-8")
    return {"ok": True, "source": __file__}
""".lstrip(),
        encoding="utf-8",
    )

    result = Worker(config)._apply_workerflow_assets()

    assert result["ok"] is True
    assert (plugin_dir / "applied.txt").read_text(encoding="utf-8") == "ok\n"


@pytest.mark.anyio
async def test_start_reads_runtime_config_installs_adapter_and_starts_loops(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    _runtime_yaml(config.runtime_config_path)
    calls: list[str] = []

    monkeypatch.setattr("qwenpaw_worker.sync.FileSync.mirror_all", lambda _self: calls.append("mirror"))
    monkeypatch.setattr("qwenpaw_worker.sync.FileSync.pull_runtime_config", lambda _self, _path: calls.append("pull"))
    monkeypatch.setattr("qwenpaw_worker.worker.Worker._configure_qwenpaw_runtime", lambda self: calls.append("runtime"))
    monkeypatch.setattr("qwenpaw_worker.worker.Worker._prepare_default_plugins", lambda self: calls.append("plugins"))
    monkeypatch.setattr("qwenpaw_worker.worker.Worker._apply_teamharness_assets", lambda self: calls.append("teamharness-sync"))
    monkeypatch.setattr("qwenpaw_worker.worker.Worker._apply_workerflow_assets", lambda self: calls.append("workerflow-sync"))

    def fake_apply_once(self, runtime_config=None, force=False, reapply_adapter=True):
        runtime_config = runtime_config or self.load()
        self.current_config = runtime_config
        calls.append(f"update:{runtime_config.generation}:{force}:{reapply_adapter}")

    monkeypatch.setattr("qwenpaw_worker.update.RuntimeUpdater.apply_once", fake_apply_once)

    async def fake_push_loop(sync, check_interval=5, heartbeat=None):
        calls.append(f"push:{check_interval}")
        await asyncio.Event().wait()

    async def fake_update_loop(self):
        calls.append("update-loop")
        await asyncio.Event().wait()

    monkeypatch.setattr("qwenpaw_worker.worker.push_loop", fake_push_loop)
    monkeypatch.setattr("qwenpaw_worker.update.RuntimeUpdater.loop", fake_update_loop)

    worker = Worker(config)

    assert await worker.start() is True
    await asyncio.sleep(0)

    assert calls[:7] == [
        "mirror",
        "pull",
        "runtime",
        "plugins",
        "update:1:True:False",
        "teamharness-sync",
        "workerflow-sync",
    ]
    assert "push:5" in calls
    assert "update-loop" in calls
    assert worker.updater.current_config is not None
    assert worker.updater.current_config.member_name == "worker-a"

    await worker.stop()


@pytest.mark.anyio
async def test_start_logs_worker_stage_durations_without_sensitive_values(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    caplog: pytest.LogCaptureFixture,
) -> None:
    config = _config(tmp_path)
    config.fs_secret_key = "credential-secret-value"
    config.fs_endpoint = "https://user:storage-secret-value@storage.example"
    _runtime_yaml(config.runtime_config_path)
    monkeypatch.setenv("AGENTTEAMS_AUTH_TOKEN", "worker-auth-token")
    calls: list[str] = []

    monkeypatch.setattr("qwenpaw_worker.sync.FileSync.mirror_all", lambda _self: calls.append("mirror"))
    monkeypatch.setattr("qwenpaw_worker.sync.FileSync.pull_runtime_config", lambda _self, _path: calls.append("pull"))
    monkeypatch.setattr("qwenpaw_worker.worker.Worker._configure_qwenpaw_runtime", lambda self: calls.append("runtime"))
    monkeypatch.setattr("qwenpaw_worker.worker.Worker._prepare_default_plugins", lambda self: calls.append("plugins"))
    monkeypatch.setattr("qwenpaw_worker.worker.Worker._apply_teamharness_assets", lambda self: calls.append("teamharness-sync"))
    monkeypatch.setattr("qwenpaw_worker.worker.Worker._apply_workerflow_assets", lambda self: calls.append("workerflow-sync"))

    def fake_apply_once(self, runtime_config=None, force=False, reapply_adapter=True):
        runtime_config = runtime_config or self.load()
        self.current_config = runtime_config
        calls.append(f"update:{runtime_config.generation}:{force}:{reapply_adapter}")

    monkeypatch.setattr("qwenpaw_worker.update.RuntimeUpdater.apply_once", fake_apply_once)

    async def fake_push_loop(sync, check_interval=5, heartbeat=None):
        calls.append(f"push:{check_interval}")
        await asyncio.Event().wait()

    async def fake_update_loop(self):
        calls.append("update-loop")
        await asyncio.Event().wait()

    monkeypatch.setattr("qwenpaw_worker.worker.push_loop", fake_push_loop)
    monkeypatch.setattr("qwenpaw_worker.update.RuntimeUpdater.loop", fake_update_loop)

    caplog.set_level(logging.INFO, logger="qwenpaw_worker.worker")
    worker = Worker(config)

    assert await worker.start() is True
    await asyncio.sleep(0)
    await worker.stop()

    assert "component=worker stage=mirror_all" in caplog.text
    assert "component=worker stage=load_runtime_config" in caplog.text
    assert "duration_ms=" in caplog.text
    assert "generation=1" in caplog.text
    assert "team=demo-team" in caplog.text
    assert "member=worker-a" in caplog.text
    assert "credential-secret-value" not in caplog.text
    assert "storage-secret-value" not in caplog.text
    assert "https://<redacted>@storage.example" in caplog.text
    assert "worker-auth-token" not in caplog.text
    assert "internal-token" not in caplog.text
    assert "prompt" not in caplog.text
    assert "session content" not in caplog.text


@pytest.mark.anyio
async def test_start_links_workspace_shared_to_runtime_team_shared_prefix(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    _runtime_yaml(config.runtime_config_path, shared_prefix="teams/demo-team/shared")
    calls: list[str] = []

    monkeypatch.delenv("AGENTTEAMS_SHARED_STORAGE_PREFIX", raising=False)
    monkeypatch.setattr("qwenpaw_worker.sync.FileSync.mirror_all", lambda _self: calls.append("mirror"))
    monkeypatch.setattr("qwenpaw_worker.sync.FileSync.pull_runtime_config", lambda _self, _path: calls.append("pull"))

    def fake_mirror_prefix(_self, remote_prefix, local_dir):
        local_dir.mkdir(parents=True, exist_ok=True)
        calls.append(f"mirror-prefix:{remote_prefix}:{local_dir}")

    monkeypatch.setattr("qwenpaw_worker.sync.FileSync.mirror_prefix", fake_mirror_prefix)
    monkeypatch.setattr("qwenpaw_worker.worker.Worker._configure_qwenpaw_runtime", lambda self: calls.append("runtime"))
    monkeypatch.setattr("qwenpaw_worker.worker.Worker._prepare_default_plugins", lambda self: calls.append("plugins"))
    monkeypatch.setattr("qwenpaw_worker.worker.Worker._apply_teamharness_assets", lambda self: calls.append("teamharness-sync"))
    monkeypatch.setattr("qwenpaw_worker.worker.Worker._apply_workerflow_assets", lambda self: calls.append("workerflow-sync"))

    def fake_apply_once(self, runtime_config=None, force=False, reapply_adapter=True):
        runtime_config = runtime_config or self.load()
        self.current_config = runtime_config
        calls.append(f"update:{runtime_config.generation}:{force}:{reapply_adapter}")

    monkeypatch.setattr("qwenpaw_worker.update.RuntimeUpdater.apply_once", fake_apply_once)

    async def fake_push_loop(sync, check_interval=5, heartbeat=None):
        calls.append(f"push:{check_interval}")
        await asyncio.Event().wait()

    async def fake_update_loop(self):
        calls.append("update-loop")
        await asyncio.Event().wait()

    monkeypatch.setattr("qwenpaw_worker.worker.push_loop", fake_push_loop)
    monkeypatch.setattr("qwenpaw_worker.update.RuntimeUpdater.loop", fake_update_loop)

    worker = Worker(config)

    assert await worker.start() is True
    await asyncio.sleep(0)

    team_shared = tmp_path / "teams" / "demo-team" / "shared"
    workspace_shared = config.default_workspace_dir / "shared"
    assert workspace_shared.is_symlink()
    assert workspace_shared.resolve() == team_shared.resolve()
    assert config.runtime_config_path == tmp_path / "agents" / "worker-a" / "runtime" / "runtime.yaml"
    assert os.environ["TEAMHARNESS_SHARED_DIR"] == str(team_shared)
    assert os.environ["AGENTTEAMS_SHARED_STORAGE_PREFIX"] == "teams/demo-team/shared"
    assert f"mirror-prefix:teams/demo-team/shared:{team_shared}" in calls

    await worker.stop()


@pytest.mark.anyio
async def test_start_fails_when_runtime_config_missing(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    monkeypatch.setattr("qwenpaw_worker.sync.FileSync.mirror_all", lambda _self: None)
    monkeypatch.setattr("qwenpaw_worker.sync.FileSync.pull_runtime_config", lambda _self, _path: False)

    worker = Worker(config)

    assert await worker.start() is False

    data = json.loads((config.qwenpaw_working_dir / "heartbeat.json").read_text(encoding="utf-8"))
    assert data["status"] == "not_ready"
    assert "runtime config missing" in data["message"]


@pytest.mark.anyio
async def test_builtin_plugin_prepare_failure_marks_worker_unready(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    _runtime_yaml(config.runtime_config_path)

    monkeypatch.setattr("qwenpaw_worker.sync.FileSync.mirror_all", lambda _self: None)
    monkeypatch.setattr("qwenpaw_worker.sync.FileSync.pull_runtime_config", lambda _self, _path: True)
    monkeypatch.setattr("qwenpaw_worker.worker.Worker._configure_qwenpaw_runtime", lambda self: None)

    def fail_prepare(_self):
        raise RuntimeError("plugin prepare failed")

    monkeypatch.setattr("qwenpaw_worker.worker.Worker._prepare_default_plugins", fail_prepare)

    worker = Worker(config)

    assert await worker.start() is False

    data = json.loads((config.qwenpaw_working_dir / "heartbeat.json").read_text(encoding="utf-8"))
    assert data["status"] == "not_ready"
    assert data["message"] == "plugin prepare failed"


@pytest.mark.anyio
async def test_qwenpaw_config_failure_marks_qwenpaw_config_unready(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    _runtime_yaml(config.runtime_config_path)

    monkeypatch.setattr("qwenpaw_worker.sync.FileSync.mirror_all", lambda _self: None)
    monkeypatch.setattr("qwenpaw_worker.sync.FileSync.pull_runtime_config", lambda _self, _path: True)

    def fail_config(_self):
        raise RuntimeError("qwenpaw config failed")

    monkeypatch.setattr("qwenpaw_worker.worker.Worker._configure_qwenpaw_runtime", fail_config)

    worker = Worker(config)

    assert await worker.start() is False

    data = json.loads((config.qwenpaw_working_dir / "heartbeat.json").read_text(encoding="utf-8"))
    assert data["status"] == "not_ready"
    assert data["message"] == "qwenpaw config failed"


@pytest.mark.anyio
async def test_hot_update_failure_does_not_relabel_qwenpaw_config_unready(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    _runtime_yaml(config.runtime_config_path)

    monkeypatch.setattr("qwenpaw_worker.sync.FileSync.mirror_all", lambda _self: None)
    monkeypatch.setattr("qwenpaw_worker.sync.FileSync.pull_runtime_config", lambda _self, _path: True)
    monkeypatch.setattr("qwenpaw_worker.worker.Worker._configure_qwenpaw_runtime", lambda _self: None)
    monkeypatch.setattr("qwenpaw_worker.worker.Worker._prepare_default_plugins", lambda _self: None)

    def fail_update(_self, runtime_config=None, force=False, reapply_adapter=True):
        raise RuntimeError("agent package failed")

    monkeypatch.setattr("qwenpaw_worker.update.RuntimeUpdater.apply_once", fail_update)

    worker = Worker(config)

    assert await worker.start() is False

    data = json.loads((config.qwenpaw_working_dir / "heartbeat.json").read_text(encoding="utf-8"))
    assert data["status"] == "not_ready"
    assert data["message"] == "agent package failed"


def test_hot_update_apply_does_not_replace_qwenpaw_process(tmp_path: Path) -> None:
    config = _config(tmp_path)
    worker = Worker(config)
    process = object()
    package_calls: list[str] = []
    adapter_calls: list[str] = []

    class FakePackageManager:
        def apply(self, runtime_config):
            package_calls.append(runtime_config.generation)
            return tmp_path / "packages" / runtime_config.generation

    worker._process = process
    worker.updater.package_manager = FakePackageManager()
    worker.updater.adapter_apply = lambda: adapter_calls.append("adapter")
    worker.updater.current_config = MemberRuntimeConfig(
        path=config.runtime_config_path,
        raw={"metadata": {"generation": "1"}, "member": {"runtime": "qwenpaw"}},
    )

    worker.updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={"metadata": {"generation": "2"}, "member": {"runtime": "qwenpaw"}},
        )
    )

    assert package_calls == ["2"]
    assert adapter_calls == ["adapter"]
    assert worker._process is process


@pytest.mark.anyio
async def test_qwenpaw_process_exit_marks_runtime_not_ready(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    caplog: pytest.LogCaptureFixture,
) -> None:
    config = _config(tmp_path)
    worker = Worker(config)
    caplog.set_level(logging.INFO, logger="qwenpaw_worker.worker")

    class FakeProcess:
        pid = 12345
        returncode = 7

        async def wait(self):
            return self.returncode

    async def fake_create_subprocess_exec(*_args, **_kwargs):
        return FakeProcess()

    async def idle_heartbeat_probe_loop(self):
        await asyncio.Event().wait()

    monkeypatch.setattr("qwenpaw_worker.worker.asyncio.create_subprocess_exec", fake_create_subprocess_exec)
    monkeypatch.setattr("qwenpaw_worker.worker.Worker._heartbeat_probe_loop", idle_heartbeat_probe_loop)

    await worker._run_qwenpaw()
    await worker.stop()

    data = json.loads((config.qwenpaw_working_dir / "heartbeat.json").read_text(encoding="utf-8"))
    assert data["status"] == "not_ready"
    assert data["details"]["returncode"] == 7
    assert "component=worker stage=start_qwenpaw_app event=begin" in caplog.text
    assert "component=worker stage=start_qwenpaw_app event=complete" in caplog.text
    assert "component=worker stage=start_qwenpaw_app event=exited" in caplog.text
    assert "pid=12345" in caplog.text
    assert "returncode=7" in caplog.text


@pytest.mark.anyio
async def test_stop_logs_failed_background_task_and_still_terminates_process(
    tmp_path: Path,
    caplog: pytest.LogCaptureFixture,
) -> None:
    config = _config(tmp_path)
    worker = Worker(config)
    terminated: list[str] = []

    async def failed_task():
        raise RuntimeError("background exploded")

    task = asyncio.create_task(failed_task())
    await asyncio.sleep(0)
    worker._update_task = task

    class FakeProcess:
        returncode = None

        def terminate(self):
            terminated.append("terminate")
            self.returncode = 0

        async def wait(self):
            return self.returncode

    worker._process = FakeProcess()

    await worker.stop()

    assert terminated == ["terminate"]
    assert worker._update_task is None
    assert "background task _update_task failed during stop" in caplog.text


@pytest.mark.anyio
async def test_qwenpaw_process_start_failure_marks_runtime_not_ready(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    caplog: pytest.LogCaptureFixture,
) -> None:
    config = _config(tmp_path)
    worker = Worker(config)
    caplog.set_level(logging.INFO, logger="qwenpaw_worker.worker")

    async def fail_create_subprocess_exec(*_args, **_kwargs):
        raise FileNotFoundError("qwenpaw not found secret-token-value")

    monkeypatch.setattr("qwenpaw_worker.worker.asyncio.create_subprocess_exec", fail_create_subprocess_exec)

    with pytest.raises(FileNotFoundError):
        await worker._run_qwenpaw()

    data = json.loads((config.qwenpaw_working_dir / "heartbeat.json").read_text(encoding="utf-8"))
    assert data["status"] == "not_ready"
    assert data["message"] == "qwenpaw app failed to start"
    assert data["details"]["error_type"] == "FileNotFoundError"
    assert "secret-token-value" not in json.dumps(data)
    assert "component=worker stage=start_qwenpaw_app event=failed" in caplog.text
    assert "error_type=FileNotFoundError" in caplog.text
    assert "secret-token-value" not in caplog.text


@pytest.mark.anyio
async def test_heartbeat_probe_loop_delegates_to_heartbeat_module(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    worker = Worker(config)
    calls: list[tuple[str, int]] = []

    async def fake_loop(heartbeat, *, worker_name, port):
        calls.append((worker_name, port))
        heartbeat.update("ready", "qwenpaw ready")
        raise asyncio.CancelledError

    monkeypatch.setattr("qwenpaw_worker.worker.run_worker_heartbeat_loop", fake_loop)
    with pytest.raises(asyncio.CancelledError):
        await worker._heartbeat_probe_loop()

    data = json.loads((config.qwenpaw_working_dir / "heartbeat.json").read_text(encoding="utf-8"))
    assert data["status"] == "ready"
    assert data["message"] == "qwenpaw ready"
    assert calls == [("worker-a-cr", config.console_port)]
