import asyncio
import io
import json
import logging
import os
from pathlib import Path
import sys
import tarfile
import types
import urllib.error

import pytest

from qwenpaw_worker.config import WorkerConfig
from qwenpaw_worker.update import (
    MemberRuntimeConfig,
    QwenPawModelRuntimeSync,
    RuntimeUpdater,
    TEAMS_INTERNAL_CONTROL_MARKER,
)


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


def _agent_package(tmp_path: Path, version: str, *, include_teams: bool = False) -> Path:
    source_dir = tmp_path / f"package-src-{version}"
    config_dir = source_dir / "config"
    config_dir.mkdir(parents=True)
    (source_dir / "manifest.json").write_text('{"version":"1.0"}\n', encoding="utf-8")
    (config_dir / "AGENTS.md").write_text(f"agent package {version}\n", encoding="utf-8")
    if include_teams:
        (config_dir / "TEAMS.md").write_text(f"package teams {version}\n", encoding="utf-8")
    package_path = tmp_path / f"agent-package-{version}.tar.gz"
    with tarfile.open(package_path, "w:gz") as archive:
        archive.add(source_dir, arcname=".")
    return package_path


def test_runtime_updater_applies_changed_config_and_reapplies_adapter(tmp_path: Path) -> None:
    config = _config(tmp_path)
    applied: list[str] = []
    adapter_calls: list[str] = []

    class FakePackageManager:
        def apply(self, runtime_config: MemberRuntimeConfig):
            applied.append(runtime_config.generation)
            return tmp_path / "packages" / runtime_config.generation

    updater = RuntimeUpdater(
        config=config,
        adapter_apply=lambda: adapter_calls.append("adapter"),
        package_manager=FakePackageManager(),
    )

    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={"metadata": {"generation": "1"}, "member": {"runtime": "qwenpaw"}},
        )
    )
    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={"metadata": {"generation": "1"}, "member": {"runtime": "qwenpaw"}},
        )
    )
    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={"metadata": {"generation": "2"}, "member": {"runtime": "qwenpaw"}},
        )
    )

    assert applied == ["1", "2"]
    assert adapter_calls == ["adapter", "adapter"]
    assert updater.current_config is not None
    assert updater.current_config.generation == "2"


def test_runtime_updater_load_and_apply_once_does_not_reapply_adapter(tmp_path: Path) -> None:
    config = _config(tmp_path)
    applied: list[str] = []
    adapter_calls: list[str] = []

    class FakePackageManager:
        def apply(self, runtime_config: MemberRuntimeConfig):
            applied.append(runtime_config.generation)
            return tmp_path / "packages" / runtime_config.generation

    config.runtime_config_path.parent.mkdir(parents=True, exist_ok=True)
    config.runtime_config_path.write_text(
        """
metadata:
  generation: "2"
member:
  runtime: qwenpaw
""",
        encoding="utf-8",
    )
    updater = RuntimeUpdater(
        config=config,
        adapter_apply=lambda: adapter_calls.append("adapter"),
        package_manager=FakePackageManager(),
    )
    updater.current_config = MemberRuntimeConfig(
        path=config.runtime_config_path,
        raw={"metadata": {"generation": "1"}, "member": {"runtime": "qwenpaw"}},
    )

    updater._load_and_apply_once()

    assert applied == ["2"]
    assert adapter_calls == []
    assert updater.current_config is not None
    assert updater.current_config.generation == "2"


def test_runtime_updater_logs_safe_apply_summary_without_sensitive_values(
    tmp_path: Path,
    caplog: pytest.LogCaptureFixture,
) -> None:
    config = _config(tmp_path)
    adapter_calls: list[str] = []
    caplog.set_level(logging.INFO, logger="qwenpaw_worker.update")
    updater = RuntimeUpdater(
        config=config,
        adapter_apply=lambda: adapter_calls.append("adapter"),
        package_manager=_NoopPackageManager(),
    )

    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "42"},
                "team": {"name": "demo-team"},
                "member": {"name": "worker-a", "role": "worker", "runtime": "qwenpaw"},
                "desired": {
                    "mcpServers": {
                        "alpha": {"command": "alpha", "env": {"TOKEN": "mcp-secret-token"}},
                        "beta": {"command": "beta"},
                    },
                    "channels": {
                        "matrix": {"enabled": True, "access_token": "matrix-secret-token"},
                        "dingtalk": {"enabled": True, "client_secret": "dingtalk-secret"},
                    },
                    "credentialBindings": [
                        {
                            "credentialRef": {
                                "tokenVaultName": "vault-a",
                                "apiKeyCredentialProviderName": "provider-a",
                            },
                            "toolWhitelist": ["shell"],
                        }
                    ],
                    "model": {"api_key": "model-secret-token"},
                },
            },
        )
    )

    assert adapter_calls == ["adapter"]
    assert "component=update" in caplog.text
    assert "worker=worker-a" in caplog.text
    assert "generation=42" in caplog.text
    assert "mcp_server_count=2" in caplog.text
    assert "channel_names=dingtalk,matrix" in caplog.text
    assert "credential_binding_count=1" in caplog.text
    assert "adapter_applied=True" in caplog.text
    assert "duration_ms=" in caplog.text
    assert "mcp-secret-token" not in caplog.text
    assert "matrix-secret-token" not in caplog.text
    assert "dingtalk-secret" not in caplog.text
    assert "model-secret-token" not in caplog.text
    assert "provider-a" not in caplog.text


def test_runtime_config_identity_distinguishes_empty_list_and_empty_object(tmp_path: Path) -> None:
    config = _config(tmp_path)
    first = MemberRuntimeConfig(
        path=config.runtime_config_path,
        raw={
            "metadata": {"generation": "1"},
            "member": {"runtime": "qwenpaw"},
            "desired": {"mcpServers": []},
        },
    )
    second = MemberRuntimeConfig(
        path=config.runtime_config_path,
        raw={
            "metadata": {"generation": "1"},
            "member": {"runtime": "qwenpaw"},
            "desired": {"mcpServers": {}},
        },
    )

    assert first.desired_identity != second.desired_identity


def test_runtime_updater_does_not_reapply_adapter_for_mcp_only_change(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    monkeypatch.setenv("AGENTTEAMS_WORKER_GATEWAY_KEY", "gateway-secret")
    adapter_calls: list[str] = []
    updater = RuntimeUpdater(
        config=config,
        adapter_apply=lambda: adapter_calls.append("adapter"),
        package_manager=_NoopPackageManager(),
    )

    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "1"},
                "member": {"runtime": "qwenpaw"},
                "desired": {
                    "mcpServers": [
                        {
                            "name": "docs",
                            "url": "https://gw.example.com/mcp-servers/docs/mcp",
                        }
                    ]
                },
            },
        )
    )
    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "2"},
                "member": {"runtime": "qwenpaw"},
                "desired": {
                    "mcpServers": [
                        {
                            "name": "docs",
                            "url": "https://gw.example.com/mcp-servers/docs-v2/mcp",
                        }
                    ]
                },
            },
        )
    )

    assert adapter_calls == ["adapter"]
    default_config = json.loads((config.default_workspace_dir / "config" / "mcporter.json").read_text(encoding="utf-8"))
    assert default_config["mcpServers"]["docs"]["url"] == "https://gw.example.com/mcp-servers/docs-v2/mcp"


def test_runtime_updater_reapplies_adapter_when_credentials_change_with_mcp(
    tmp_path: Path,
) -> None:
    config = _config(tmp_path)
    adapter_calls: list[str] = []
    updater = RuntimeUpdater(
        config=config,
        adapter_apply=lambda: adapter_calls.append("adapter"),
        package_manager=_NoopPackageManager(),
    )

    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "1"},
                "member": {"runtime": "qwenpaw"},
                "desired": {
                    "mcpServers": [{"name": "docs", "url": "https://one.example/mcp"}],
                    "agentIdentity": {"workloadIdentityName": "wi-worker-a"},
                    "credentialBindings": [
                        {
                            "credentialRef": {
                                "tokenVaultName": "default",
                                "apiKeyCredentialProviderName": "GITHUB_TOKEN",
                            }
                        }
                    ],
                },
            },
        )
    )
    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "2"},
                "member": {"runtime": "qwenpaw"},
                "desired": {
                    "mcpServers": [{"name": "docs", "url": "https://two.example/mcp"}],
                    "agentIdentity": {"workloadIdentityName": "wi-worker-a"},
                    "credentialBindings": [
                        {
                            "credentialRef": {
                                "tokenVaultName": "default",
                                "apiKeyCredentialProviderName": "ALIBABA_CLOUD_ACCESS_KEY_ID",
                            }
                        }
                    ],
                },
            },
        )
    )

    assert adapter_calls == ["adapter", "adapter"]


