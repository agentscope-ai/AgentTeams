import asyncio
import json
import logging
import os
from pathlib import Path
import sys
import tarfile
import types

import pytest

from qwenpaw_worker.config import WorkerConfig
from qwenpaw_worker.update import (
    MemberRuntimeConfig,
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


class _FakeQwenPawApi:
    def __init__(self):
        self.channels = {
            "agentteams_matrix": {},
            "dingtalk": {},
        }
        self.acls = {"agentteams_matrix": {"whitelist": {}, "blacklist": {}, "pending": []}}
        self.mcp = {}
        self.mcp_events = []
        self.channel_events = []
        self.model_events = []
        self.active_model = None
        self.enabled_skills = []
        self.skill_events = []

    def get_channel(self, channel):
        return dict(self.channels.get(channel, {}))

    def put_channel(self, channel, desired, *, secret_fields=()):
        self.channel_events.append(channel)
        current = self.get_channel(channel)
        payload = dict(desired)
        for field in secret_fields:
            if not payload.get(field) and current.get(field):
                payload[field] = current[field]
        self.channels[channel] = payload
        return dict(payload)

    def reconcile_acl(self, channel, whitelist, blacklist):
        payload = {
            "whitelist": {item: {} for item in whitelist},
            "blacklist": {item: {} for item in blacklist},
            "pending": [],
        }
        self.acls[channel] = payload
        return payload

    def list_mcp(self):
        return list(self.mcp.values())

    def create_mcp(self, key, payload):
        self.mcp_events.append(("create", key))
        self.mcp[key] = {"key": key, **payload}
        return self.mcp[key]

    def update_mcp(self, key, payload):
        self.mcp_events.append(("update", key))
        self.mcp[key] = {"key": key, **payload}
        return self.mcp[key]

    def delete_mcp(self, key):
        self.mcp_events.append(("delete", key))
        self.mcp.pop(key, None)

    def configure_active_model(self, provider_id, model, **kwargs):
        self.model_events.append((provider_id, model))
        self.active_model = {"provider_id": provider_id, "model": model, **kwargs}
        return {"active_llm": self.active_model}

    def refresh_and_enable_skills(self, skill_names):
        self.enabled_skills = list(skill_names)
        self.skill_events.append(list(skill_names))
        return {"results": {name: {"success": True} for name in self.enabled_skills}}


def _runtime_updater(*args, **kwargs):
    kwargs.setdefault("api_client", _FakeQwenPawApi())
    return RuntimeUpdater(*args, **kwargs)


def test_runtime_updater_syncs_and_enables_assigned_skills(tmp_path: Path) -> None:
    config = _config(tmp_path)
    synced = []

    def sync_skills(skill_names):
        synced.extend(skill_names)
        for name in skill_names:
            skill_dir = config.default_workspace_dir / "skills" / name
            skill_dir.mkdir(parents=True, exist_ok=True)
            (skill_dir / "SKILL.md").write_text(f"# {name}\n", encoding="utf-8")

    updater = _runtime_updater(
        config=config,
        package_manager=_NoopPackageManager(),
        skill_sync=sync_skills,
    )
    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "1"},
                "member": {"runtime": "qwenpaw"},
                "desired": {"skills": ["competition-skill"]},
            },
        ),
    )

    assert synced == ["competition-skill"]
    assert (config.default_workspace_dir / "skills" / "competition-skill" / "SKILL.md").is_file()
    assert updater.api_client.enabled_skills == ["competition-skill"]


def test_runtime_updater_treats_managed_skill_order_as_equivalent(tmp_path: Path) -> None:
    config = _config(tmp_path)
    synced: list[list[str]] = []

    def sync_skills(skill_names: list[str]) -> None:
        synced.append(list(skill_names))
        for name in skill_names:
            skill_dir = config.default_workspace_dir / "skills" / name
            skill_dir.mkdir(parents=True, exist_ok=True)
            (skill_dir / "SKILL.md").write_text(f"# {name}\n", encoding="utf-8")

    updater = _runtime_updater(
        config=config,
        package_manager=_NoopPackageManager(),
        skill_sync=sync_skills,
    )

    def runtime_config(skills: list[str]) -> MemberRuntimeConfig:
        return MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "-".join(skills)},
                "member": {"runtime": "qwenpaw"},
                "desired": {"skills": skills},
            },
        )

    updater.apply_once(runtime_config=runtime_config(["beta", "alpha"]))
    updater.apply_once(runtime_config=runtime_config(["alpha", "beta"]))

    assert synced == [["alpha", "beta"]]
    assert updater.api_client.skill_events == [["alpha", "beta"]]


