from pathlib import Path

import pytest

from qwenpaw_worker.update import REGION_ID_ENV_NAMES, MemberRuntimeConfig


@pytest.fixture(autouse=True)
def clear_region_env(monkeypatch: pytest.MonkeyPatch) -> None:
    for name in REGION_ID_ENV_NAMES:
        monkeypatch.delenv(name, raising=False)


def test_load_runtime_config_reads_team_member_package_and_sanitizer(tmp_path: Path) -> None:
    path = tmp_path / "runtime.yaml"
    path.write_text(
        """
apiVersion: agentteams.io/v1beta1
kind: MemberRuntimeConfig
metadata:
  generation: 7
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
    - name: human-coord
      role: coordinator
      matrixUserId: "@human:matrix.local"
member:
  name: worker-a
  runtimeName: worker-a
  role: worker
  runtime: qwenpaw
  matrixUserId: "@worker-a:matrix.local"
desired:
  model:
    provider: hiclaw-gateway
    model: qwen-max
  mcpServers:
    taskflow:
      command: python3
      args: ["server.py"]
  channelPolicy:
    allowedChannels:
      - matrix
  agentPackage:
    ref: file:///tmp/dev-worker.tar.gz
    name: dev-worker
    version: 1.2.0
    digest: sha256:abc
  outputSanitize:
    keywords:
      - internal-token
    envRefs:
      - AGENTTEAMS_WORKER_MATRIX_TOKEN
storage:
  memberPrefix: agents/worker-a
credentials:
  matrixTokenEnv: AGENTTEAMS_WORKER_MATRIX_TOKEN
  gatewayKeyEnv: AGENTTEAMS_WORKER_GATEWAY_KEY
""",
        encoding="utf-8",
    )

    config = MemberRuntimeConfig.load(path)

    assert config.generation == "7"
    assert config.team_name == "demo-team"
    assert config.member_name == "worker-a"
    assert config.member_role == "worker"
    assert config.agent_package_identity == (
        "file:///tmp/dev-worker.tar.gz",
        "dev-worker",
        "1.2.0",
        "sha256:abc",
    )
    assert config.model["model"] == "qwen-max"
    assert config.mcp_servers["taskflow"]["command"] == "python3"
    assert config.channel_policy["allowedChannels"] == ["matrix"]
    assert config.team_members == [
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
        {
            "name": "human-coord",
            "role": "coordinator",
            "matrixUserId": "@human:matrix.local",
        },
    ]
    assert config.output_sanitize_keywords == ["internal-token"]
    assert config.output_sanitize_env_refs == [
        "AGENTTEAMS_WORKER_MATRIX_TOKEN",
        "AGENTTEAMS_WORKER_GATEWAY_KEY",
    ]


