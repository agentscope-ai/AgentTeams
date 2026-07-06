import importlib.util
from pathlib import Path
import sys
import types

import pytest

from qwenpaw_worker.update import MemberRuntimeConfig


REPO_ROOT = Path(__file__).resolve().parents[5]
PLUGIN = REPO_ROOT / "plugins" / "teamharness" / "adapters" / "qwenpaw" / "plugin.py"


def _load_plugin():
    spec = importlib.util.spec_from_file_location("teamharness_qwenpaw_credential_hooks_test", PLUGIN)
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


class FakeAgentIdentityDataClient:
    def __init__(self) -> None:
        self.calls: list[str] = []

    def get_workload_access_token(self, workload_identity_name: str) -> str:
        self.calls.append(f"token:{workload_identity_name}")
        return "workload-token"

    def get_resource_api_key(self, provider_name: str, workload_access_token: str) -> str:
        self.calls.append(f"api-key:{provider_name}:{workload_access_token}")
        return f"real-{provider_name.lower()}"


def _runtime_config(tmp_path: Path) -> MemberRuntimeConfig:
    return MemberRuntimeConfig(
        path=tmp_path / "runtime.yaml",
        raw={
            "desired": {
                "agentIdentity": {"workloadIdentityName": "wi-worker-a"},
                "credentialBindings": [
                    {
                        "credentialRef": {
                            "tokenVaultName": "default",
                            "apiKeyCredentialProviderName": "GITHUB_TOKEN",
                        },
                        "toolWhitelist": ["gh"],
                    },
                    {
                        "credentialRef": {
                            "tokenVaultName": "default",
                            "apiKeyCredentialProviderName": "ALIBABA_CLOUD_ACCESS_KEY_ID",
                        },
                        "toolWhitelist": ["aliyun"],
                    },
                ],
            },
        },
    )


def _runtime_config_without_tool_whitelist(tmp_path: Path) -> MemberRuntimeConfig:
    return MemberRuntimeConfig(
        path=tmp_path / "runtime.yaml",
        raw={
            "desired": {
                "agentIdentity": {"workloadIdentityName": "wi-worker-a"},
                "credentialBindings": [
                    {
                        "credentialRef": {
                            "tokenVaultName": "default",
                            "apiKeyCredentialProviderName": "GITHUB_TOKEN",
                        }
                    },
                ],
            },
        },
    )


def _runtime_config_with_instance_prefixed_providers(tmp_path: Path) -> MemberRuntimeConfig:
    return MemberRuntimeConfig(
        path=tmp_path / "runtime.yaml",
        raw={
            "desired": {
                "agentIdentity": {"workloadIdentityName": "wi-worker-a"},
                "credentialBindings": [
                    {
                        "credentialRef": {
                            "tokenVaultName": "default",
                            "apiKeyCredentialProviderName": "at-cn-4b84u92kf0f-ALIBABA_CLOUD_ACCESS_KEY_ID",
                        },
                        "toolWhitelist": ["aliyun"],
                    },
                    {
                        "credentialRef": {
                            "tokenVaultName": "default",
                            "apiKeyCredentialProviderName": "at-cn-4b84u92kf0f-ALIBABA_CLOUD_ACCESS_KEY_SECRET",
                        },
                        "toolWhitelist": ["aliyun"],
                    },
                ],
            },
        },
    )


def test_shell_command_env_overlay_resolves_referenced_bound_credentials(tmp_path: Path) -> None:
    module = _load_plugin()
    client = FakeAgentIdentityDataClient()

    env = module.credential_env_for_shell_command(
        "curl -H 'Authorization: Bearer $GITHUB_TOKEN' https://example.test",
        _runtime_config(tmp_path),
        client,
    )

    assert env == {"GITHUB_TOKEN": "real-github_token"}
    assert client.calls == [
        "token:wi-worker-a",
        "api-key:GITHUB_TOKEN:workload-token",
    ]


def test_shell_command_env_overlay_uses_braced_env_references(tmp_path: Path) -> None:
    module = _load_plugin()
    client = FakeAgentIdentityDataClient()

    env = module.credential_env_for_shell_command(
        "aliyun sts GetCallerIdentity --access-key-id ${ALIBABA_CLOUD_ACCESS_KEY_ID}",
        _runtime_config(tmp_path),
        client,
    )

    assert env == {"ALIBABA_CLOUD_ACCESS_KEY_ID": "real-alibaba_cloud_access_key_id"}