def test_runtime_updater_does_not_reapply_adapter_for_dingtalk_channel_change(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    monkeypatch.setenv("AGENTTEAMS_WORKER_GATEWAY_KEY", "gateway-secret")
    adapter_calls: list[str] = []
    updater = RuntimeUpdater(
        config=config,
        adapter_apply=lambda: adapter_calls.append("adapter"),
        package_manager=_NoopPackageManager(),
    )

    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "1"},
                "member": {"runtime": "qwenpaw"},
                "desired": {
                    "channels": {"dingtalk": {"enabled": False}},
                },
            },
        )
    )
    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "2"},
                "member": {"runtime": "qwenpaw"},
                "desired": {
                    "channels": {
                        "dingtalk": {
                            "enabled": True,
                            "client_id": "demo-client-id",
                            "client_secret": "test-client-secret",
                        }
                    },
                },
            },
        )
    )

    assert adapter_calls == ["adapter"]


def test_runtime_updater_does_not_reapply_adapter_for_mcp_and_channel_change(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    monkeypatch.setenv("AGENTTEAMS_WORKER_GATEWAY_KEY", "gateway-secret")
    adapter_calls: list[str] = []
    updater = RuntimeUpdater(
        config=config,
        adapter_apply=lambda: adapter_calls.append("adapter"),
        package_manager=_NoopPackageManager(),
    )

    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "1"},
                "member": {"runtime": "qwenpaw"},
                "desired": {
                    "mcpServers": [{"name": "docs", "url": "https://gw.example.com/mcp/docs-v1"}],
                    "channels": {"dingtalk": {"enabled": False}},
                },
            },
        )
    )
    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "2"},
                "member": {"runtime": "qwenpaw"},
                "desired": {
                    "mcpServers": [{"name": "docs", "url": "https://gw.example.com/mcp/docs-v2"}],
                    "channels": {
                        "dingtalk": {
                            "enabled": True,
                            "client_id": "demo-client-id",
                            "client_secret": "test-client-secret",
                        }
                    },
                },
            },
        )
    )

    assert adapter_calls == ["adapter"]


def test_runtime_updater_applies_member_role_to_config_and_env(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.delenv("AGENTTEAMS_WORKER_ROLE", raising=False)
    monkeypatch.delenv("AGENTTEAMS_AGENT_ROLE", raising=False)
    config = _config(tmp_path)
    updater = RuntimeUpdater(config=config, package_manager=_NoopPackageManager())

    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "1"},
                "member": {"runtime": "qwenpaw", "role": "team_leader"},
            },
        )
    )

    assert config.agent_role == "team_leader"
    assert os.environ["AGENTTEAMS_WORKER_ROLE"] == "team_leader"
    assert os.environ["AGENTTEAMS_AGENT_ROLE"] == "team_leader"


def test_runtime_updater_refreshes_teams_md_without_secrets(tmp_path: Path) -> None:
    config = _config(tmp_path)
    teams_md = config.default_workspace_dir / "TEAMS.md"
    teams_md.parent.mkdir(parents=True)
    teams_md.write_text(
        """# Static TeamHarness Prompt

<!-- BEGIN AGENTTEAMS RUNTIME TEAM CONTEXT -->
old context
<!-- END AGENTTEAMS RUNTIME TEAM CONTEXT -->
""",
        encoding="utf-8",
    )
    updater = RuntimeUpdater(config=config, package_manager=_NoopPackageManager())

    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "1"},
                "team": {
                    "name": "demo-team",
                    "teamRoomId": "!team:matrix.local",
                    "leaderName": "leader",
                    "leaderRuntimeName": "leader-runtime",
                    "admin": {"name": "admin", "matrixUserId": "@admin:matrix.local"},
                    "members": [
                        {
                            "name": "leader",
                            "runtimeName": "leader-runtime",
                            "role": "team_leader",
                            "matrixUserId": "@leader-runtime:matrix.local",
                            "personalRoomId": "!leader-dm:matrix.local",
                        },
                        {
                            "name": "worker-a",
                            "runtimeName": "worker-a",
                            "role": "worker",
                            "matrixUserId": "@worker-a:matrix.local",
                            "personalRoomId": "!worker-dm:matrix.local",
                        },
                    ],
                },
                "member": {
                    "name": "worker-a",
                    "runtimeName": "worker-a",
                    "role": "worker",
                    "runtime": "qwenpaw",
                    "matrixUserId": "@worker-a:matrix.local",
                    "personalRoomId": "!worker-dm:matrix.local",
                },
                "credentials": {
                    "matrixTokenEnv": "AGENTTEAMS_WORKER_MATRIX_TOKEN",
                    "gatewayKeyEnv": "AGENTTEAMS_WORKER_GATEWAY_KEY",
                },
                "storage": {"bucket": "secret-ish-bucket"},
                "desired": {"model": {"model": "qwen-plus"}},
            },
        )
    )

    text = teams_md.read_text(encoding="utf-8")
    assert "# Static TeamHarness Prompt" in text
    assert TEAMS_INTERNAL_CONTROL_MARKER in text
    assert "old context" not in text
    assert "## Runtime Team Context" in text
    assert "runtimeName: leader-runtime" in text
    assert "member.runtimeName: worker-a" in text
    assert not (config.qwenpaw_working_dir / "teamharness" / "team-context.json").exists()
    assert "desired" not in text
    assert "storage" not in text
    assert "matrixTokenEnv" not in text
    assert "gatewayKeyEnv" not in text
    assert "AGENTTEAMS_WORKER_MATRIX_TOKEN" not in text
    assert "secret-ish-bucket" not in text


def test_runtime_updater_rebuilds_missing_teams_md_with_renderer(tmp_path: Path) -> None:
    config = _config(tmp_path)
    rendered: list[str] = []
    adapter_calls: list[str] = []

    def render_team_context(runtime_config: MemberRuntimeConfig) -> str:
        rendered.append(runtime_config.generation)
        return """# Full TeamHarness Contract

Use Quick Task and Project Work.

<!-- BEGIN AGENTTEAMS RUNTIME TEAM CONTEXT -->
old renderer context
<!-- END AGENTTEAMS RUNTIME TEAM CONTEXT -->
"""

    updater = RuntimeUpdater(
        config=config,
        adapter_apply=lambda: adapter_calls.append("adapter"),
        package_manager=_NoopPackageManager(),
        team_context_renderer=render_team_context,
    )

    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "1"},
                "team": {"name": "demo-team", "teamRoomId": "!team:matrix.local"},
                "member": {
                    "name": "worker-a",
                    "runtimeName": "worker-a",
                    "role": "worker",
                    "runtime": "qwenpaw",
                },
            },
        ),
        reapply_adapter=False,
    )

    text = (config.default_workspace_dir / "TEAMS.md").read_text(encoding="utf-8")
    assert rendered == ["1"]
    assert adapter_calls == []
    assert "# Full TeamHarness Contract" in text
    assert "Use Quick Task and Project Work." in text
    assert "old renderer context" not in text
    assert "team.name: demo-team" in text
    assert TEAMS_INTERNAL_CONTROL_MARKER in text


def test_runtime_updater_preserves_teams_md_when_package_changes_without_adapter(tmp_path: Path) -> None:
    config = _config(tmp_path)
    package_v1 = _agent_package(tmp_path, "1", include_teams=True)
    package_v2 = _agent_package(tmp_path, "2", include_teams=False)
    adapter_calls: list[str] = []

    def render_team_context(runtime_config: MemberRuntimeConfig) -> str:
        return f"""# Full TeamHarness Contract

<!-- BEGIN AGENTTEAMS RUNTIME TEAM CONTEXT -->
rendered generation {runtime_config.generation}
<!-- END AGENTTEAMS RUNTIME TEAM CONTEXT -->
"""

    updater = RuntimeUpdater(
        config=config,
        adapter_apply=lambda: adapter_calls.append("adapter"),
        team_context_renderer=render_team_context,
    )

    def runtime_config(package_path: Path, version: str, team_name: str) -> MemberRuntimeConfig:
        return MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": version},
                "team": {"name": team_name, "teamRoomId": "!team:matrix.local"},
                "member": {
                    "name": "worker-a",
                    "runtimeName": "worker-a",
                    "role": "worker",
                    "runtime": "qwenpaw",
                },
                "desired": {
                    "agentPackage": {
                        "ref": f"file://{package_path}",
                        "name": "dev-worker",
                        "version": version,
                        "digest": f"sha256:{version}",
                    }
                },
            },
        )

    updater.apply_once(runtime_config=runtime_config(package_v1, "1", "team-one"), reapply_adapter=False)
    updater.apply_once(runtime_config=runtime_config(package_v2, "2", "team-two"), reapply_adapter=False)

    text = (config.default_workspace_dir / "TEAMS.md").read_text(encoding="utf-8")
    assert adapter_calls == []
    assert "# Full TeamHarness Contract" in text
    assert "team.name: team-two" in text
    assert "rendered generation" not in text
    assert "package teams" not in text
    assert TEAMS_INTERNAL_CONTROL_MARKER in text