def test_load_runtime_config_reads_desired_dingtalk_channel(tmp_path: Path) -> None:
    path = tmp_path / "runtime.yaml"
    path.write_text(
        """
kind: MemberRuntimeConfig
member:
  runtime: qwenpaw
desired:
  channels:
    dingtalk:
      enabled: true
      client_id: demo-client-id
      client_secret: test-client-secret
      robot_code: demo-robot-code
      filter_thinking: false
      filter_tool_messages: true
      streaming_enabled: true
      message_type: card
      card_template_id: card-template-1
      card_template_key: content
      card_auto_layout: false
""",
        encoding="utf-8",
    )

    config = MemberRuntimeConfig.load(path)

    assert config.channels["dingtalk"] == {
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
    assert config.dingtalk_channel == config.channels["dingtalk"]


def test_runtime_config_rejects_non_qwenpaw_runtime(tmp_path: Path) -> None:
    path = tmp_path / "runtime.yaml"
    path.write_text(
        """
kind: MemberRuntimeConfig
member:
  name: worker-a
  runtime: copaw
""",
        encoding="utf-8",
    )

    with pytest.raises(ValueError, match="runtime must be qwenpaw"):
        MemberRuntimeConfig.load(path)


def test_runtime_config_reads_agent_identity_and_credential_bindings(tmp_path: Path) -> None:
    path = tmp_path / "runtime.yaml"
    path.write_text(
        """
kind: MemberRuntimeConfig
member:
  name: worker-a
  runtime: qwenpaw
desired:
  agentIdentity:
    workloadIdentityName: wi-worker-a
  credentialBindings:
    - credentialRef:
        tokenVaultName: default
        apiKeyCredentialProviderName: GITHUB_TOKEN
      toolWhitelist:
        - gh
        - ""
    - credentialRef:
        tokenVaultName: default
        apiKeyCredentialProviderName: ALIBABA_CLOUD_ACCESS_KEY_ID
agentIdentityData:
  endpoint: agentidentitydata.cn-beijing.aliyuncs.com
""",
        encoding="utf-8",
    )

    config = MemberRuntimeConfig.load(path)

    assert config.agent_identity == {"workloadIdentityName": "wi-worker-a"}
    assert config.workload_identity_name == "wi-worker-a"
    assert config.credential_bindings == [
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
            }
        },
    ]
    assert config.credential_binding_env_names == [
        "GITHUB_TOKEN",
        "ALIBABA_CLOUD_ACCESS_KEY_ID",
    ]
    assert config.credential_binding_env_provider_names == {
        "GITHUB_TOKEN": "GITHUB_TOKEN",
        "ALIBABA_CLOUD_ACCESS_KEY_ID": "ALIBABA_CLOUD_ACCESS_KEY_ID",
    }
    assert config.agent_identity_data == {
        "endpoint": "agentidentitydata.cn-beijing.aliyuncs.com"
    }
    assert config.agent_identity_data["endpoint"] == "agentidentitydata.cn-beijing.aliyuncs.com"
    assert config.agent_identity_data_endpoint == "agentidentitydata.cn-beijing.aliyuncs.com"