def test_shell_command_env_overlay_resolves_whitelisted_tool(tmp_path: Path) -> None:
    module = _load_plugin()
    client = FakeAgentIdentityDataClient()

    env = module.credential_env_for_shell_command(
        "gh api user",
        _runtime_config(tmp_path),
        client,
    )

    assert env == {"GITHUB_TOKEN": "real-github_token"}
    assert client.calls == [
        "token:wi-worker-a",
        "api-key:GITHUB_TOKEN:workload-token",
    ]


def test_shell_command_env_overlay_resolves_wrapped_whitelisted_tool(tmp_path: Path) -> None:
    module = _load_plugin()
    client = FakeAgentIdentityDataClient()

    env = module.credential_env_for_shell_command(
        "PATH=/usr/bin env FOO=bar gh api user && sudo -E aliyun sts GetCallerIdentity",
        _runtime_config(tmp_path),
        client,
    )

    assert env == {
        "GITHUB_TOKEN": "real-github_token",
        "ALIBABA_CLOUD_ACCESS_KEY_ID": "real-alibaba_cloud_access_key_id",
    }
    assert client.calls == [
        "token:wi-worker-a",
        "api-key:GITHUB_TOKEN:workload-token",
        "api-key:ALIBABA_CLOUD_ACCESS_KEY_ID:workload-token",
    ]


def test_shell_command_env_overlay_ignores_whitelist_text_outside_tool_position(
    tmp_path: Path,
) -> None:
    module = _load_plugin()
    client = FakeAgentIdentityDataClient()

    env = module.credential_env_for_shell_command(
        "curl https://example.test/gh && echo aliyun # gh aliyun",
        _runtime_config(tmp_path),
        client,
    )

    assert env == {}
    assert client.calls == []


def test_shell_command_env_overlay_scopes_tool_whitelist_per_binding(tmp_path: Path) -> None:
    module = _load_plugin()
    client = FakeAgentIdentityDataClient()

    env = module.credential_env_for_shell_command(
        "aliyun sts GetCallerIdentity",
        _runtime_config(tmp_path),
        client,
    )

    assert env == {"ALIBABA_CLOUD_ACCESS_KEY_ID": "real-alibaba_cloud_access_key_id"}
    assert client.calls == [
        "token:wi-worker-a",
        "api-key:ALIBABA_CLOUD_ACCESS_KEY_ID:workload-token",
    ]


