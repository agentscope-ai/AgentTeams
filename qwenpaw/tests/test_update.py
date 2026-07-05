import asyncio
import json
import logging
import os
from pathlib import Path
import sys
import types

import pytest

from qwenpaw_worker.config import WorkerConfig
from qwenpaw_worker.update import MemberRuntimeConfig, RuntimeUpdater


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
                "desired": {"model": {"providerId": "hiclaw-gateway", "model": "qwen-plus"}},
            },
        )
    )

    assert saved["default"].active_model.provider_id == "hiclaw-gateway"
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
                        "providerId": "hiclaw-gateway",
                        "model": "qwen-plus",
                        "gatewayUrl": "https://dashscope.aliyuncs.com/compatible-mode",
                        "apiKeyEnv": "REAL_MODEL_KEY",
                    }
                },
            },
        )
    )

    provider = saved_provider["hiclaw-gateway"]
    assert provider.base_url == "https://dashscope.aliyuncs.com/compatible-mode/v1"
    assert provider.api_key == "real-model-secret"
    assert provider.chat_model == "OpenAIChatModel"
    assert provider.models[0].id == "qwen-plus"
    assert manager.custom_providers["hiclaw-gateway"] is provider
    assert saved_active["active"].provider_id == "hiclaw-gateway"
    assert saved_active["active"].model == "qwen-plus"
    assert saved_agent["default"].active_model.provider_id == "hiclaw-gateway"
    assert saved_agent["default"].active_model.model == "qwen-plus"


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