def test_runtime_updater_reconciles_model_mcp_matrix_channel_and_acl_via_api(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("AGENTTEAMS_MATRIX_URL", "http://matrix.example.com")
    monkeypatch.setenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "matrix-token")
    monkeypatch.setenv("AGENTTEAMS_WORKER_GATEWAY_KEY", "gateway-secret")
    updater = _runtime_updater(
        config=_config(tmp_path),
        package_manager=_NoopPackageManager(),
    )

    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=updater.config.runtime_config_path,
            raw={
                "metadata": {"generation": "1"},
                "team": {"teamRoomId": "!team:matrix.local"},
                "member": {
                    "runtime": "qwenpaw",
                    "matrixUserId": "@worker-a:matrix.local",
                },
                "credentials": {
                    "matrixTokenEnv": "AGENTTEAMS_WORKER_MATRIX_TOKEN",
                    "gatewayKeyEnv": "AGENTTEAMS_WORKER_GATEWAY_KEY",
                },
                "desired": {
                    "model": {
                        "providerId": "agentteams-gateway",
                        "model": "qwen-plus",
                        "gatewayUrl": "https://gateway.example.com",
                    },
                    "mcpServers": [
                        {"name": "docs", "url": "https://gateway.example.com/mcp"},
                    ],
                    "channelPolicy": {
                        "groupAllowExtra": ["@human:matrix.local"],
                    },
                },
            },
        ),
    )

    api = updater.api_client
    assert api.active_model["provider_id"] == "agentteams-gateway"
    assert api.mcp["docs"]["transport"] == "streamable_http"
    assert api.channels["agentteams_matrix"]["access_token"] == "matrix-token"
    assert api.channels["agentteams_matrix"]["groups"]["!team:matrix.local"]["requireMention"] is True
    assert "@human:matrix.local" in api.acls["agentteams_matrix"]["whitelist"]
    assert not (updater.config.default_workspace_dir / "agent.json").exists()
    assert not (updater.config.default_workspace_dir / "config" / "mcporter.json").exists()
    assert not (updater.config.default_workspace_dir / "access_control.json").exists()


def test_runtime_updater_maps_dingtalk_visibility_and_preserves_empty_secret(
    tmp_path: Path,
) -> None:
    updater = _runtime_updater(
        config=_config(tmp_path),
        package_manager=_NoopPackageManager(),
    )
    updater.api_client.channels["dingtalk"] = {
        "enabled": True,
        "client_secret": "existing-secret",
    }

    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=updater.config.runtime_config_path,
            raw={
                "metadata": {"generation": "1"},
                "member": {"runtime": "qwenpaw"},
                "desired": {
                    "channels": {
                        "dingtalk": {
                            "enabled": True,
                            "client_id": "client-id",
                            "client_secret": "",
                            "robot_code": "robot",
                            "filter_thinking": True,
                            "filter_tool_messages": False,
                        },
                    },
                },
            },
        ),
    )
    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=updater.config.runtime_config_path,
            raw={
                "metadata": {"generation": "1"},
                "team": {"members": [{"name": "new-member", "role": "coordinator"}]},
                "member": {"runtime": "qwenpaw"},
                "desired": {
                    "channels": {
                        "dingtalk": {
                            "enabled": True,
                            "client_id": "client-id",
                            "client_secret": "",
                            "robot_code": "robot",
                            "filter_thinking": True,
                            "filter_tool_messages": False,
                        },
                    },
                },
            },
        ),
    )

    actual = updater.api_client.channels["dingtalk"]
    assert actual["client_secret"] == "existing-secret"
    assert actual["show_thinking"] is False
    assert actual["show_tool_calls"] is True
    assert actual["show_tool_results"] is True
    assert updater.api_client.channel_events == ["dingtalk"]