def test_runtime_updater_applies_desired_model_to_qwenpaw_agent_config(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    saved = {}
    agent_config = types.SimpleNamespace(active_model=None)

    class ModelSlotConfig:
        def __init__(self, provider_id, model):
            self.provider_id = provider_id
            self.model = model

    config_module = types.ModuleType("qwenpaw.config.config")
    config_module.ModelSlotConfig = ModelSlotConfig
    config_module.load_agent_config = lambda _agent_id: agent_config
    config_module.save_agent_config = lambda agent_id, value: saved.update({agent_id: value})

    monkeypatch.setitem(sys.modules, "qwenpaw", types.ModuleType("qwenpaw"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config", types.ModuleType("qwenpaw.config"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config.config", config_module)

    updater = RuntimeUpdater(config=config, package_manager=_NoopPackageManager())
    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "1"},
                "member": {"runtime": "qwenpaw"},
                "desired": {"model": {"providerId": "agentteams-gateway", "model": "qwen-plus"}},
            },
        )
    )

    assert saved["default"].active_model.provider_id == "agentteams-gateway"
    assert saved["default"].active_model.model == "qwen-plus"


def test_runtime_updater_configures_openai_compatible_provider_from_runtime_config(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    monkeypatch.setenv("REAL_MODEL_KEY", "real-model-secret")

    saved_agent = {}
    saved_provider = {}
    saved_active = {}
    agent_config = types.SimpleNamespace(active_model=None)

    class ModelSlotConfig:
        def __init__(self, provider_id, model):
            self.provider_id = provider_id
            self.model = model

    class ModelInfo:
        def __init__(self, id, name):
            self.id = id
            self.name = name

    class ProviderInfo:
        def __init__(self, **kwargs):
            self.data = kwargs

        def model_dump(self):
            return self.data

    class ProviderManager:
        def __init__(self):
            self.custom_providers = {}
            self.active_model = None

        @classmethod
        def get_instance(cls):
            return manager

        def _provider_from_data(self, data):
            return types.SimpleNamespace(**data)

        def save_provider_config(self, provider_id, provider):
            saved_provider[provider_id] = provider

        def save_active_model(self, value):
            saved_active["active"] = value

    manager = ProviderManager()

    config_module = types.ModuleType("qwenpaw.config.config")
    config_module.ModelSlotConfig = ModelSlotConfig
    config_module.load_agent_config = lambda _agent_id: agent_config
    config_module.save_agent_config = lambda agent_id, value: saved_agent.update({agent_id: value})

    provider_module = types.ModuleType("qwenpaw.providers.provider")
    provider_module.ModelInfo = ModelInfo
    provider_module.ProviderInfo = ProviderInfo

    provider_manager_module = types.ModuleType("qwenpaw.providers.provider_manager")
    provider_manager_module.ProviderManager = ProviderManager

    monkeypatch.setitem(sys.modules, "qwenpaw", types.ModuleType("qwenpaw"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config", types.ModuleType("qwenpaw.config"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config.config", config_module)
    monkeypatch.setitem(sys.modules, "qwenpaw.providers", types.ModuleType("qwenpaw.providers"))
    monkeypatch.setitem(sys.modules, "qwenpaw.providers.provider", provider_module)
    monkeypatch.setitem(sys.modules, "qwenpaw.providers.provider_manager", provider_manager_module)

    updater = RuntimeUpdater(config=config, package_manager=_NoopPackageManager())
    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "1"},
                "member": {"runtime": "qwenpaw"},
                "desired": {
                    "model": {
                        "providerId": "agentteams-gateway",
                        "model": "qwen-plus",
                        "gatewayUrl": "https://dashscope.aliyuncs.com/compatible-mode",
                        "apiKeyEnv": "REAL_MODEL_KEY",
                    }
                },
            },
        )
    )

    provider = saved_provider["agentteams-gateway"]
    assert provider.base_url == "https://dashscope.aliyuncs.com/compatible-mode/v1"
    assert provider.api_key == "real-model-secret"
    assert provider.chat_model == "OpenAIChatModel"
    assert provider.models[0].id == "qwen-plus"
    assert manager.custom_providers["agentteams-gateway"] is provider
    assert saved_active["active"].provider_id == "agentteams-gateway"
    assert saved_active["active"].model == "qwen-plus"
    assert saved_agent["default"].active_model.provider_id == "agentteams-gateway"
    assert saved_agent["default"].active_model.model == "qwen-plus"


def test_runtime_updater_syncs_live_qwenpaw_app_only_when_model_changes(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    monkeypatch.setenv("REAL_MODEL_KEY", "real-model-secret")
    sync_calls: list[tuple[str, str]] = []

    updater = RuntimeUpdater(
        config=config,
        package_manager=_NoopPackageManager(),
        model_runtime_sync=lambda runtime_config: sync_calls.append(
            (runtime_config.generation, runtime_config.model["model"])
        ),
    )

    base_model = {
        "providerId": "agentteams-gateway",
        "model": "qwen-plus",
        "gatewayUrl": "https://dashscope.aliyuncs.com/compatible-mode",
        "apiKeyEnv": "REAL_MODEL_KEY",
    }
    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "1"},
                "member": {"runtime": "qwenpaw"},
                "desired": {"model": base_model},
            },
        )
    )
    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "2"},
                "member": {"runtime": "qwenpaw"},
                "desired": {"model": base_model},
            },
        )
    )
    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "3"},
                "member": {"runtime": "qwenpaw"},
                "desired": {"model": {**base_model, "model": "qwen-max"}},
            },
        )
    )

    assert sync_calls == [("3", "qwen-max")]


def test_runtime_updater_model_runtime_sync_failure_is_safe_and_retriable(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    caplog: pytest.LogCaptureFixture,
) -> None:
    config = _config(tmp_path)
    monkeypatch.setenv("REAL_MODEL_KEY", "real-model-secret")
    caplog.set_level(logging.WARNING, logger="qwenpaw_worker.update")

    def fail_sync(_runtime_config: MemberRuntimeConfig) -> None:
        raise RuntimeError("live sync failed with real-model-secret")

    updater = RuntimeUpdater(
        config=config,
        package_manager=_NoopPackageManager(),
        model_runtime_sync=fail_sync,
    )
    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "1"},
                "member": {"runtime": "qwenpaw"},
                "desired": {
                    "model": {
                        "providerId": "agentteams-gateway",
                        "model": "qwen-plus",
                        "gatewayUrl": "https://dashscope.aliyuncs.com/compatible-mode",
                        "apiKeyEnv": "REAL_MODEL_KEY",
                    }
                },
            },
        )
    )

    with pytest.raises(RuntimeError):
        updater.apply_once(
            runtime_config=MemberRuntimeConfig(
                path=config.runtime_config_path,
                raw={
                    "metadata": {"generation": "2"},
                    "member": {"runtime": "qwenpaw"},
                    "desired": {
                        "model": {
                            "providerId": "agentteams-gateway",
                            "model": "qwen-max",
                            "gatewayUrl": "https://dashscope.aliyuncs.com/compatible-mode",
                            "apiKeyEnv": "REAL_MODEL_KEY",
                        }
                    },
                },
            )
        )

    assert updater.current_config is not None
    assert updater.current_config.generation == "1"
    assert "component=update step=model_runtime_sync event=failed" in caplog.text
    assert "generation=2" in caplog.text
    assert "error_type=RuntimeError" in caplog.text
    assert "safe_error_summary=RuntimeError" in caplog.text
    assert "real-model-secret" not in caplog.text


