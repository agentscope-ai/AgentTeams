from __future__ import annotations

import calendar
import logging
from pathlib import Path
from types import SimpleNamespace
import os
import time
import qwenpaw_worker.credentials as credentials_module

import pytest

from qwenpaw_worker.credentials import (
    _DEFAULT_STS_PROVIDERS,
    AgentIdentityCredentialResolver,
    AlibabaCloudAgentIdentityDataClient,
    ControllerSTSProvider,
    default_agentidentity_data_client,
)
from qwenpaw_worker.update import REGION_ID_ENV_NAMES, MemberRuntimeConfig


@pytest.fixture(autouse=True)
def clear_region_env(monkeypatch: pytest.MonkeyPatch) -> None:
    for name in REGION_ID_ENV_NAMES:
        monkeypatch.delenv(name, raising=False)


class FakeAgentIdentityDataClient:
    def __init__(self) -> None:
        self.calls: list[tuple[str, str, str | None]] = []

    def get_workload_access_token(self, workload_identity_name: str) -> str:
        self.calls.append(("GetWorkloadAccessToken", workload_identity_name, None))
        return "workload-token"

    def get_resource_api_key(self, provider_name: str, workload_access_token: str) -> str:
        self.calls.append(("GetResourceAPIKey", provider_name, workload_access_token))
        return f"real-{provider_name.lower()}"


class FakeGeneratedModels:
    class GetWorkloadAccessTokenRequest:
        def __init__(self, workload_identity_name: str) -> None:
            self.workload_identity_name = workload_identity_name

    class GetResourceAPIKeyRequest:
        def __init__(
            self,
            resource_credential_provider_name: str,
            workload_access_token: str,
        ) -> None:
            self.resource_credential_provider_name = resource_credential_provider_name
            self.workload_access_token = workload_access_token


class FakeGeneratedSdkClient:
    def __init__(self) -> None:
        self.config: object | None = None
        self.workload_requests: list[FakeGeneratedModels.GetWorkloadAccessTokenRequest] = []
        self.requests: list[FakeGeneratedModels.GetResourceAPIKeyRequest] = []

    def get_workload_access_token(
        self,
        request: FakeGeneratedModels.GetWorkloadAccessTokenRequest,
    ) -> SimpleNamespace:
        self.workload_requests.append(request)
        return SimpleNamespace(body=SimpleNamespace(workload_access_token="workload-token"))

    def get_resource_apikey(
        self,
        request: FakeGeneratedModels.GetResourceAPIKeyRequest,
    ) -> SimpleNamespace:
        self.requests.append(request)
        return SimpleNamespace(body=SimpleNamespace(apikey="real-provider-secret"))


class FakeAlibabaCloudAgentIdentityDataClient(AlibabaCloudAgentIdentityDataClient):
    def __init__(self, sdk_client: FakeGeneratedSdkClient, endpoint: str = "agentidentitydata.cn-beijing.aliyuncs.com") -> None:
        self._sts_provider = object()
        self._endpoint = endpoint
        self._client = sdk_client

    @staticmethod
    def _models() -> type[FakeGeneratedModels]:
        return FakeGeneratedModels


def test_alibaba_cloud_data_client_uses_official_get_resource_apikey_method() -> None:
    sdk_client = FakeGeneratedSdkClient()
    client = FakeAlibabaCloudAgentIdentityDataClient(sdk_client)

    api_key = client.get_resource_api_key("GITHUB_TOKEN", "workload-token")

    assert api_key == "real-provider-secret"
    assert len(sdk_client.requests) == 1
    assert sdk_client.requests[0].resource_credential_provider_name == "GITHUB_TOKEN"
    assert sdk_client.requests[0].workload_access_token == "workload-token"


class FakeOpenAPIConfig:
    def __init__(self, **kwargs) -> None:
        self.kwargs = kwargs
        self.endpoint = ""
        self.region_id = ""


class FakeGeneratedClient:
    last_config: FakeOpenAPIConfig | None = None

    def __init__(self, config: FakeOpenAPIConfig) -> None:
        FakeGeneratedClient.last_config = config