def test_runtime_updater_ignores_unused_dingtalk_fields(
    tmp_path: Path,
) -> None:
    updater = _runtime_updater(config=_config(tmp_path), package_manager=_NoopPackageManager())

    def runtime_config(ignored: str) -> MemberRuntimeConfig:
        return MemberRuntimeConfig(
            path=updater.config.runtime_config_path,
            raw={
                "metadata": {"generation": ignored},
                "member": {"runtime": "qwenpaw"},
                "desired": {
                    "channels": {
                        "dingtalk": {
                            "enabled": True,
                            "client_id": "client-id",
                            "client_secret": "secret",
                            "robot_code": "robot",
                            "unused": ignored,
                        },
                    },
                },
            },
        )

    updater.apply_once(runtime_config=runtime_config("one"))
    updater.apply_once(runtime_config=runtime_config("two"))

    assert updater.api_client.channel_events == ["dingtalk"]


def test_runtime_updater_restores_matrix_policy_after_channel_update(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("AGENTTEAMS_MATRIX_URL", "http://matrix.example.com")
    monkeypatch.setenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "matrix-token")
    monkeypatch.setenv("AGENTTEAMS_MATRIX_DOMAIN", "matrix.local")
    updater = _runtime_updater(config=_config(tmp_path), package_manager=_NoopPackageManager())

    def runtime_config(room_id: str) -> MemberRuntimeConfig:
        return MemberRuntimeConfig(
            path=updater.config.runtime_config_path,
            raw={
                "metadata": {"generation": room_id},
                "team": {"teamRoomId": room_id},
                "member": {
                    "runtime": "qwenpaw",
                    "matrixUserId": "@worker:matrix.local",
                },
                "desired": {
                    "channelPolicy": {
                        "groupAllowExtra": ["@human:matrix.local"],
                        "dmAllowExtra": ["@human:matrix.local"],
                    },
                },
            },
        )

    updater.apply_once(runtime_config=runtime_config("!room-one:matrix.local"))
    updater.apply_once(runtime_config=runtime_config("!room-two:matrix.local"))

    matrix = updater.api_client.channels["agentteams_matrix"]
    assert matrix["access_control_group"] is True
    assert matrix["access_control_dm"] is True
    assert updater.api_client.channel_events == [
        "agentteams_matrix",
        "agentteams_matrix",
        "agentteams_matrix",
        "agentteams_matrix",
    ]


def test_runtime_updater_preserves_unmanaged_mcp_clients(tmp_path: Path) -> None:
    updater = _runtime_updater(config=_config(tmp_path), package_manager=_NoopPackageManager())
    updater.api_client.mcp["third-party"] = {"key": "third-party", "name": "third-party"}

    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=updater.config.runtime_config_path,
            raw={"metadata": {"generation": "1"}, "member": {"runtime": "qwenpaw"}},
        ),
    )

    assert "third-party" in updater.api_client.mcp