def test_model_runtime_sync_calls_qwenpaw_models_api_and_verifies_agent_active(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    caplog: pytest.LogCaptureFixture,
) -> None:
    config = _config(tmp_path)
    monkeypatch.setenv("REAL_MODEL_KEY", "real-model-secret")
    caplog.set_level(logging.INFO, logger="qwenpaw_worker.update")
    calls: list[tuple[str, str, dict]] = []

    class FakeResponse:
        def __init__(self, status: int, payload):
            self.status = status
            self._body = json.dumps(payload).encode("utf-8")

        def __enter__(self):
            return self

        def __exit__(self, *_args):
            return False

        def read(self):
            return self._body

    def fake_urlopen(request, timeout):
        body = json.loads(request.data.decode("utf-8")) if request.data else {}
        calls.append((request.get_method(), request.full_url, body))
        index = len(calls)
        if index == 1:
            return FakeResponse(200, [])
        if index == 2:
            raise urllib.error.HTTPError(
                request.full_url,
                404,
                "Not Found",
                {},
                io.BytesIO(b'{"detail":"Provider not found"}'),
            )
        if index == 3:
            return FakeResponse(
                201,
                {"id": "agentteams-gateway", "models": [], "extra_models": []},
            )
        if index == 4:
            return FakeResponse(
                200,
                {"id": "agentteams-gateway", "models": [], "extra_models": []},
            )
        if index == 5:
            return FakeResponse(
                201,
                {
                    "id": "agentteams-gateway",
                    "models": [],
                    "extra_models": [{"id": "qwen-max", "name": "qwen-max"}],
                },
            )
        if index == 6:
            return FakeResponse(
                200,
                {"active_llm": {"provider_id": "agentteams-gateway", "model": "qwen-max"}},
            )
        return FakeResponse(
            200,
            {"active_llm": {"provider_id": "agentteams-gateway", "model": "qwen-max"}},
        )

    monkeypatch.setattr("urllib.request.urlopen", fake_urlopen)

    QwenPawModelRuntimeSync(port=8088).sync(
        MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "2"},
                "member": {"runtime": "qwenpaw"},
                "desired": {
                    "model": {
                        "providerId": "agentteams-gateway",
                        "providerName": "AgentTeams Gateway",
                        "model": "qwen-max",
                        "gatewayUrl": "https://dashscope.aliyuncs.com/compatible-mode",
                        "apiKeyEnv": "REAL_MODEL_KEY",
                    }
                },
            },
        )
    )

    assert [(method, url.split("/api/models", 1)[1].split("?", 1)[0]) for method, url, _ in calls] == [
        ("GET", ""),
        ("PUT", "/agentteams-gateway/config"),
        ("POST", "/custom-providers"),
        ("PUT", "/agentteams-gateway/config"),
        ("POST", "/agentteams-gateway/models"),
        ("PUT", "/active"),
        ("GET", "/active"),
    ]
    assert calls[3][2] == {
        "api_key": "real-model-secret",
        "base_url": "https://dashscope.aliyuncs.com/compatible-mode/v1",
        "chat_model": "OpenAIChatModel",
    }
    assert calls[5][2] == {
        "scope": "agent",
        "agent_id": "default",
        "provider_id": "agentteams-gateway",
        "model": "qwen-max",
    }
    assert "component=update step=model_runtime_sync event=complete" in caplog.text
    assert "generation=2" in caplog.text
    assert "provider_id=agentteams-gateway" in caplog.text
    assert "model=qwen-max" in caplog.text
    assert "changed=True" in caplog.text
    assert "real-model-secret" not in caplog.text


def test_model_runtime_sync_http_error_does_not_expose_response_body_secret(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    monkeypatch.setenv("REAL_MODEL_KEY", "real-model-secret")

    def fake_urlopen(request, timeout):
        raise urllib.error.HTTPError(
            request.full_url,
            500,
            "Server Error",
            {},
            io.BytesIO(b'{"detail":"failed with real-model-secret"}'),
        )

    monkeypatch.setattr("urllib.request.urlopen", fake_urlopen)
    sync = QwenPawModelRuntimeSync(port=8088)

    with pytest.raises(RuntimeError) as excinfo:
        sync.sync(
            MemberRuntimeConfig(
                path=config.runtime_config_path,
                raw={
                    "metadata": {"generation": "2"},
                    "member": {"runtime": "qwenpaw"},
                    "desired": {
                        "model": {
                            "providerId": "agentteams-gateway",
                            "model": "qwen-max",
                            "gatewayUrl": "https://dashscope.aliyuncs.com/compatible-mode",
                            "apiKeyEnv": "REAL_MODEL_KEY",
                        }
                    },
                },
            )
        )

    assert "real-model-secret" not in str(excinfo.value)
    assert "HTTP 500" in str(excinfo.value)


def test_runtime_updater_writes_mcporter_config_from_desired_mcp_servers(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    monkeypatch.setenv("AGENTTEAMS_WORKER_GATEWAY_KEY", "gateway-secret")
    updater = RuntimeUpdater(config=config, package_manager=_NoopPackageManager())

    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "1"},
                "member": {"runtime": "qwenpaw"},
                "credentials": {"gatewayKeyEnv": "AGENTTEAMS_WORKER_GATEWAY_KEY"},
                "desired": {
                    "mcpServers": [
                        {
                            "name": "github",
                            "url": "https://gw.example.com/mcp-servers/github/mcp",
                            "transport": "http",
                        }
                    ]
                },
            },
        )
    )

    legacy_config = config.default_workspace_dir / "mcporter-servers.json"
    default_config = json.loads((config.default_workspace_dir / "config" / "mcporter.json").read_text(encoding="utf-8"))
    assert not legacy_config.exists()
    assert default_config["mcpServers"]["github"] == {
        "url": "https://gw.example.com/mcp-servers/github/mcp",
        "transport": "http",
        "headers": {"Authorization": "Bearer gateway-secret"},
    }


def test_runtime_updater_writes_empty_mcporter_config_when_mcp_servers_are_omitted(
    tmp_path: Path,
) -> None:
    config = _config(tmp_path)
    updater = RuntimeUpdater(config=config, package_manager=_NoopPackageManager())

    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "1"},
                "member": {"runtime": "qwenpaw"},
                "desired": {
                    "mcpServers": [
                        {
                            "name": "github",
                            "url": "https://gw.example.com/mcp-servers/github/mcp",
                        }
                    ]
                },
            },
        )
    )
    legacy_config = config.default_workspace_dir / "mcporter-servers.json"
    default_config = config.default_workspace_dir / "config" / "mcporter.json"
    legacy_config.write_text('{"mcpServers":{"legacy":{"url":"https://old.example.com/mcp"}}}\n', encoding="utf-8")
    assert default_config.exists()

    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "1"},
                "member": {"runtime": "qwenpaw"},
                "desired": {},
            },
        )
    )

    assert not legacy_config.exists()
    assert json.loads(default_config.read_text(encoding="utf-8")) == {"mcpServers": {}}


def test_runtime_updater_configures_matrix_channel_in_agent_config(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    monkeypatch.setenv("AGENTTEAMS_MATRIX_URL", "http://matrix.example.com:6167")
    monkeypatch.setenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "matrix-token")
    monkeypatch.setenv("AGENTTEAMS_MATRIX_E2EE", "1")
    saved = {}
    matrix_config = types.SimpleNamespace(
        enabled=False,
        homeserver="",
        user_id="",
        access_token="",
        password="legacy",
        encryption=True,
        group_disabled=True,
        dm_disabled=True,
        filter_tool_messages=False,
        filter_thinking=False,
        groups={},
    )
    channel_config = types.SimpleNamespace(matrix=matrix_config)
    agent_config = types.SimpleNamespace(channels=channel_config)

    config_module = types.ModuleType("qwenpaw.config.config")
    config_module.load_agent_config = lambda _agent_id: agent_config
    config_module.save_agent_config = lambda agent_id, value: saved.update({agent_id: value})

    monkeypatch.setitem(sys.modules, "qwenpaw", types.ModuleType("qwenpaw"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config", types.ModuleType("qwenpaw.config"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config.config", config_module)

    updater = RuntimeUpdater(config=config, package_manager=_NoopPackageManager())
    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "1"},
                "team": {"teamRoomId": "!team:matrix.local"},
                "member": {
                    "runtime": "qwenpaw",
                    "matrixUserId": "@worker-a:matrix.local",
                },
                "credentials": {"matrixTokenEnv": "AGENTTEAMS_WORKER_MATRIX_TOKEN"},
            },
        )
    )

    matrix = saved["default"].channels.matrix
    assert matrix.enabled is True
    assert matrix.homeserver == "http://matrix.example.com:6167"
    assert matrix.user_id == "@worker-a:matrix.local"
    assert matrix.access_token == "matrix-token"
    assert matrix.password == ""
    assert matrix.encryption is True
    assert matrix.group_disabled is False
    assert matrix.dm_disabled is False
    assert matrix.filter_tool_messages is False
    assert matrix.filter_thinking is False
    assert matrix.groups["*"]["requireMention"] is True
    assert matrix.groups["!team:matrix.local"]["requireMention"] is True