class FakeSTSProvider:
    def __init__(self) -> None:
        self.calls = 0

    def fetch(self) -> dict[str, object]:
        self.calls += 1
        return {
            "access_key_id": "sts-ak",
            "access_key_secret": "sts-sk",
            "security_token": "sts-token",
            "expires_in_sec": 3600,
        }


class FakeSDKEndpointClient(AlibabaCloudAgentIdentityDataClient):
    @staticmethod
    def _models() -> type[FakeGeneratedModels]:
        return FakeGeneratedModels

    @staticmethod
    def _sdk_client_class() -> type[FakeGeneratedClient]:
        return FakeGeneratedClient

    @staticmethod
    def _open_api_config_class() -> type[FakeOpenAPIConfig]:
        return FakeOpenAPIConfig


def test_alibaba_cloud_data_client_uses_runtime_endpoint_without_region_id() -> None:
    FakeGeneratedClient.last_config = None
    sts_provider = FakeSTSProvider()
    client = FakeSDKEndpointClient(
        sts_provider,
        endpoint="agentidentitydata.cn-beijing.aliyuncs.com",
    )

    client._sdk_client()

    assert sts_provider.calls == 1
    assert FakeGeneratedClient.last_config is not None
    assert FakeGeneratedClient.last_config.endpoint == "agentidentitydata.cn-beijing.aliyuncs.com"
    assert FakeGeneratedClient.last_config.region_id == ""


def test_alibaba_cloud_data_client_requires_runtime_endpoint() -> None:
    client = FakeSDKEndpointClient(FakeSTSProvider(), endpoint="")

    try:
        client._sdk_client()
    except RuntimeError as exc:
        assert "runtime.yaml agentIdentityData.regionId or endpoint" in str(exc)
    else:
        raise AssertionError("expected missing endpoint to fail")


def test_default_agentidentity_data_client_reuses_sts_provider_by_effective_endpoint(tmp_path: Path) -> None:
    _DEFAULT_STS_PROVIDERS.clear()
    config = MemberRuntimeConfig(
        path=tmp_path / "runtime.yaml",
        raw={"agentIdentityData": {"regionId": "cn-beijing"}},
    )
    other_config = MemberRuntimeConfig(
        path=tmp_path / "runtime-other.yaml",
        raw={"agentIdentityData": {"regionId": "cn-hangzhou"}},
    )

    first = default_agentidentity_data_client(config)
    second = default_agentidentity_data_client(config)
    third = default_agentidentity_data_client(other_config)

    assert getattr(first, "_sts_provider") is getattr(second, "_sts_provider")
    assert getattr(first, "_sts_provider") is not getattr(third, "_sts_provider")
    assert getattr(first, "_endpoint") == "agentidentitydata.cn-beijing.aliyuncs.com"
    assert getattr(third, "_endpoint") == "agentidentitydata.cn-hangzhou.aliyuncs.com"
    _DEFAULT_STS_PROVIDERS.clear()


class FakeCachingSTSProvider(ControllerSTSProvider):
    def __init__(
        self,
        responses: list[dict[str, object]],
        *,
        now: list[float],
        endpoint: str = "agentidentitydata.cn-beijing.aliyuncs.com",
    ) -> None:
        super().__init__(endpoint=endpoint, now=lambda: now[0])
        self._responses = responses
        self.requests: list[tuple[str, str, str]] = []

    def _controller_context(self) -> tuple[str, str, str]:
        return ("http://controller.test", "cluster-a", "bearer")

    def _request_sts(self, controller_url: str, cluster_id: str, bearer: str) -> dict[str, object]:
        self.requests.append((controller_url, cluster_id, bearer))
        return self._responses.pop(0)


def test_controller_sts_provider_reuses_cached_token_until_refresh_margin() -> None:
    now = [1000.0]
    provider = FakeCachingSTSProvider(
        [
            {"access_key_id": "ak-1", "access_key_secret": "sk-1", "expires_in_sec": 3600},
            {"access_key_id": "ak-2", "access_key_secret": "sk-2", "expires_in_sec": 3600},
        ],
        now=now,
    )

    assert provider.fetch()["access_key_id"] == "ak-1"
    now[0] += 2400
    assert provider.fetch()["access_key_id"] == "ak-1"
    now[0] += 700
    assert provider.fetch()["access_key_id"] == "ak-2"
    assert len(provider.requests) == 2