def test_runtime_updater_does_not_reapply_mcp_for_team_member_change(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("AGENTTEAMS_WORKER_GATEWAY_KEY", "gateway-secret")
    monkeypatch.setenv("AGENTTEAMS_MATRIX_URL", "http://matrix.example.com")
    monkeypatch.setenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "matrix-token")
    updater = _runtime_updater(config=_config(tmp_path), package_manager=_NoopPackageManager())

    def runtime_config(generation: str, members: list[dict[str, str]]) -> MemberRuntimeConfig:
        return MemberRuntimeConfig(
            path=updater.config.runtime_config_path,
            raw={
                "metadata": {"generation": generation},
                "team": {"teamRoomId": "!team:matrix.local", "members": members},
                "member": {
                    "runtime": "qwenpaw",
                    "matrixUserId": "@worker:matrix.local",
                },
                "credentials": {
                    "matrixTokenEnv": "AGENTTEAMS_WORKER_MATRIX_TOKEN",
                    "gatewayKeyEnv": "AGENTTEAMS_WORKER_GATEWAY_KEY",
                },
                "desired": {
                    "model": {
                        "providerId": "agentteams-gateway",
                        "model": "qwen-plus",
                        "gatewayUrl": "https://gateway.example.com",
                    },
                    "mcpServers": [
                        {"name": "docs", "url": "https://gateway.example.com/mcp"},
                    ],
                },
            },
        )

    first = runtime_config(
        "1",
        [
            {"name": "alice", "role": "coordinator", "matrixUserId": "@alice:matrix.local"},
            {"name": "bob", "role": "coordinator", "matrixUserId": "@bob:matrix.local"},
        ],
    )
    reordered = runtime_config(
        "1",
        [
            {"name": "bob", "role": "coordinator", "matrixUserId": "@bob:matrix.local"},
            {"name": "alice", "role": "coordinator", "matrixUserId": "@alice:matrix.local"},
        ],
    )
    changed = runtime_config(
        "1",
        [
            {"name": "bob", "role": "coordinator", "matrixUserId": "@bob:matrix.local"},
            {"name": "alice", "role": "coordinator", "matrixUserId": "@alice:matrix.local"},
            {"name": "carol", "role": "coordinator", "matrixUserId": "@carol:matrix.local"},
        ],
    )

    first_result = updater.apply_once(runtime_config=first, reapply_adapter=False)
    reordered_result = updater.apply_once(runtime_config=reordered, reapply_adapter=False)
    changed_result = updater.apply_once(runtime_config=changed, reapply_adapter=False)

    assert first_result.changed is True
    assert reordered_result.changed is True
    assert changed_result.changed is True
    assert updater.api_client.mcp_events == [("create", "docs")]
    assert updater.api_client.model_events == [("agentteams-gateway", "qwen-plus")]
    assert updater.api_client.channel_events == [
        "agentteams_matrix",
        "agentteams_matrix",
    ]


def test_runtime_updater_reapplies_direct_mcp_when_effective_payload_changes(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("AGENTTEAMS_WORKER_GATEWAY_KEY", "gateway-secret")
    updater = _runtime_updater(config=_config(tmp_path), package_manager=_NoopPackageManager())

    def runtime_config(url: str) -> MemberRuntimeConfig:
        return MemberRuntimeConfig(
            path=updater.config.runtime_config_path,
            raw={
                "metadata": {"generation": url},
                "member": {"runtime": "qwenpaw"},
                "desired": {"mcpServers": [{"name": "docs", "url": url}]},
            },
        )

    first = runtime_config("https://one.example/mcp")
    second = runtime_config("https://two.example/mcp")
    updater.apply_once(runtime_config=first, reapply_adapter=False)
    updater.apply_once(runtime_config=second, reapply_adapter=False)
    updater.apply_once(runtime_config=second, force=True, reapply_adapter=False)
    updater.api_client.mcp.pop("docs")
    updater.apply_once(runtime_config=second, force=True, reapply_adapter=False)
    updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=updater.config.runtime_config_path,
            raw={
                "metadata": {"generation": "removed"},
                "member": {"runtime": "qwenpaw"},
                "desired": {"mcpServers": []},
            },
        ),
        reapply_adapter=False,
    )

    assert updater.api_client.mcp_events == [
        ("create", "docs"),
        ("update", "docs"),
        ("update", "docs"),
        ("create", "docs"),
        ("delete", "docs"),
    ]
    assert "docs" not in updater.api_client.mcp


def test_runtime_updater_reconciles_only_changed_direct_mcp_client(
    tmp_path: Path,
) -> None:
    updater = _runtime_updater(config=_config(tmp_path), package_manager=_NoopPackageManager())

    def runtime_config(a_url: str, a_transport: str = "http") -> MemberRuntimeConfig:
        return MemberRuntimeConfig(
            path=updater.config.runtime_config_path,
            raw={
                "metadata": {"generation": a_url + a_transport},
                "member": {"runtime": "qwenpaw"},
                "desired": {
                    "mcpServers": {
                        "a": {"url": a_url, "transport": a_transport},
                        "b": {"url": "https://b.example/mcp"},
                    },
                },
            },
        )

    updater.apply_once(runtime_config=runtime_config("https://a-v1.example/mcp"))
    updater.apply_once(runtime_config=runtime_config("https://a-v2.example/mcp"))
    updater.apply_once(
        runtime_config=runtime_config("https://a-v2.example/mcp", "streamable_http")
    )

    assert updater.api_client.mcp_events == [
        ("create", "a"),
        ("create", "b"),
        ("update", "a"),
    ]