def test_runtime_updater_configures_dingtalk_channel_in_agent_config(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    saved = {}
    dingtalk_config = types.SimpleNamespace(
        enabled=False,
        client_id="",
        client_secret="",
        robot_code="",
        filter_thinking=True,
        filter_tool_messages=False,
        streaming_enabled=False,
        message_type="markdown",
        card_template_id="",
        card_template_key="content",
        card_auto_layout=True,
    )
    channel_config = types.SimpleNamespace(dingtalk=dingtalk_config)
    agent_config = types.SimpleNamespace(channels=channel_config)

    config_module = types.ModuleType("qwenpaw.config.config")
    config_module.load_agent_config = lambda _agent_id: agent_config
    config_module.save_agent_config = lambda agent_id, value: saved.update({agent_id: value})

    monkeypatch.setitem(sys.modules, "qwenpaw", types.ModuleType("qwenpaw"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config", types.ModuleType("qwenpaw.config"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config.config", config_module)

    updater = RuntimeUpdater(config=config, package_manager=_NoopPackageManager())
    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "1"},
                "member": {"runtime": "qwenpaw"},
                "desired": {
                    "channels": {
                        "dingtalk": {
                            "enabled": True,
                            "client_id": "demo-client-id",
                            "client_secret": "test-client-secret",
                            "robot_code": "demo-robot-code",
                            "filter_thinking": False,
                            "filter_tool_messages": True,
                            "streaming_enabled": True,
                            "message_type": "card",
                            "card_template_id": "card-template-1",
                            "card_template_key": "content",
                            "card_auto_layout": False,
                        }
                    }
                },
            },
        )
    )

    dingtalk = saved["default"].channels.dingtalk
    assert dingtalk.enabled is True
    assert dingtalk.client_id == "demo-client-id"
    assert dingtalk.client_secret == "test-client-secret"
    assert dingtalk.robot_code == "demo-robot-code"
    assert dingtalk.filter_thinking is False
    assert dingtalk.filter_tool_messages is True
    assert dingtalk.streaming_enabled is True
    assert dingtalk.message_type == "card"
    assert dingtalk.card_template_id == "card-template-1"
    assert dingtalk.card_template_key == "content"
    assert dingtalk.card_auto_layout is False
    assert not (config.default_workspace_dir / "config.json").exists()


def test_runtime_updater_uses_provided_stream_dingtalk_card_config(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    caplog: pytest.LogCaptureFixture,
) -> None:
    config = _config(tmp_path)
    saved = {}
    dingtalk_config = types.SimpleNamespace(
        enabled=True,
        client_id="old-client-id",
        client_secret="old-secret",
        robot_code="old-robot",
        filter_thinking=True,
        filter_tool_messages=True,
        streaming_enabled=False,
        message_type="card",
        card_template_id="custom-template.schema",
        card_template_key="custom_content",
        card_auto_layout=True,
    )
    channel_config = types.SimpleNamespace(dingtalk=dingtalk_config)
    agent_config = types.SimpleNamespace(channels=channel_config)

    config_module = types.ModuleType("qwenpaw.config.config")
    config_module.load_agent_config = lambda _agent_id: agent_config
    config_module.save_agent_config = lambda agent_id, value: saved.update({agent_id: value})

    monkeypatch.setitem(sys.modules, "qwenpaw", types.ModuleType("qwenpaw"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config", types.ModuleType("qwenpaw.config"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config.config", config_module)

    caplog.set_level(logging.WARNING)
    updater = RuntimeUpdater(
        config=config,
        package_manager=_NoopPackageManager(),
    )
    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "1"},
                "member": {"runtime": "qwenpaw"},
                "desired": {
                    "channels": {
                        "dingtalk": {
                            "enabled": True,
                            "client_id": "demo-client-id",
                            "client_secret": "test-client-secret",
                            "robot_code": "demo-robot-code",
                            "streaming_enabled": True,
                            "filter_thinking": False,
                            "filter_tool_messages": False,
                            "card_template_id": "stream-template.schema",
                        }
                    }
                },
            },
        )
    )

    dingtalk = saved["default"].channels.dingtalk
    assert dingtalk.enabled is True
    assert dingtalk.client_id == "demo-client-id"
    assert dingtalk.client_secret == "test-client-secret"
    assert dingtalk.robot_code == "demo-robot-code"
    assert dingtalk.streaming_enabled is True
    assert dingtalk.message_type == "card"
    assert dingtalk.card_template_id == "stream-template.schema"
    assert dingtalk.card_template_key == "content"
    assert dingtalk.card_auto_layout is False
    assert dingtalk.filter_thinking is False
    assert dingtalk.filter_tool_messages is False
    assert "current runtime card configuration will switch" in caplog.text
    assert "custom-template.schema" in caplog.text
    assert "test-client-secret" not in caplog.text


def test_runtime_updater_rejects_incomplete_dingtalk_streaming_config(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    saved = {}
    dingtalk_config = types.SimpleNamespace(
        enabled=False,
        client_id="",
        client_secret="",
        robot_code="",
    )
    channel_config = types.SimpleNamespace(dingtalk=dingtalk_config)
    agent_config = types.SimpleNamespace(channels=channel_config)

    config_module = types.ModuleType("qwenpaw.config.config")
    config_module.load_agent_config = lambda _agent_id: agent_config
    config_module.save_agent_config = lambda agent_id, value: saved.update({agent_id: value})

    monkeypatch.setitem(sys.modules, "qwenpaw", types.ModuleType("qwenpaw"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config", types.ModuleType("qwenpaw.config"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config.config", config_module)

    updater = RuntimeUpdater(config=config, package_manager=_NoopPackageManager())
    with pytest.raises(ValueError, match="DingTalk streaming requires"):
        updater.apply_once(
            runtime_config=MemberRuntimeConfig(
                path=config.runtime_config_path,
                raw={
                    "metadata": {"generation": "1"},
                    "member": {"runtime": "qwenpaw"},
                    "desired": {
                        "channels": {
                            "dingtalk": {
                                "enabled": True,
                                "client_id": "demo-client-id",
                                "client_secret": "test-client-secret",
                                "streaming_enabled": True,
                            }
                        }
                    },
                },
            )
        )

    assert saved == {}
    assert dingtalk_config.enabled is False


def test_runtime_updater_preserves_card_config_when_dingtalk_streaming_disabled(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    saved = {}
    dingtalk_config = types.SimpleNamespace(
        enabled=True,
        client_id="old-client-id",
        client_secret="old-secret",
        robot_code="old-robot",
        filter_thinking=True,
        filter_tool_messages=True,
        streaming_enabled=True,
        message_type="card",
        card_template_id="custom-template.schema",
        card_template_key="custom_content",
        card_auto_layout=True,
    )
    channel_config = types.SimpleNamespace(dingtalk=dingtalk_config)
    agent_config = types.SimpleNamespace(channels=channel_config)

    config_module = types.ModuleType("qwenpaw.config.config")
    config_module.load_agent_config = lambda _agent_id: agent_config
    config_module.save_agent_config = lambda agent_id, value: saved.update({agent_id: value})

    monkeypatch.setitem(sys.modules, "qwenpaw", types.ModuleType("qwenpaw"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config", types.ModuleType("qwenpaw.config"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config.config", config_module)

    updater = RuntimeUpdater(config=config, package_manager=_NoopPackageManager())
    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "1"},
                "member": {"runtime": "qwenpaw"},
                "desired": {
                    "channels": {
                        "dingtalk": {
                            "enabled": True,
                            "client_id": "demo-client-id",
                            "client_secret": "test-client-secret",
                            "robot_code": "demo-robot-code",
                            "streaming_enabled": False,
                        }
                    }
                },
            },
        )
    )

    dingtalk = saved["default"].channels.dingtalk
    assert dingtalk.streaming_enabled is False
    assert dingtalk.message_type == "card"
    assert dingtalk.card_template_id == "custom-template.schema"
    assert dingtalk.card_template_key == "custom_content"
    assert dingtalk.card_auto_layout is True