def test_shell_command_env_overlay_uses_instance_prefixed_provider_names(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv(
        "AGENTTEAMS_CONTROLLER_URL",
        "http://controller.at-cn-4b84u92kf0f.vpc.agentteams.aliyuncs.com",
    )
    module = _load_plugin()
    client = FakeAgentIdentityDataClient()

    env = module.credential_env_for_shell_command(
        "aliyun sts GetCallerIdentity",
        _runtime_config_with_instance_prefixed_providers(tmp_path),
        client,
    )

    assert env == {
        "ALIBABA_CLOUD_ACCESS_KEY_ID": "real-at-cn-4b84u92kf0f-alibaba_cloud_access_key_id",
        "ALIBABA_CLOUD_ACCESS_KEY_SECRET": "real-at-cn-4b84u92kf0f-alibaba_cloud_access_key_secret",
    }
    assert client.calls == [
        "token:wi-worker-a",
        "api-key:at-cn-4b84u92kf0f-ALIBABA_CLOUD_ACCESS_KEY_ID:workload-token",
        "api-key:at-cn-4b84u92kf0f-ALIBABA_CLOUD_ACCESS_KEY_SECRET:workload-token",
    ]


def test_shell_command_env_overlay_ignores_tool_without_whitelist(tmp_path: Path) -> None:
    module = _load_plugin()
    client = FakeAgentIdentityDataClient()

    env = module.credential_env_for_shell_command(
        "gh api user",
        _runtime_config_without_tool_whitelist(tmp_path),
        client,
    )

    assert env == {}
    assert client.calls == []


def test_shell_command_env_overlay_does_not_resolve_unreferenced_bindings(tmp_path: Path) -> None:
    module = _load_plugin()
    client = FakeAgentIdentityDataClient()

    env = module.credential_env_for_shell_command(
        "echo hello",
        _runtime_config(tmp_path),
        client,
    )

    assert env == {}
    assert client.calls == []


def test_shell_command_env_overlay_ignores_unbound_env_references(tmp_path: Path) -> None:
    module = _load_plugin()
    client = FakeAgentIdentityDataClient()

    env = module.credential_env_for_shell_command(
        "echo $NOT_BOUND",
        _runtime_config(tmp_path),
        client,
    )

    assert env == {}
    assert client.calls == []


@pytest.mark.anyio
async def test_agentscope2_before_hook_sets_overlay_for_referenced_credentials(tmp_path: Path) -> None:
    module = _load_plugin()
    client = FakeAgentIdentityDataClient()
    before = module.make_shell_credential_before_hook(
        config_loader=lambda: _runtime_config(tmp_path),
        data_client_factory=lambda _config: client,
    )

    await before({"command": "echo ${GITHUB_TOKEN}"}, object())

    assert module.current_credential_env_overlay() == {"GITHUB_TOKEN": "real-github_token"}
    module.clear_current_credential_env_overlay()


@pytest.mark.anyio
async def test_agentscope2_after_hook_clears_overlay() -> None:
    module = _load_plugin()
    module.set_current_credential_env_overlay({"GITHUB_TOKEN": "real-token"})

    response = await module.make_shell_credential_after_hook()("result", object())

    assert response == "result"
    assert module.current_credential_env_overlay() == {}


@pytest.mark.anyio
async def test_agentscope2_after_hook_sanitizes_before_clearing_overlay() -> None:
    module = _load_plugin()
    module.set_current_credential_env_overlay({"GITHUB_TOKEN": "real-secret-token"})

    def sanitizer(result):
        for value in module.current_credential_secret_values():
            result["content"] = result["content"].replace(value, "[REDACTED]")
        return result

    response = await module.make_shell_credential_after_hook(result_sanitizer=sanitizer)(
        {"content": "token=real-secret-token"},
        object(),
    )

    assert response == {"content": "token=[REDACTED]"}
    assert module.current_credential_env_overlay() == {}


@pytest.mark.anyio
async def test_agentscope2_after_hook_keeps_original_response_when_sanitizer_returns_summary() -> None:
    module = _load_plugin()
    module.set_current_credential_env_overlay({"GITHUB_TOKEN": "real-secret-token"})
    response = {"content": "token=real-secret-token"}

    def sanitizer(result):
        result["content"] = result["content"].replace("real-secret-token", "[REDACTED]")
        return {"ok": True, "redacted": True}

    returned = await module.make_shell_credential_after_hook(result_sanitizer=sanitizer)(
        response,
        object(),
    )

    assert returned is response
    assert returned == {"content": "token=[REDACTED]"}
    assert module.current_credential_env_overlay() == {}


def test_subprocess_env_merge_uses_current_overlay_without_mutating_base() -> None:
    module = _load_plugin()
    base_env = {"PATH": "/bin"}
    module.set_current_credential_env_overlay({"GITHUB_TOKEN": "real-token"})

    merged = module.merge_env_with_credential_overlay(base_env)

    assert merged == {"PATH": "/bin", "GITHUB_TOKEN": "real-token"}
    assert base_env == {"PATH": "/bin"}
    module.clear_current_credential_env_overlay()


@pytest.mark.anyio
async def test_legacy_shell_hook_merges_overlay_into_subprocess_env(tmp_path: Path) -> None:
    module = _load_plugin()
    captured: dict[str, dict[str, str]] = {}

    async def create_subprocess_shell(_command: str, **kwargs):
        captured["env"] = kwargs["env"]
        return "process"

    shell_module = types.SimpleNamespace()
    shell_module.asyncio = types.SimpleNamespace(create_subprocess_shell=create_subprocess_shell)

    async def execute_shell_command(command: str):
        return await shell_module.asyncio.create_subprocess_shell(
            command,
            env={"PATH": "/bin"},
        )

    shell_module.execute_shell_command = execute_shell_command

    module.install_legacy_shell_credential_hook(
        shell_module,
        config_loader=lambda: _runtime_config(tmp_path),
        data_client_factory=lambda _config: FakeAgentIdentityDataClient(),
    )

    assert shell_module.execute_shell_command.__name__ == "execute_shell_command"

    result = await shell_module.execute_shell_command("echo $GITHUB_TOKEN")

    assert result == "process"
    assert captured["env"] == {"PATH": "/bin", "GITHUB_TOKEN": "real-github_token"}
    assert module.current_credential_env_overlay() == {}


@pytest.mark.anyio
async def test_legacy_shell_hook_merges_whitelisted_tool_overlay(tmp_path: Path) -> None:
    module = _load_plugin()
    captured: dict[str, dict[str, str]] = {}

    async def create_subprocess_shell(_command: str, **kwargs):
        captured["env"] = kwargs["env"]
        return "process"

    shell_module = types.SimpleNamespace()
    shell_module.asyncio = types.SimpleNamespace(create_subprocess_shell=create_subprocess_shell)

    async def execute_shell_command(command: str):
        return await shell_module.asyncio.create_subprocess_shell(
            command,
            env={"PATH": "/bin"},
        )

    shell_module.execute_shell_command = execute_shell_command

    module.install_legacy_shell_credential_hook(
        shell_module,
        config_loader=lambda: _runtime_config(tmp_path),
        data_client_factory=lambda _config: FakeAgentIdentityDataClient(),
    )

    result = await shell_module.execute_shell_command("gh api user")

    assert result == "process"
    assert captured["env"] == {"PATH": "/bin", "GITHUB_TOKEN": "real-github_token"}
    assert module.current_credential_env_overlay() == {}


@pytest.mark.anyio
async def test_legacy_shell_hook_keeps_original_tool_response_when_sanitizer_returns_summary(
    tmp_path: Path,
) -> None:
    module = _load_plugin()
    response = {"content": "token=real-github_token"}

    async def create_subprocess_shell(_command: str, **_kwargs):
        return "process"

    shell_module = types.SimpleNamespace()
    shell_module.asyncio = types.SimpleNamespace(create_subprocess_shell=create_subprocess_shell)

    async def execute_shell_command(command: str):
        return response

    def sanitizer(result):
        result["content"] = result["content"].replace("real-github_token", "[REDACTED]")
        return {"ok": True, "redacted": True}

    shell_module.execute_shell_command = execute_shell_command

    module.install_legacy_shell_credential_hook(
        shell_module,
        config_loader=lambda: _runtime_config(tmp_path),
        data_client_factory=lambda _config: FakeAgentIdentityDataClient(),
        result_sanitizer=sanitizer,
    )

    result = await shell_module.execute_shell_command("echo $GITHUB_TOKEN")

    assert result is response
    assert result == {"content": "token=[REDACTED]"}
    assert module.current_credential_env_overlay() == {}


@pytest.mark.anyio
async def test_legacy_shell_hook_preserves_subprocess_env_without_overlay(tmp_path: Path) -> None:
    module = _load_plugin()
    captured: dict[str, object] = {}

    async def create_subprocess_shell(_command: str, **kwargs):
        captured.update(kwargs)
        return "process"

    shell_module = types.SimpleNamespace()
    shell_module.asyncio = types.SimpleNamespace(create_subprocess_shell=create_subprocess_shell)

    async def execute_shell_command(command: str):
        return await shell_module.asyncio.create_subprocess_shell(command)

    shell_module.execute_shell_command = execute_shell_command

    module.install_legacy_shell_credential_hook(
        shell_module,
        config_loader=lambda: _runtime_config(tmp_path),
        data_client_factory=lambda _config: FakeAgentIdentityDataClient(),
    )

    result = await shell_module.execute_shell_command("echo hello")

    assert result == "process"
    assert "env" not in captured


@pytest.mark.anyio
async def test_legacy_shell_hook_does_not_create_data_client_without_bound_reference(tmp_path: Path) -> None:
    module = _load_plugin()
    created = False

    async def create_subprocess_shell(_command: str, **_kwargs):
        return "process"

    shell_module = types.SimpleNamespace()
    shell_module.asyncio = types.SimpleNamespace(create_subprocess_shell=create_subprocess_shell)

    async def execute_shell_command(command: str):
        return await shell_module.asyncio.create_subprocess_shell(command)

    shell_module.execute_shell_command = execute_shell_command

    def data_client_factory(_config: MemberRuntimeConfig) -> FakeAgentIdentityDataClient:
        nonlocal created
        created = True
        return FakeAgentIdentityDataClient()

    module.install_legacy_shell_credential_hook(
        shell_module,
        config_loader=lambda: _runtime_config(tmp_path),
        data_client_factory=data_client_factory,
    )

    result = await shell_module.execute_shell_command("echo hello")

    assert result == "process"
    assert not created


@pytest.mark.anyio
async def test_legacy_shell_hook_returns_error_response_when_credential_resolution_fails(
    tmp_path: Path,
) -> None:
    module = _load_plugin()
    executed = False

    async def create_subprocess_shell(_command: str, **_kwargs):
        nonlocal executed
        executed = True
        return "process"

    shell_module = types.SimpleNamespace()
    shell_module.asyncio = types.SimpleNamespace(create_subprocess_shell=create_subprocess_shell)

    async def execute_shell_command(command: str):
        return await shell_module.asyncio.create_subprocess_shell(command)

    def data_client_factory(_config: MemberRuntimeConfig):
        raise RuntimeError("agentidentitydata unavailable")

    shell_module.execute_shell_command = execute_shell_command

    module.install_legacy_shell_credential_hook(
        shell_module,
        config_loader=lambda: _runtime_config(tmp_path),
        data_client_factory=data_client_factory,
    )

    result = await shell_module.execute_shell_command("echo $GITHUB_TOKEN")

    assert not executed
    assert result["content"].startswith("Error: Credential resolution failed")
    assert "agentidentitydata unavailable" in result["content"]
    assert module.current_credential_env_overlay() == {}