def test_runtime_updater_reapplies_package_mcp_only_when_package_identity_changes(
    tmp_path: Path,
) -> None:
    class PackageManager:
        def __init__(self) -> None:
            self.client_reads: list[str] = []

        def apply(self, runtime_config: MemberRuntimeConfig) -> Path:
            version = runtime_config.agent_package_identity[2]
            return tmp_path / f"package-{version}"

        def package_mcp_clients(self, package_dir: Path) -> dict[str, dict[str, str]]:
            self.client_reads.append(package_dir.name)
            return {
                "package-docs": {
                    "name": "package-docs",
                    "transport": "streamable_http",
                    "url": f"https://{package_dir.name}.example/mcp",
                },
            }

        def package_skill_names(self, _package_dir: Path) -> list[str]:
            return []

    package_manager = PackageManager()
    updater = _runtime_updater(config=_config(tmp_path), package_manager=package_manager)

    def runtime_config(version: str, generation: str) -> MemberRuntimeConfig:
        return MemberRuntimeConfig(
            path=updater.config.runtime_config_path,
            raw={
                "metadata": {"generation": generation},
                "team": {"members": [{"name": generation, "role": "coordinator"}]},
                "member": {"runtime": "qwenpaw"},
                "desired": {
                    "agentPackage": {
                        "ref": f"file:///tmp/package-{version}.tar.gz",
                        "name": "demo",
                        "version": version,
                        "digest": f"sha256:{version}",
                    },
                },
            },
        )

    updater.apply_once(runtime_config=runtime_config("1", "1"), reapply_adapter=False)
    updater.apply_once(runtime_config=runtime_config("1", "2"), reapply_adapter=False)
    package_v2 = runtime_config("2", "3")
    updater.apply_once(runtime_config=package_v2, reapply_adapter=False)
    updater.apply_once(runtime_config=package_v2, force=True, reapply_adapter=False)

    assert package_manager.client_reads == ["package-1", "package-2", "package-2"]
    assert updater.api_client.mcp_events == [
        ("create", "package-docs"),
        ("update", "package-docs"),
        ("update", "package-docs"),
    ]


def test_runtime_updater_does_not_reapply_unchanged_package_mcp_payload(
    tmp_path: Path,
) -> None:
    class PackageManager:
        def apply(self, runtime_config: MemberRuntimeConfig) -> Path:
            return tmp_path / f"package-{runtime_config.agent_package_identity[2]}"

        def package_mcp_clients(self, _package_dir: Path) -> dict[str, dict[str, object]]:
            return {
                "docs": {
                    "name": "docs",
                    "enabled": True,
                    "transport": "streamable_http",
                    "url": "https://package.example/mcp",
                },
            }

        def package_skill_names(self, _package_dir: Path) -> list[str]:
            return []

    updater = _runtime_updater(config=_config(tmp_path), package_manager=PackageManager())

    def runtime_config(version: str) -> MemberRuntimeConfig:
        return MemberRuntimeConfig(
            path=updater.config.runtime_config_path,
            raw={
                "metadata": {"generation": version},
                "member": {"runtime": "qwenpaw"},
                "desired": {
                    "agentPackage": {
                        "ref": f"file:///tmp/package-{version}.tar.gz",
                        "name": "demo",
                        "version": version,
                        "digest": f"sha256:{version}",
                    },
                },
            },
        )

    updater.apply_once(runtime_config=runtime_config("1"))
    updater.apply_once(runtime_config=runtime_config("2"))

    assert updater.api_client.mcp_events == [("create", "docs")]