def test_runtime_updater_disables_dingtalk_channel_from_runtime_config(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    saved = {}
    dingtalk_config = types.SimpleNamespace(enabled=True, client_secret="existing-secret")
    channel_config = types.SimpleNamespace(dingtalk=dingtalk_config)
    agent_config = types.SimpleNamespace(channels=channel_config)

    config_module = types.ModuleType("qwenpaw.config.config")
    config_module.load_agent_config = lambda _agent_id: agent_config
    config_module.save_agent_config = lambda agent_id, value: saved.update({agent_id: value})

    monkeypatch.setitem(sys.modules, "qwenpaw", types.ModuleType("qwenpaw"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config", types.ModuleType("qwenpaw.config"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config.config", config_module)

    updater = RuntimeUpdater(config=config, package_manager=_NoopPackageManager())
    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "1"},
                "member": {"runtime": "qwenpaw"},
                "desired": {"channels": {"dingtalk": {"enabled": False}}},
            },
        )
    )

    assert saved["default"].channels.dingtalk.enabled is False
    assert saved["default"].channels.dingtalk.client_secret == "existing-secret"
    assert not (config.default_workspace_dir / "config.json").exists()


def test_runtime_updater_leaves_dingtalk_channel_when_runtime_omits_config(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    saved = {}
    dingtalk_config = types.SimpleNamespace(enabled=True, client_id="existing-client-id")
    channel_config = types.SimpleNamespace(dingtalk=dingtalk_config)
    agent_config = types.SimpleNamespace(channels=channel_config)

    config_module = types.ModuleType("qwenpaw.config.config")
    config_module.load_agent_config = lambda _agent_id: agent_config
    config_module.save_agent_config = lambda agent_id, value: saved.update({agent_id: value})

    monkeypatch.setitem(sys.modules, "qwenpaw", types.ModuleType("qwenpaw"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config", types.ModuleType("qwenpaw.config"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config.config", config_module)

    updater = RuntimeUpdater(config=config, package_manager=_NoopPackageManager())
    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "1"},
                "member": {"runtime": "qwenpaw"},
                "desired": {},
            },
        )
    )

    assert saved == {}
    assert dingtalk_config.enabled is True
    assert dingtalk_config.client_id == "existing-client-id"


def test_runtime_updater_applies_copaw_style_matrix_channel_policy(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    saved = {}
    matrix_config = types.SimpleNamespace(access_control_dm=False, access_control_group=False)
    channel_config = types.SimpleNamespace(matrix=matrix_config)
    agent_config = types.SimpleNamespace(channels=channel_config)
    whitelist_calls: list[tuple[str, list[str]]] = []
    blacklist_calls: list[tuple[str, list[str]]] = []
    monkeypatch.setenv("AGENTTEAMS_MATRIX_DOMAIN", "matrix.local")
    monkeypatch.setenv("AGENTTEAMS_ADMIN_USER", "admin")

    config_module = types.ModuleType("qwenpaw.config.config")
    config_module.load_agent_config = lambda _agent_id: agent_config
    config_module.save_agent_config = lambda agent_id, value: saved.update({agent_id: value})

    class Store:
        def set_whitelist(self, channel, values):
            whitelist_calls.append((channel, values))

        def set_blacklist(self, channel, values):
            blacklist_calls.append((channel, values))

    access_module = types.ModuleType("qwenpaw.app.channels.access_control")
    access_module.get_access_control_store = lambda workspace_dir: Store()

    monkeypatch.setitem(sys.modules, "qwenpaw", types.ModuleType("qwenpaw"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config", types.ModuleType("qwenpaw.config"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config.config", config_module)
    monkeypatch.setitem(sys.modules, "qwenpaw.app", types.ModuleType("qwenpaw.app"))
    monkeypatch.setitem(sys.modules, "qwenpaw.app.channels", types.ModuleType("qwenpaw.app.channels"))
    monkeypatch.setitem(sys.modules, "qwenpaw.app.channels.access_control", access_module)

    updater = RuntimeUpdater(config=config, package_manager=_NoopPackageManager())
    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "1"},
                "team": {
                    "leaderRuntimeName": "leader-a",
                    "members": [
                        {
                            "runtimeName": "leader-a",
                            "role": "team_leader",
                            "matrixUserId": "@leader-a:matrix.local",
                        },
                        {
                            "runtimeName": "worker-a",
                            "role": "worker",
                            "matrixUserId": "@worker-a:matrix.local",
                        },
                    ],
                },
                "member": {
                    "runtime": "qwenpaw",
                    "runtimeName": "leader-a",
                    "matrixUserId": "@leader-a:matrix.local",
                    "role": "team_leader",
                },
                "desired": {
                    "channelPolicy": {
                        "groupAllowExtra": ["worker-b"],
                        "dmAllowExtra": ["@human:matrix.local"],
                        "groupDenyExtra": ["blocked-worker"],
                        "dmDenyExtra": ["@blocked:matrix.local"],
                    }
                },
            },
        )
    )

    assert saved["default"].channels.matrix.access_control_group is True
    assert saved["default"].channels.matrix.access_control_dm is True
    assert whitelist_calls == [
        (
            "matrix",
            [
                "@leader-a:matrix.local",
                "@manager:matrix.local",
                "@admin:matrix.local",
                "@worker-a:matrix.local",
                "@worker-b:matrix.local",
                "@human:matrix.local",
            ],
        )
    ]
    assert blacklist_calls == [("matrix", ["@blocked-worker:matrix.local", "@blocked:matrix.local"])]


def test_runtime_updater_self_matrix_id_still_respects_deny_policy(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    saved = {}
    matrix_config = types.SimpleNamespace(access_control_dm=False, access_control_group=False)
    channel_config = types.SimpleNamespace(matrix=matrix_config)
    agent_config = types.SimpleNamespace(channels=channel_config)
    whitelist_calls: list[tuple[str, list[str]]] = []
    blacklist_calls: list[tuple[str, list[str]]] = []
    monkeypatch.setenv("AGENTTEAMS_MATRIX_DOMAIN", "matrix.local")
    monkeypatch.setenv("AGENTTEAMS_ADMIN_USER", "admin")

    config_module = types.ModuleType("qwenpaw.config.config")
    config_module.load_agent_config = lambda _agent_id: agent_config
    config_module.save_agent_config = lambda agent_id, value: saved.update({agent_id: value})

    class Store:
        def set_whitelist(self, channel, values):
            whitelist_calls.append((channel, values))

        def set_blacklist(self, channel, values):
            blacklist_calls.append((channel, values))

    access_module = types.ModuleType("qwenpaw.app.channels.access_control")
    access_module.get_access_control_store = lambda workspace_dir: Store()

    monkeypatch.setitem(sys.modules, "qwenpaw", types.ModuleType("qwenpaw"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config", types.ModuleType("qwenpaw.config"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config.config", config_module)
    monkeypatch.setitem(sys.modules, "qwenpaw.app", types.ModuleType("qwenpaw.app"))
    monkeypatch.setitem(sys.modules, "qwenpaw.app.channels", types.ModuleType("qwenpaw.app.channels"))
    monkeypatch.setitem(sys.modules, "qwenpaw.app.channels.access_control", access_module)

    updater = RuntimeUpdater(config=config, package_manager=_NoopPackageManager())
    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "1"},
                "team": {
                    "leaderRuntimeName": "leader-a",
                    "members": [
                        {
                            "runtimeName": "leader-a",
                            "role": "team_leader",
                            "matrixUserId": "@leader-a:matrix.local",
                        },
                        {
                            "runtimeName": "worker-a",
                            "role": "worker",
                            "matrixUserId": "@worker-a:matrix.local",
                        },
                    ],
                },
                "member": {
                    "runtime": "qwenpaw",
                    "runtimeName": "leader-a",
                    "matrixUserId": "@leader-a:matrix.local",
                    "role": "team_leader",
                },
                "desired": {
                    "channelPolicy": {
                        "dmDenyExtra": ["@leader-a:matrix.local"],
                    },
                },
            },
        )
    )

    assert "@leader-a:matrix.local" not in whitelist_calls[0][1]
    assert blacklist_calls == [("matrix", ["@leader-a:matrix.local"])]