def test_controller_sts_provider_sends_agentidentitydata_purpose_as_query(monkeypatch) -> None:
    captured: dict[str, object] = {}

    class FakeResponse:
        def __enter__(self) -> "FakeResponse":
            return self

        def __exit__(self, exc_type, exc, tb) -> None:
            return None

        def read(self) -> bytes:
            return b'{"access_key_id":"ak","access_key_secret":"sk","expires_in_sec":3600}'

    def fake_urlopen(request, timeout):
        captured["url"] = request.full_url
        captured["method"] = request.get_method()
        captured["headers"] = {key.lower(): value for key, value in request.header_items()}
        captured["timeout"] = timeout
        return FakeResponse()

    monkeypatch.setenv("AGENTTEAMS_CONTROLLER_URL", "http://controller.test")
    monkeypatch.setenv("AGENTTEAMS_CLUSTER_ID", "cluster-a")
    monkeypatch.setenv("AGENTTEAMS_AUTH_TOKEN", "bearer-token")
    monkeypatch.setattr(credentials_module.urllib.request, "urlopen", fake_urlopen)

    token = ControllerSTSProvider(purpose="agentidentitydata").fetch()

    assert token["access_key_id"] == "ak"
    assert captured["method"] == "POST"
    assert captured["timeout"] == 60
    assert captured["url"] == "http://controller.test/api/v1/credentials/sts?purpose=agentidentitydata"
    assert captured["headers"]["authorization"] == "Bearer bearer-token"
    assert captured["headers"]["x-agentteams-cluster-id"] == "cluster-a"


def test_controller_sts_provider_without_cluster_id_skips_auth_cluster_header(monkeypatch) -> None:
    captured: dict[str, object] = {}

    class FakeResponse:
        def __enter__(self) -> "FakeResponse":
            return self

        def __exit__(self, exc_type, exc, tb) -> None:
            return None

        def read(self) -> bytes:
            return b'{"access_key_id":"ak","access_key_secret":"sk","expires_in_sec":3600}'

    def fake_urlopen(request, timeout):
        captured["headers"] = {key.lower(): value for key, value in request.header_items()}
        captured["timeout"] = timeout
        return FakeResponse()

    monkeypatch.setenv("AGENTTEAMS_CONTROLLER_URL", "http://controller.test")
    monkeypatch.delenv("AGENTTEAMS_CLUSTER_ID", raising=False)
    monkeypatch.setenv("AGENTTEAMS_AUTH_TOKEN", "bearer-token")
    monkeypatch.setattr(credentials_module.urllib.request, "urlopen", fake_urlopen)

    token = ControllerSTSProvider(purpose="agentidentitydata").fetch()

    assert token["access_key_id"] == "ak"
    assert captured["timeout"] == 60
    headers = captured["headers"]
    assert headers["authorization"] == "Bearer bearer-token"
    assert "x-agentteams-cluster-id" not in headers


def test_controller_sts_provider_cache_is_scoped_by_endpoint() -> None:
    now = [1000.0]
    provider = FakeCachingSTSProvider(
        [
            {"access_key_id": "ak-1", "access_key_secret": "sk-1", "expires_in_sec": 3600},
            {"access_key_id": "ak-2", "access_key_secret": "sk-2", "expires_in_sec": 3600},
        ],
        now=now,
    )

    assert provider.fetch()["access_key_id"] == "ak-1"
    provider.endpoint = "agentidentitydata.cn-shanghai.aliyuncs.com"
    assert provider.fetch()["access_key_id"] == "ak-2"
    assert len(provider.requests) == 2