def test_runtime_updater_preserves_package_precedence_for_direct_mcp_collision(
    tmp_path: Path,
) -> None:
    class PackageManager:
        def apply(self, _runtime_config: MemberRuntimeConfig) -> Path:
            return tmp_path / "package"

        def package_mcp_clients(self, _package_dir: Path) -> dict[str, dict[str, object]]:
            return {
                "docs": {
                    "name": "docs",
                    "transport": "streamable_http",
                    "url": "https://package.example/mcp",
                },
            }

        def package_skill_names(self, _package_dir: Path) -> list[str]:
            return []

    updater = _runtime_updater(config=_config(tmp_path), package_manager=PackageManager())

    def runtime_config(url: str) -> MemberRuntimeConfig:
        return MemberRuntimeConfig(
            path=updater.config.runtime_config_path,
            raw={
                "metadata": {"generation": url},
                "member": {"runtime": "qwenpaw"},
                "desired": {
                    "agentPackage": {
                        "ref": "file:///tmp/package.tar.gz",
                        "name": "demo",
                        "version": "1",
                        "digest": "sha256:1",
                    },
                    "mcpServers": {"docs": {"url": url}},
                },
            },
        )

    updater.apply_once(runtime_config=runtime_config("https://direct-v1.example/mcp"))
    updater.apply_once(runtime_config=runtime_config("https://direct-v2.example/mcp"))

    assert updater.api_client.mcp_events == [("create", "docs")]
    assert updater.api_client.mcp["docs"]["url"] == "https://package.example/mcp"


def _agent_package(
    tmp_path: Path,
    version: str,
    *,
    include_teams: bool = False,
    skill_name: str = "",
) -> Path:
    source_dir = tmp_path / f"package-src-{version}"
    config_dir = source_dir / "config"
    config_dir.mkdir(parents=True)
    (source_dir / "manifest.json").write_text('{"version":"1.0"}\n', encoding="utf-8")
    (config_dir / "AGENTS.md").write_text(f"agent package {version}\n", encoding="utf-8")
    if include_teams:
        (config_dir / "TEAMS.md").write_text(f"package teams {version}\n", encoding="utf-8")
    if skill_name:
        skill_dir = source_dir / "skills" / skill_name
        skill_dir.mkdir(parents=True)
        (skill_dir / "SKILL.md").write_text("package skill\n", encoding="utf-8")
    package_path = tmp_path / f"agent-package-{version}.tar.gz"
    with tarfile.open(package_path, "w:gz") as archive:
        archive.add(source_dir, arcname=".")
    return package_path


def test_runtime_updater_preserves_inline_prompt_on_unrelated_member_change(
    tmp_path: Path,
) -> None:
    config = _config(tmp_path)
    package = _agent_package(tmp_path, "1")
    updater = _runtime_updater(config=config)

    def runtime_config(member_name: str) -> MemberRuntimeConfig:
        return MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": member_name},
                "team": {"members": [{"name": member_name, "role": "worker"}]},
                "member": {"runtime": "qwenpaw"},
                "desired": {
                    "agentPackage": {
                        "ref": f"file://{package}",
                        "name": "demo",
                        "version": "1",
                        "digest": "sha256:1",
                    },
                    "inlineConfig": {"agents": "inline agents"},
                },
            },
        )

    updater.apply_once(runtime_config=runtime_config("alice"))
    updater.apply_once(runtime_config=runtime_config("bob"))

    assert (config.default_workspace_dir / "AGENTS.md").read_text(
        encoding="utf-8"
    ) == "inline agents\n"


def test_runtime_updater_preserves_managed_skill_over_package_on_member_change(
    tmp_path: Path,
) -> None:
    config = _config(tmp_path)
    package = _agent_package(tmp_path, "1", skill_name="shared-skill")

    def sync_skills(skill_names: list[str]) -> None:
        for name in skill_names:
            skill_dir = config.default_workspace_dir / "skills" / name
            skill_dir.mkdir(parents=True, exist_ok=True)
            (skill_dir / "SKILL.md").write_text("managed skill\n", encoding="utf-8")

    updater = _runtime_updater(config=config, skill_sync=sync_skills)

    def runtime_config(member_name: str) -> MemberRuntimeConfig:
        return MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": member_name},
                "team": {"members": [{"name": member_name, "role": "worker"}]},
                "member": {"runtime": "qwenpaw"},
                "desired": {
                    "agentPackage": {
                        "ref": f"file://{package}",
                        "name": "demo",
                        "version": "1",
                        "digest": "sha256:1",
                    },
                    "skills": ["shared-skill"],
                },
            },
        )

    updater.apply_once(runtime_config=runtime_config("alice"))
    updater.apply_once(runtime_config=runtime_config("bob"))

    skill_path = config.default_workspace_dir / "skills" / "shared-skill" / "SKILL.md"
    assert skill_path.read_text(encoding="utf-8") == "managed skill\n"