def test_runtime_updater_applies_team_roster_matrix_defaults_for_team_worker(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    saved = {}
    matrix_config = types.SimpleNamespace(access_control_dm=False, access_control_group=False)
    channel_config = types.SimpleNamespace(matrix=matrix_config)
    agent_config = types.SimpleNamespace(channels=channel_config)
    whitelist_calls: list[tuple[str, list[str]]] = []
    blacklist_calls: list[tuple[str, list[str]]] = []
    monkeypatch.setenv("AGENTTEAMS_MATRIX_DOMAIN", "matrix.local")
    monkeypatch.setenv("AGENTTEAMS_ADMIN_USER", "admin")

    config_module = types.ModuleType("qwenpaw.config.config")
    config_module.load_agent_config = lambda _agent_id: agent_config
    config_module.save_agent_config = lambda agent_id, value: saved.update({agent_id: value})

    class Store:
        def set_whitelist(self, channel, values):
            whitelist_calls.append((channel, values))

        def set_blacklist(self, channel, values):
            blacklist_calls.append((channel, values))

    access_module = types.ModuleType("qwenpaw.app.channels.access_control")
    access_module.get_access_control_store = lambda workspace_dir: Store()

    monkeypatch.setitem(sys.modules, "qwenpaw", types.ModuleType("qwenpaw"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config", types.ModuleType("qwenpaw.config"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config.config", config_module)
    monkeypatch.setitem(sys.modules, "qwenpaw.app", types.ModuleType("qwenpaw.app"))
    monkeypatch.setitem(sys.modules, "qwenpaw.app.channels", types.ModuleType("qwenpaw.app.channels"))
    monkeypatch.setitem(sys.modules, "qwenpaw.app.channels.access_control", access_module)

    updater = RuntimeUpdater(config=config, package_manager=_NoopPackageManager())
    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "1"},
                "team": {
                    "leaderRuntimeName": "leader-a",
                    "members": [
                        {
                            "runtimeName": "leader-a",
                            "role": "team_leader",
                            "matrixUserId": "@leader-a:matrix.local",
                        },
                        {
                            "runtimeName": "worker-a",
                            "role": "worker",
                            "matrixUserId": "@worker-a:matrix.local",
                        },
                        {
                            "runtimeName": "worker-b",
                            "role": "worker",
                            "matrixUserId": "@worker-b:matrix.local",
                        },
                    ],
                },
                "member": {
                    "runtime": "qwenpaw",
                    "runtimeName": "worker-a",
                    "matrixUserId": "@worker-a:matrix.local",
                    "role": "worker",
                },
            },
        )
    )

    assert saved["default"].channels.matrix.access_control_group is True
    assert saved["default"].channels.matrix.access_control_dm is True
    assert whitelist_calls == [
        ("matrix", ["@worker-a:matrix.local", "@leader-a:matrix.local", "@admin:matrix.local", "@worker-b:matrix.local"])
    ]
    assert blacklist_calls == [("matrix", [])]