def test_controller_sts_provider_uses_expiration_when_expires_in_sec_is_missing() -> None:
    now = [float(calendar.timegm(time.strptime("2026-06-23T00:00:00Z", "%Y-%m-%dT%H:%M:%SZ")))]
    provider = FakeCachingSTSProvider(
        [
            {
                "access_key_id": "ak-1",
                "access_key_secret": "sk-1",
                "expiration": "2026-06-23T01:00:00Z",
            },
            {
                "access_key_id": "ak-2",
                "access_key_secret": "sk-2",
                "expiration": "2026-06-23T02:00:00Z",
            },
        ],
        now=now,
    )

    assert provider.fetch()["access_key_id"] == "ak-1"
    now[0] += 3500
    assert provider.fetch()["access_key_id"] == "ak-2"


def test_resolver_returns_env_for_bound_credentials_only(tmp_path: Path) -> None:
    config = MemberRuntimeConfig(
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
    client = FakeAgentIdentityDataClient()

    env = AgentIdentityCredentialResolver(config, client).resolve_env(
        {"GITHUB_TOKEN", "NOT_BOUND"}
    )

    assert env == {"GITHUB_TOKEN": "real-github_token"}
    assert client.calls == [
        ("GetWorkloadAccessToken", "wi-worker-a", None),
        ("GetResourceAPIKey", "GITHUB_TOKEN", "workload-token"),
    ]


def test_resolver_logs_safe_credential_summary_without_values(
    tmp_path: Path,
    caplog: pytest.LogCaptureFixture,
) -> None:
    config = MemberRuntimeConfig(
        path=tmp_path / "runtime.yaml",
        raw={
            "member": {"name": "worker-a", "runtime": "qwenpaw"},
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
        },
    )
    caplog.set_level(logging.INFO, logger="qwenpaw_worker.credentials")

    env = AgentIdentityCredentialResolver(config, FakeAgentIdentityDataClient()).resolve_env(
        {"GITHUB_TOKEN", "NOT_BOUND"}
    )

    assert env == {"GITHUB_TOKEN": "real-github_token"}
    assert "component=credential" in caplog.text
    assert "worker=worker-a" in caplog.text
    assert "binding_count=1" in caplog.text
    assert "requested_env_count=2" in caplog.text
    assert "matched_env_names=GITHUB_TOKEN" in caplog.text
    assert "resolved_env_count=1" in caplog.text
    assert "duration_ms=" in caplog.text
    assert "real-github_token" not in caplog.text
    assert "workload-token" not in caplog.text


def test_resolver_uses_prefixed_provider_name_but_injects_standard_env_name(
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
                "agentIdentity": {"workloadIdentityName": "wi-worker-a"},
                "credentialBindings": [
                    {
                        "credentialRef": {
                            "tokenVaultName": "default",
                            "apiKeyCredentialProviderName": "at-cn-4b84u92kf0f-ALIBABA_CLOUD_ACCESS_KEY_ID",
                        }
                    }
                ],
            },
        },
    )
    client = FakeAgentIdentityDataClient()

    env = AgentIdentityCredentialResolver(config, client).resolve_env(
        {"ALIBABA_CLOUD_ACCESS_KEY_ID"}
    )

    assert env == {
        "ALIBABA_CLOUD_ACCESS_KEY_ID": "real-at-cn-4b84u92kf0f-alibaba_cloud_access_key_id",
    }
    assert client.calls == [
        ("GetWorkloadAccessToken", "wi-worker-a", None),
        (
            "GetResourceAPIKey",
            "at-cn-4b84u92kf0f-ALIBABA_CLOUD_ACCESS_KEY_ID",
            "workload-token",
        ),
    ]


def test_resolver_does_not_write_resolved_values_to_global_environment(
    tmp_path: Path,
) -> None:
    os.environ.pop("GITHUB_TOKEN", None)
    config = MemberRuntimeConfig(
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
                    }
                ],
            },
        },
    )

    env = AgentIdentityCredentialResolver(config, FakeAgentIdentityDataClient()).resolve_env(
        {"GITHUB_TOKEN"}
    )

    assert env == {"GITHUB_TOKEN": "real-github_token"}
    assert "GITHUB_TOKEN" not in os.environ