def test_runtime_updater_skips_package_skill_refresh_when_skill_content_is_unchanged(
    tmp_path: Path,
) -> None:
    config = _config(tmp_path)
    package_v1 = _agent_package(tmp_path, "1", skill_name="shared-skill")
    package_v2 = _agent_package(tmp_path, "2", skill_name="shared-skill")
    updater = _runtime_updater(config=config)

    def runtime_config(package: Path, version: str) -> MemberRuntimeConfig:
        return MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": version},
                "member": {"runtime": "qwenpaw"},
                "desired": {
                    "agentPackage": {
                        "ref": f"file://{package}",
                        "name": "demo",
                        "version": version,
                        "digest": f"sha256:{version}",
                    },
                },
            },
        )

    updater.apply_once(runtime_config=runtime_config(package_v1, "1"))
    updater.apply_once(runtime_config=runtime_config(package_v2, "2"))

    assert updater.api_client.skill_events == [["shared-skill"]]


def test_runtime_updater_applies_changed_config_and_reapplies_adapter(tmp_path: Path) -> None:
    config = _config(tmp_path)
    applied: list[str] = []
    adapter_calls: list[str] = []

    class FakePackageManager:
        def apply(self, runtime_config: MemberRuntimeConfig):
            applied.append(runtime_config.generation)
            return tmp_path / "packages" / runtime_config.generation

    updater = _runtime_updater(
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

    assert applied == ["1"]
    assert adapter_calls == ["adapter", "adapter"]
    assert updater.current_config is not None
    assert updater.current_config.generation == "2"


def test_runtime_updater_propagates_force_to_adapter_reconcile(tmp_path: Path) -> None:
    config = _config(tmp_path)
    adapter_calls: list[str] = []
    updater = _runtime_updater(
        config=config,
        adapter_apply=lambda: adapter_calls.append("normal"),
        adapter_force_apply=lambda: adapter_calls.append("force"),
        package_manager=_NoopPackageManager(),
    )
    runtime_config = MemberRuntimeConfig(
        path=config.runtime_config_path,
        raw={"metadata": {"generation": "1"}, "member": {"runtime": "qwenpaw"}},
    )

    updater.apply_once(runtime_config=runtime_config)
    updater.apply_once(runtime_config=runtime_config, force=True)

    assert adapter_calls == ["normal", "force"]


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
    updater = _runtime_updater(
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


def test_runtime_updater_reconciles_storage_only_change(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    reconciled: list[tuple[str, str]] = []
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "standalone")
    updater = _runtime_updater(
        config=config,
        package_manager=_NoopPackageManager(),
        runtime_reconcile=lambda runtime: reconciled.append(
            (runtime.storage["sharedPrefix"], os.environ["AGENTTEAMS_AGENT_ROLE"]),
        ),
    )
    updater.current_config = MemberRuntimeConfig(
        path=config.runtime_config_path,
        raw={
            "metadata": {"generation": "2"},
            "member": {"runtime": "qwenpaw"},
            "storage": {"sharedPrefix": "shared"},
        },
    )

    result = updater.apply_once(
        runtime_config=MemberRuntimeConfig(
            path=config.runtime_config_path,
            raw={
                "metadata": {"generation": "2"},
                "member": {"runtime": "qwenpaw", "role": "team_leader"},
                "storage": {"sharedPrefix": "teams/demo-team/shared"},
            },
        ),
        reapply_adapter=False,
    )

    assert result.changed is True
    assert reconciled == [("teams/demo-team/shared", "team_leader")]


def test_runtime_updater_logs_safe_apply_summary_without_sensitive_values(
    tmp_path: Path,
    caplog: pytest.LogCaptureFixture,
) -> None:
    config = _config(tmp_path)
    adapter_calls: list[str] = []
    caplog.set_level(logging.INFO, logger="qwenpaw_worker.update")
    updater = _runtime_updater(
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


def _legacy_test_runtime_updater_does_not_reapply_adapter_for_mcp_only_change(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    monkeypatch.setenv("AGENTTEAMS_WORKER_GATEWAY_KEY", "gateway-secret")
    adapter_calls: list[str] = []
    updater = _runtime_updater(
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
    updater = _runtime_updater(
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
    updater = _runtime_updater(
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
    updater = _runtime_updater(
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
    updater = _runtime_updater(config=config, package_manager=_NoopPackageManager())

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
    updater = _runtime_updater(config=config, package_manager=_NoopPackageManager())

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

    updater = _runtime_updater(
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

    updater = _runtime_updater(
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


def _legacy_test_runtime_updater_applies_desired_model_to_qwenpaw_agent_config(
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

    updater = _runtime_updater(config=config, package_manager=_NoopPackageManager())
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


def _legacy_test_runtime_updater_configures_openai_compatible_provider_from_runtime_config(
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

    updater = _runtime_updater(config=config, package_manager=_NoopPackageManager())
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


def _legacy_test_runtime_updater_writes_mcporter_config_from_desired_mcp_servers(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    config = _config(tmp_path)
    monkeypatch.setenv("AGENTTEAMS_WORKER_GATEWAY_KEY", "gateway-secret")
    updater = _runtime_updater(config=config, package_manager=_NoopPackageManager())

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


def _legacy_test_runtime_updater_writes_empty_mcporter_config_when_mcp_servers_are_omitted(
    tmp_path: Path,
) -> None:
    config = _config(tmp_path)
    updater = _runtime_updater(config=config, package_manager=_NoopPackageManager())

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


def _legacy_test_runtime_updater_configures_matrix_channel_in_agent_config(
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

    updater = _runtime_updater(config=config, package_manager=_NoopPackageManager())
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


def _legacy_test_runtime_updater_configures_dingtalk_channel_in_agent_config(
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

    updater = _runtime_updater(config=config, package_manager=_NoopPackageManager())
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


def _legacy_test_runtime_updater_uses_provided_stream_dingtalk_card_config(
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
    updater = _runtime_updater(
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

    updater = _runtime_updater(config=config, package_manager=_NoopPackageManager())
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


def _legacy_test_runtime_updater_preserves_card_config_when_dingtalk_streaming_disabled(
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

    updater = _runtime_updater(config=config, package_manager=_NoopPackageManager())
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


def _legacy_test_runtime_updater_disables_dingtalk_channel_from_runtime_config(
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

    updater = _runtime_updater(config=config, package_manager=_NoopPackageManager())
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

    updater = _runtime_updater(config=config, package_manager=_NoopPackageManager())
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


def _legacy_test_runtime_updater_applies_copaw_style_matrix_channel_policy(
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

    updater = _runtime_updater(config=config, package_manager=_NoopPackageManager())
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


def _legacy_test_runtime_updater_self_matrix_id_still_respects_deny_policy(
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

    updater = _runtime_updater(config=config, package_manager=_NoopPackageManager())
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


def _legacy_test_runtime_updater_applies_team_roster_matrix_defaults_for_team_worker(
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

    updater = _runtime_updater(config=config, package_manager=_NoopPackageManager())
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


def _legacy_test_runtime_updater_system_admin_in_allowlist_when_team_admin_differs(
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

    updater = _runtime_updater(config=config, package_manager=_NoopPackageManager())
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


def _legacy_test_runtime_updater_applies_copaw_style_matrix_defaults_for_team_worker(
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

    updater = _runtime_updater(config=config, package_manager=_NoopPackageManager())
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


def _legacy_test_runtime_updater_applies_copaw_style_matrix_defaults_for_standalone_worker(
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

    updater = _runtime_updater(config=config, package_manager=_NoopPackageManager())
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

    updater = _runtime_updater(config=config, package_manager=_NoopPackageManager())
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
    updater = _runtime_updater(config=config)
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
    updater = _runtime_updater(config=config)
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

    updater = _runtime_updater(config=config, runtime_config_pull=pull_runtime_config)

    loaded = updater.load()

    assert pulls == ["pull"]
    assert loaded.generation == "7"


class _NoopPackageManager:
    def apply(self, _runtime_config: MemberRuntimeConfig):
        return None