def test_runtime_config_prefers_agentidentitydata_endpoint_over_region_id_and_env(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("AGENTTEAMS_REGION", "cn-shanghai")
    config = MemberRuntimeConfig(
        path=tmp_path / "runtime.yaml",
        raw={
            "agentIdentityData": {
                "regionId": "cn-hangzhou",
                "endpoint": "agentidentitydata-vpc-pre.cn-hangzhou.aliyuncs.com",
            }
        },
    )

    assert config.agent_identity_data_region_id == "cn-hangzhou"
    assert config.agent_identity_data_endpoint == "agentidentitydata-vpc-pre.cn-hangzhou.aliyuncs.com"


def test_runtime_config_derives_agentidentitydata_endpoint_from_region_id(tmp_path: Path) -> None:
    config = MemberRuntimeConfig(
        path=tmp_path / "runtime.yaml",
        raw={"agentIdentityData": {"regionId": "cn-hangzhou"}},
    )

    assert config.agent_identity_data_region_id == "cn-hangzhou"
    assert config.agent_identity_data_endpoint == "agentidentitydata.cn-hangzhou.aliyuncs.com"


def test_runtime_config_derives_agentidentitydata_endpoint_from_region_env(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("AGENTTEAMS_REGION", "cn-hangzhou")
    config = MemberRuntimeConfig(
        path=tmp_path / "runtime.yaml",
        raw={},
    )

    assert config.agent_identity_data_region_id == "cn-hangzhou"
    assert config.agent_identity_data_endpoint == "agentidentitydata.cn-hangzhou.aliyuncs.com"


def test_runtime_config_derives_env_names_from_prefixed_credential_providers(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv(
        "AGENTTEAMS_CONTROLLER_URL",
        "http://controller.at-cn-4b84u92kf0f.vpc.agentteams.aliyuncs.com",
    )
    path = tmp_path / "runtime.yaml"
    path.write_text(
        """
kind: MemberRuntimeConfig
member:
  name: worker-a
  runtime: qwenpaw
desired:
  credentialBindings:
    - credentialRef:
        tokenVaultName: default
        apiKeyCredentialProviderName: at-cn-4b84u92kf0f-ALIBABA_CLOUD_ACCESS_KEY_ID
      toolWhitelist:
        - aliyun
    - credentialRef:
        tokenVaultName: default
        apiKeyCredentialProviderName: at-cn-4b84u92kf0f-ALIBABA_CLOUD_ACCESS_KEY_SECRET
      toolWhitelist:
        - aliyun
""",
        encoding="utf-8",
    )

    config = MemberRuntimeConfig.load(path)

    assert config.credential_binding_env_names == [
        "ALIBABA_CLOUD_ACCESS_KEY_ID",
        "ALIBABA_CLOUD_ACCESS_KEY_SECRET",
    ]
    assert config.credential_binding_env_provider_names == {
        "ALIBABA_CLOUD_ACCESS_KEY_ID": "at-cn-4b84u92kf0f-ALIBABA_CLOUD_ACCESS_KEY_ID",
        "ALIBABA_CLOUD_ACCESS_KEY_SECRET": "at-cn-4b84u92kf0f-ALIBABA_CLOUD_ACCESS_KEY_SECRET",
    }


def test_runtime_config_does_not_strip_other_instance_provider_prefix(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv(
        "AGENTTEAMS_CONTROLLER_URL",
        "http://controller.at-cn-4b84u92kf0f.vpc.agentteams.aliyuncs.com",
    )
    config = MemberRuntimeConfig(
        path=tmp_path / "runtime.yaml",
        raw={
            "desired": {
                "credentialBindings": [
                    {
                        "credentialRef": {
                            "tokenVaultName": "default",
                            "apiKeyCredentialProviderName": "other-instance-ALIBABA_CLOUD_ACCESS_KEY_ID",
                        },
                    },
                ],
            },
        },
    )

    assert config.credential_binding_env_names == []
    assert config.credential_binding_env_provider_names == {}


def test_runtime_config_keeps_credential_bindings_reference_only(tmp_path: Path) -> None:
    config = MemberRuntimeConfig(
        path=tmp_path / "runtime.yaml",
        raw={
            "desired": {
                "agentIdentity": {
                    "workloadIdentityName": "wi-worker-a",
                    "token": "should-not-survive",
                },
                "credentialBindings": [
                    {
                        "credentialRef": {
                            "tokenVaultName": "default",
                            "apiKeyCredentialProviderName": "GITHUB_TOKEN",
                            "apiKey": "real-secret",
                        },
                        "value": "another-secret",
                        "toolWhitelist": ["gh", "", " "],
                    },
                ],
            },
        },
    )

    assert config.agent_identity == {"workloadIdentityName": "wi-worker-a"}
    assert config.credential_bindings == [
        {
            "credentialRef": {
                "tokenVaultName": "default",
                "apiKeyCredentialProviderName": "GITHUB_TOKEN",
            },
            "toolWhitelist": ["gh"],
        }
    ]
    assert "real-secret" not in repr(config.credential_bindings)
    assert "another-secret" not in repr(config.credential_bindings)


def test_runtime_config_change_detection_includes_model_mcp_and_channel_policy(tmp_path: Path) -> None:
    first = MemberRuntimeConfig(
        path=tmp_path / "runtime.yaml",
        raw={
            "metadata": {"generation": "1"},
            "desired": {
                "model": {"provider": "hiclaw-gateway", "model": "qwen-max"},
                "mcpServers": {"taskflow": {"command": "python3"}},
                "channelPolicy": {"allowedChannels": ["matrix"]},
            },
        },
    )
    second = MemberRuntimeConfig(
        path=tmp_path / "runtime.yaml",
        raw={
            "metadata": {"generation": "1"},
            "desired": {
                "model": {"provider": "hiclaw-gateway", "model": "qwen-plus"},
                "mcpServers": {"taskflow": {"command": "python3"}},
                "channelPolicy": {"allowedChannels": ["matrix"]},
            },
        },
    )
    third = MemberRuntimeConfig(
        path=tmp_path / "runtime.yaml",
        raw={
            "metadata": {"generation": "1"},
            "desired": {
                "model": {"provider": "hiclaw-gateway", "model": "qwen-max"},
                "mcpServers": {"taskflow": {"command": "python3"}},
                "channelPolicy": {"allowedChannels": ["matrix"]},
            },
        },
    )

    assert second.changed_from(first)
    assert not third.changed_from(first)


def test_runtime_config_change_detection_includes_desired_channels(tmp_path: Path) -> None:
    first = MemberRuntimeConfig(
        path=tmp_path / "runtime.yaml",
        raw={
            "metadata": {"generation": "1"},
            "desired": {"channels": {"dingtalk": {"enabled": False}}},
        },
    )
    second = MemberRuntimeConfig(
        path=tmp_path / "runtime.yaml",
        raw={
            "metadata": {"generation": "1"},
            "desired": {"channels": {"dingtalk": {"enabled": True, "client_id": "demo-client-id"}}},
        },
    )

    assert second.changed_from(first)


def test_runtime_config_change_detection_includes_team_members(tmp_path: Path) -> None:
    first = MemberRuntimeConfig(
        path=tmp_path / "runtime.yaml",
        raw={
            "metadata": {"generation": "1"},
            "team": {
                "members": [
                    {
                        "name": "dev",
                        "runtimeName": "dev-runtime",
                        "role": "worker",
                        "matrixUserId": "@dev-runtime:matrix.local",
                    }
                ]
            },
        },
    )
    second = MemberRuntimeConfig(
        path=tmp_path / "runtime.yaml",
        raw={
            "metadata": {"generation": "1"},
            "team": {
                "members": [
                    {
                        "name": "dev",
                        "runtimeName": "dev-runtime",
                        "role": "worker",
                        "matrixUserId": "@dev-new:matrix.local",
                    }
                ]
            },
        },
    )

    assert second.changed_from(first)


def test_runtime_config_change_detection_includes_agent_identity_and_credential_bindings(
    tmp_path: Path,
) -> None:
    first = MemberRuntimeConfig(
        path=tmp_path / "runtime.yaml",
        raw={
            "metadata": {"generation": "1"},
            "desired": {
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
            "agentIdentityData": {"endpoint": "agentidentitydata.cn-beijing.aliyuncs.com"},
        },
    )
    changed_workload = MemberRuntimeConfig(
        path=tmp_path / "runtime.yaml",
        raw={
            "metadata": {"generation": "1"},
            "desired": {
                "agentIdentity": {"workloadIdentityName": "wi-worker-b"},
                "credentialBindings": [
                    {
                        "credentialRef": {
                            "tokenVaultName": "default",
                            "apiKeyCredentialProviderName": "GITHUB_TOKEN",
                        }
                    }
                ],
            },
            "agentIdentityData": {"endpoint": "agentidentitydata.cn-beijing.aliyuncs.com"},
        },
    )
    changed_binding = MemberRuntimeConfig(
        path=tmp_path / "runtime.yaml",
        raw={
            "metadata": {"generation": "1"},
            "desired": {
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
            "agentIdentityData": {"endpoint": "agentidentitydata.cn-beijing.aliyuncs.com"},
        },
    )
    changed_tool_whitelist = MemberRuntimeConfig(
        path=tmp_path / "runtime.yaml",
        raw={
            "metadata": {"generation": "1"},
            "desired": {
                "agentIdentity": {"workloadIdentityName": "wi-worker-a"},
                "credentialBindings": [
                    {
                        "credentialRef": {
                            "tokenVaultName": "default",
                            "apiKeyCredentialProviderName": "GITHUB_TOKEN",
                        },
                        "toolWhitelist": ["gh"],
                    }
                ],
            },
            "agentIdentityData": {"endpoint": "agentidentitydata.cn-beijing.aliyuncs.com"},
        },
    )
    changed_endpoint = MemberRuntimeConfig(
        path=tmp_path / "runtime.yaml",
        raw={
            "metadata": {"generation": "1"},
            "desired": {
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
            "agentIdentityData": {"endpoint": "agentidentitydata.cn-shanghai.aliyuncs.com"},
        },
    )
    changed_region = MemberRuntimeConfig(
        path=tmp_path / "runtime.yaml",
        raw={
            "metadata": {"generation": "1"},
            "desired": {
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
            "agentIdentityData": {"regionId": "cn-hangzhou"},
        },
    )
    unchanged = MemberRuntimeConfig(
        path=tmp_path / "runtime.yaml",
        raw={
            "metadata": {"generation": "1"},
            "desired": {
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
            "agentIdentityData": {"endpoint": "agentidentitydata.cn-beijing.aliyuncs.com"},
        },
    )

    assert changed_workload.changed_from(first)
    assert changed_binding.changed_from(first)
    assert changed_tool_whitelist.changed_from(first)
    assert changed_endpoint.changed_from(first)
    assert changed_region.changed_from(first)
    assert not unchanged.changed_from(first)