def test_runtime_updater_system_admin_in_allowlist_when_team_admin_differs(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Regression: system admin must remain in whitelist even when
    team.admin.matrixUserId is a *different* user than AGENTTEAMS_ADMIN_USER."""
    config = _config(tmp_path)
    saved = {}
    matrix_config = types.SimpleNamespace(access_control_dm=False, access_control_group=False)
    channel_config = types.SimpleNamespace(matrix=matrix_config)
    agent_config = types.SimpleNamespace(channels=channel_config)
    whitelist_calls: list[tuple[str, list[str]]] = []
    blacklist_calls: list[tuple[str, list[str]]] = []
    monkeypatch.setenv("AGENTTEAMS_MATRIX_DOMAIN", "matrix.local")
    # System admin is "sysadmin", but team admin is a different user.
    monkeypatch.setenv("AGENTTEAMS_ADMIN_USER", "sysadmin")

    config_module = types.ModuleType("qwenpaw.config.config")
    config_module.load_agent_config = lambda _agent_id: agent_config
    config_module.save_agent_config = lambda agent_id, value: saved.update({agent_id: value})

    class Store:
        def set_whitelist(self, channel, values):
            whitelist_calls.append((channel, values))

        def set_blacklist(self, channel, values):
            blacklist_calls.append((channel, values))

    access_module = types.ModuleType("qwenpaw.app.channels.access_control")
    access_module.get_access_control_store = lambda workspace_dir: Store()

    monkeypatch.setitem(sys.modules, "qwenpaw", types.ModuleType("qwenpaw"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config", types.ModuleType("qwenpaw.config"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config.config", config_module)
    monkeypatch.setitem(sys.modules, "qwenpaw.app", types.ModuleType("qwenpaw.app"))
    monkeypatch.setitem(sys.modules, "qwenpaw.app.channels", types.ModuleType("qwenpaw.app.channels"))
    monkeypatch.setitem(sys.modules, "qwenpaw.app.channels.access_control", access_module)

    updater = RuntimeUpdater(config=config, package_manager=_NoopPackageManager())
    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "1"},
                "team": {
                    "leaderRuntimeName": "leader-a",
                    "admin": {"matrixUserId": "@team-human:matrix.local"},
                    "members": [
                        {
                            "runtimeName": "leader-a",
                            "role": "team_leader",
                            "matrixUserId": "@leader-a:matrix.local",
                        },
                        {
                            "runtimeName": "worker-a",
                            "role": "worker",
                            "matrixUserId": "@worker-a:matrix.local",
                        },
                    ],
                },
                "member": {
                    "runtime": "qwenpaw",
                    "runtimeName": "worker-a",
                    "matrixUserId": "@worker-a:matrix.local",
                    "role": "worker",
                },
            },
        )
    )

    # Both team admin (@team-human) and system admin (@sysadmin) must be present.
    assert len(whitelist_calls) == 1
    channel, wl = whitelist_calls[0]
    assert channel == "matrix"
    assert "@team-human:matrix.local" in wl, "team admin must be in whitelist"
    assert "@sysadmin:matrix.local" in wl, "system admin must be in whitelist"
    assert "@leader-a:matrix.local" in wl, "leader must be in whitelist"
    assert "@worker-a:matrix.local" in wl, "current member must be in whitelist"


def test_runtime_updater_applies_copaw_style_matrix_defaults_for_team_worker(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    saved = {}
    matrix_config = types.SimpleNamespace(access_control_dm=False, access_control_group=False)
    channel_config = types.SimpleNamespace(matrix=matrix_config)
    agent_config = types.SimpleNamespace(channels=channel_config)
    whitelist_calls: list[tuple[str, list[str]]] = []
    blacklist_calls: list[tuple[str, list[str]]] = []
    monkeypatch.setenv("AGENTTEAMS_MATRIX_DOMAIN", "matrix.local")
    monkeypatch.setenv("AGENTTEAMS_ADMIN_USER", "admin")

    config_module = types.ModuleType("qwenpaw.config.config")
    config_module.load_agent_config = lambda _agent_id: agent_config
    config_module.save_agent_config = lambda agent_id, value: saved.update({agent_id: value})

    class Store:
        def set_whitelist(self, channel, values):
            whitelist_calls.append((channel, values))

        def set_blacklist(self, channel, values):
            blacklist_calls.append((channel, values))

    access_module = types.ModuleType("qwenpaw.app.channels.access_control")
    access_module.get_access_control_store = lambda workspace_dir: Store()

    monkeypatch.setitem(sys.modules, "qwenpaw", types.ModuleType("qwenpaw"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config", types.ModuleType("qwenpaw.config"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config.config", config_module)
    monkeypatch.setitem(sys.modules, "qwenpaw.app", types.ModuleType("qwenpaw.app"))
    monkeypatch.setitem(sys.modules, "qwenpaw.app.channels", types.ModuleType("qwenpaw.app.channels"))
    monkeypatch.setitem(sys.modules, "qwenpaw.app.channels.access_control", access_module)

    updater = RuntimeUpdater(config=config, package_manager=_NoopPackageManager())
    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "1"},
                "team": {"leaderRuntimeName": "leader-a"},
                "member": {"runtime": "qwenpaw", "role": "worker"},
            },
        )
    )

    assert saved["default"].channels.matrix.access_control_group is True
    assert saved["default"].channels.matrix.access_control_dm is True
    assert whitelist_calls == [("matrix", ["@leader-a:matrix.local", "@admin:matrix.local"])]
    assert blacklist_calls == [("matrix", [])]


def test_runtime_updater_applies_copaw_style_matrix_defaults_for_standalone_worker(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    saved = {}
    matrix_config = types.SimpleNamespace(access_control_dm=False, access_control_group=False)
    channel_config = types.SimpleNamespace(matrix=matrix_config)
    agent_config = types.SimpleNamespace(channels=channel_config)
    whitelist_calls: list[tuple[str, list[str]]] = []
    blacklist_calls: list[tuple[str, list[str]]] = []
    monkeypatch.setenv("AGENTTEAMS_MATRIX_DOMAIN", "matrix.local")
    monkeypatch.setenv("AGENTTEAMS_ADMIN_USER", "admin")

    config_module = types.ModuleType("qwenpaw.config.config")
    config_module.load_agent_config = lambda _agent_id: agent_config
    config_module.save_agent_config = lambda agent_id, value: saved.update({agent_id: value})

    class Store:
        def set_whitelist(self, channel, values):
            whitelist_calls.append((channel, values))

        def set_blacklist(self, channel, values):
            blacklist_calls.append((channel, values))

    access_module = types.ModuleType("qwenpaw.app.channels.access_control")
    access_module.get_access_control_store = lambda workspace_dir: Store()

    monkeypatch.setitem(sys.modules, "qwenpaw", types.ModuleType("qwenpaw"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config", types.ModuleType("qwenpaw.config"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config.config", config_module)
    monkeypatch.setitem(sys.modules, "qwenpaw.app", types.ModuleType("qwenpaw.app"))
    monkeypatch.setitem(sys.modules, "qwenpaw.app.channels", types.ModuleType("qwenpaw.app.channels"))
    monkeypatch.setitem(sys.modules, "qwenpaw.app.channels.access_control", access_module)

    updater = RuntimeUpdater(config=config, package_manager=_NoopPackageManager())
    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "1"},
                "member": {"runtime": "qwenpaw", "role": "worker"},
            },
        )
    )

    assert saved["default"].channels.matrix.access_control_group is True
    assert saved["default"].channels.matrix.access_control_dm is True
    assert whitelist_calls == [("matrix", ["@manager:matrix.local", "@admin:matrix.local"])]
    assert blacklist_calls == [("matrix", [])]


def test_runtime_updater_skips_short_name_matrix_policy_without_matrix_domain(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    saved = {}
    matrix_config = types.SimpleNamespace(access_control_dm=False, access_control_group=False)
    channel_config = types.SimpleNamespace(matrix=matrix_config)
    agent_config = types.SimpleNamespace(channels=channel_config)
    whitelist_calls: list[tuple[str, list[str]]] = []
    blacklist_calls: list[tuple[str, list[str]]] = []
    monkeypatch.delenv("AGENTTEAMS_MATRIX_DOMAIN", raising=False)
    monkeypatch.setenv("AGENTTEAMS_ADMIN_USER", "admin")

    config_module = types.ModuleType("qwenpaw.config.config")
    config_module.load_agent_config = lambda _agent_id: agent_config
    config_module.save_agent_config = lambda agent_id, value: saved.update({agent_id: value})

    class Store:
        def set_whitelist(self, channel, values):
            whitelist_calls.append((channel, values))

        def set_blacklist(self, channel, values):
            blacklist_calls.append((channel, values))

    access_module = types.ModuleType("qwenpaw.app.channels.access_control")
    access_module.get_access_control_store = lambda workspace_dir: Store()

    monkeypatch.setitem(sys.modules, "qwenpaw", types.ModuleType("qwenpaw"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config", types.ModuleType("qwenpaw.config"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config.config", config_module)
    monkeypatch.setitem(sys.modules, "qwenpaw.app", types.ModuleType("qwenpaw.app"))
    monkeypatch.setitem(sys.modules, "qwenpaw.app.channels", types.ModuleType("qwenpaw.app.channels"))
    monkeypatch.setitem(sys.modules, "qwenpaw.app.channels.access_control", access_module)

    updater = RuntimeUpdater(config=config, package_manager=_NoopPackageManager())
    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "1"},
                "member": {"runtime": "qwenpaw", "role": "worker"},
                "desired": {
                    "channelPolicy": {
                        "groupAllowExtra": ["worker-b"],
                        "dmDenyExtra": ["blocked-worker"],
                    }
                },
            },
        )
    )

    assert saved == {}
    assert whitelist_calls == []
    assert blacklist_calls == []


@pytest.mark.anyio
async def test_runtime_update_loop_survives_bad_config_and_applies_next_change(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    caplog: pytest.LogCaptureFixture,
) -> None:
    config = _config(tmp_path)
    updater = RuntimeUpdater(config=config)
    caplog.set_level(logging.INFO, logger="qwenpaw_worker.update")
    loads = [
        RuntimeError("runtime config parse failed secret-token-value"),
        MemberRuntimeConfig(path=config.runtime_config_path, raw={"metadata": {"generation": "2"}}),
    ]
    applied: list[str] = []
    sleeps = 0

    async def sleep_tick(_seconds):
        nonlocal sleeps
        sleeps += 1
        if sleeps > 2:
            raise asyncio.CancelledError

    def fake_load(_path):
        value = loads.pop(0)
        if isinstance(value, Exception):
            raise value
        return value

    def fake_apply_once(self, runtime_config=None, force=False, reapply_adapter=True):
        assert runtime_config is not None
        applied.append(runtime_config.generation)
        self.current_config = runtime_config

    monkeypatch.setattr("qwenpaw_worker.update.asyncio.sleep", sleep_tick)
    monkeypatch.setattr("qwenpaw_worker.update.MemberRuntimeConfig.load", fake_load)
    monkeypatch.setattr("qwenpaw_worker.update.RuntimeUpdater.apply_once", fake_apply_once)

    with pytest.raises(asyncio.CancelledError):
        await updater.loop()

    assert applied == ["2"]
    assert "runtime config update loop started component=update" in caplog.text
    assert "runtime config update failed component=update" in caplog.text
    assert "error_type=RuntimeError" in caplog.text
    assert "runtime config update loop stopped component=update" in caplog.text
    assert "secret-token-value" not in caplog.text


@pytest.mark.anyio
async def test_runtime_update_loop_offloads_load_and_apply_to_thread(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    updater = RuntimeUpdater(config=config)
    applied: list[str] = []
    to_thread_calls: list[str] = []
    sleeps = 0

    async def sleep_tick(_seconds):
        nonlocal sleeps
        sleeps += 1
        if sleeps > 1:
            raise asyncio.CancelledError

    async def fake_to_thread(func, *args, **kwargs):
        to_thread_calls.append(getattr(func, "__name__", repr(func)))
        return func(*args, **kwargs)

    def fake_load(_path):
        return MemberRuntimeConfig(path=config.runtime_config_path, raw={"metadata": {"generation": "3"}})

    def fake_apply_once(self, runtime_config=None, force=False, reapply_adapter=True):
        assert runtime_config is not None
        applied.append(runtime_config.generation)
        self.current_config = runtime_config

    monkeypatch.setattr("qwenpaw_worker.update.asyncio.sleep", sleep_tick)
    monkeypatch.setattr("qwenpaw_worker.update.asyncio.to_thread", fake_to_thread)
    monkeypatch.setattr("qwenpaw_worker.update.MemberRuntimeConfig.load", fake_load)
    monkeypatch.setattr("qwenpaw_worker.update.RuntimeUpdater.apply_once", fake_apply_once)

    with pytest.raises(asyncio.CancelledError):
        await updater.loop()

    assert to_thread_calls == ["_load_and_apply_once"]
    assert applied == ["3"]


def test_runtime_updater_pulls_runtime_config_before_loading(tmp_path: Path) -> None:
    config = _config(tmp_path)
    pulls: list[str] = []

    def pull_runtime_config() -> None:
        pulls.append("pull")
        config.runtime_config_path.parent.mkdir(parents=True, exist_ok=True)
        config.runtime_config_path.write_text(
            """
metadata:
  generation: "7"
member:
  runtime: qwenpaw
""",
            encoding="utf-8",
        )

    updater = RuntimeUpdater(config=config, runtime_config_pull=pull_runtime_config)

    loaded = updater.load()

    assert pulls == ["pull"]
    assert loaded.generation == "7"


class _NoopPackageManager:
    def apply(self, _runtime_config: MemberRuntimeConfig):
        return None
