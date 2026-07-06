from __future__ import annotations

import calendar
import json
import logging
import os
from pathlib import Path
import time
from typing import Any, Protocol
import urllib.error
import urllib.parse
import urllib.request

from .update import MemberRuntimeConfig

logger = logging.getLogger(__name__)


class AgentIdentityDataClient(Protocol):
    def get_workload_access_token(self, workload_identity_name: str) -> str:
        ...

    def get_resource_api_key(self, provider_name: str, workload_access_token: str) -> str:
        ...


class AgentIdentityCredentialResolver:
    """Resolve bound API keys for one controlled worker execution."""

    def __init__(self, config: MemberRuntimeConfig, data_client: AgentIdentityDataClient) -> None:
        self._config = config
        self._data_client = data_client
        self._workload_access_token: str | None = None

    def resolve_env(self, requested_names: set[str]) -> dict[str, str]:
        started_at = time.monotonic()
        provider_names = self._config.credential_binding_env_provider_names
        names = [name for name in self._config.credential_binding_env_names if name in requested_names and name in provider_names]
        logger.info(
            "credential resolution begin component=credential worker=%s binding_count=%s requested_env_count=%s "
            "matched_env_names=%s workload_identity=%s",
            self._config.member_name,
            len(self._config.credential_bindings),
            len(requested_names),
            _join_names(names),
            bool(self._config.workload_identity_name),
        )
        if not names:
            logger.info(
                "credential resolution complete component=credential worker=%s resolved_env_count=0 "
                "reason=no_matching_bindings duration_ms=%s",
                self._config.member_name,
                _duration_ms(started_at),
            )
            return {}
        workload_identity_name = self._config.workload_identity_name
        if not workload_identity_name:
            logger.info(
                "credential resolution complete component=credential worker=%s resolved_env_count=0 "
                "reason=no_workload_identity duration_ms=%s",
                self._config.member_name,
                _duration_ms(started_at),
            )
            return {}

        try:
            token = self._workload_token(workload_identity_name)
        except Exception as exc:
            logger.warning(
                "credential resolution failed component=credential worker=%s step=workload_token error_type=%s duration_ms=%s",
                self._config.member_name,
                type(exc).__name__,
                _duration_ms(started_at),
            )
            raise
        env: dict[str, str] = {}
        for name in names:
            try:
                value = self._data_client.get_resource_api_key(provider_names[name], token)
            except Exception as exc:
                logger.warning(
                    "credential resolution failed component=credential worker=%s step=resource_api_key "
                    "env_name=%s provider_name=%s error_type=%s duration_ms=%s",
                    self._config.member_name,
                    name,
                    provider_names[name],
                    type(exc).__name__,
                    _duration_ms(started_at),
                )
                raise
            if value:
                env[name] = value
        logger.info(
            "credential resolution complete component=credential worker=%s resolved_env_count=%s resolved_env_names=%s "
            "duration_ms=%s",
            self._config.member_name,
            len(env),
            _join_names(env.keys()),
            _duration_ms(started_at),
        )
        return env

    def _workload_token(self, workload_identity_name: str) -> str:
        if self._workload_access_token is None:
            self._workload_access_token = self._data_client.get_workload_access_token(workload_identity_name)
        return self._workload_access_token


class UnavailableAgentIdentityDataClient:
    def __init__(self, reason: str) -> None:
        self._reason = reason

    def get_workload_access_token(self, workload_identity_name: str) -> str:
        raise RuntimeError(f"AgentIdentityData client unavailable: {self._reason}")

    def get_resource_api_key(self, provider_name: str, workload_access_token: str) -> str:
        raise RuntimeError(f"AgentIdentityData client unavailable: {self._reason}")


_DEFAULT_STS_PROVIDERS: dict[tuple[str, str], "ControllerSTSProvider"] = {}


def _default_sts_provider(purpose: str, endpoint: str) -> "ControllerSTSProvider":
    key = (purpose, endpoint)
    provider = _DEFAULT_STS_PROVIDERS.get(key)
    if provider is None:
        provider = ControllerSTSProvider(purpose=purpose, endpoint=endpoint)
        _DEFAULT_STS_PROVIDERS[key] = provider
    return provider


def default_agentidentity_data_client(config: MemberRuntimeConfig) -> AgentIdentityDataClient:
    endpoint = config.agent_identity_data_endpoint
    return AlibabaCloudAgentIdentityDataClient(
        _default_sts_provider("agentidentitydata", endpoint),
        endpoint=endpoint,
    )


def _string(value: Any) -> str:
    return value.strip() if isinstance(value, str) else ""


def _join_names(values: Any) -> str:
    names = sorted(str(value).strip() for value in values if str(value).strip())
    return ",".join(names) if names else "-"


def _duration_ms(started_at: float) -> int:
    return max(0, int((time.monotonic() - started_at) * 1000))


def _first_text(payload: Any, *names: str) -> str:
    current = payload
    for name in ("body", "Body"):
        if hasattr(current, name):
            current = getattr(current, name)
            break
        if isinstance(current, dict) and name in current:
            current = current[name]
            break
    for name in names:
        if hasattr(current, name):
            value = _string(getattr(current, name))
            if value:
                return value
        if isinstance(current, dict):
            value = _string(current.get(name))
            if value:
                return value
    return ""


class ControllerSTSProvider:
    def __init__(
        self,
        purpose: str = "agentidentitydata",
        *,
        endpoint: str = "",
        now: Any = time.time,
    ) -> None:
        self._purpose = purpose
        self.endpoint = endpoint
        self._now = now
        self._refresh_margin_seconds = 600
        self._cached: dict[str, Any] | None = None
        self._cache_key: tuple[str, str, str, str] | None = None
        self._expires_at = 0.0

    def fetch(self) -> dict[str, Any]:
        controller_url, cluster_id, bearer = self._controller_context()
        cache_key = (self._purpose, controller_url, cluster_id, self.endpoint)
        now = float(self._now())
        if (
            self._cached is not None
            and self._cache_key == cache_key
            and now + self._refresh_margin_seconds < self._expires_at
        ):
            return dict(self._cached)

        payload = self._request_sts(controller_url, cluster_id, bearer)
        if not isinstance(payload, dict):
            raise RuntimeError("AgentIdentityData STS response must be an object")
        self._cached = dict(payload)
        self._cache_key = cache_key
        self._expires_at = self._sts_expires_at(payload, now)
        return dict(payload)

    def _controller_context(self) -> tuple[str, str, str]:
        controller_url = os.getenv("AGENTTEAMS_CONTROLLER_URL", "").strip().rstrip("/")
        if not controller_url:
            raise RuntimeError("AgentIdentityData STS requires AGENTTEAMS_CONTROLLER_URL")
        cluster_id = self._auth_cluster_id()
        return controller_url, cluster_id, self._controller_bearer_token()

    def _request_sts(self, controller_url: str, cluster_id: str, bearer: str) -> dict[str, Any]:
        body = json.dumps({"purpose": self._purpose}).encode("utf-8")
        url = f"{controller_url}/api/v1/credentials/sts"
        if self._purpose:
            url = f"{url}?{urllib.parse.urlencode({'purpose': self._purpose})}"
        headers = {
            "Authorization": f"Bearer {bearer}",
            "Content-Type": "application/json",
        }
        if cluster_id:
            headers["X-AgentTeams-Cluster-ID"] = cluster_id
        request = urllib.request.Request(
            url,
            data=body,
            headers=headers,
            method="POST",
        )
        try:
            with urllib.request.urlopen(request, timeout=60) as response:
                payload = json.loads(response.read().decode("utf-8"))
        except urllib.error.HTTPError as exc:
            body_text = exc.read().decode("utf-8", errors="replace")
            raise RuntimeError(f"AgentIdentityData STS request failed: HTTP {exc.code}: {body_text}") from None
        except urllib.error.URLError as exc:
            raise RuntimeError(f"AgentIdentityData STS request failed: {exc.reason}") from None
        except json.JSONDecodeError as exc:
            raise RuntimeError("AgentIdentityData STS request failed: invalid JSON response") from exc
        if not isinstance(payload, dict):
            raise RuntimeError("AgentIdentityData STS response must be an object")
        return payload

    @staticmethod
    def _sts_expires_at(payload: dict[str, Any], now: float) -> float:
        for name in ("expires_in_sec", "expiresInSec", "ExpiresInSec"):
            try:
                seconds = int(payload.get(name) or 0)
            except (TypeError, ValueError):
                seconds = 0
            if seconds > 0:
                return now + seconds

        expiration = _first_text(payload, "expiration", "Expiration")
        if expiration:
            try:
                if expiration.endswith("Z"):
                    return float(calendar.timegm(time.strptime(expiration, "%Y-%m-%dT%H:%M:%SZ")))
                parsed = time.strptime(expiration[:19], "%Y-%m-%dT%H:%M:%S")
                return float(time.mktime(parsed))
            except ValueError:
                pass
        return now + 3600

    @staticmethod
    def _controller_bearer_token() -> str:
        token = os.getenv("AGENTTEAMS_AUTH_TOKEN", "").strip()
        if token:
            return token
        token_file = os.getenv("AGENTTEAMS_AUTH_TOKEN_FILE", "").strip()
        if token_file:
            path = Path(token_file)
            if not path.exists():
                raise RuntimeError(f"AGENTTEAMS_AUTH_TOKEN_FILE does not exist: {token_file}")
            token = path.read_text(encoding="utf-8").strip()
            if token:
                return token
        raise RuntimeError("AgentIdentityData STS requires AGENTTEAMS_AUTH_TOKEN or AGENTTEAMS_AUTH_TOKEN_FILE")

    @staticmethod
    def _auth_cluster_id() -> str:
        return os.getenv("AGENTTEAMS_CLUSTER_ID", "").strip()


class AlibabaCloudAgentIdentityDataClient:
    def __init__(self, sts_provider: ControllerSTSProvider, *, endpoint: str) -> None:
        self._sts_provider = sts_provider
        self._endpoint = endpoint.strip()
        self._client: Any = None

    def get_workload_access_token(self, workload_identity_name: str) -> str:
        models = self._models()
        request = models.GetWorkloadAccessTokenRequest(
            workload_identity_name=workload_identity_name
        )
        response = self._sdk_client().get_workload_access_token(request)
        token = _first_text(
            response,
            "workload_access_token",
            "workloadAccessToken",
            "WorkloadAccessToken",
        )
        if not token:
            raise RuntimeError("AgentIdentityData GetWorkloadAccessToken response missing token")
        return token

    def get_resource_api_key(self, provider_name: str, workload_access_token: str) -> str:
        models = self._models()
        request = models.GetResourceAPIKeyRequest(
            resource_credential_provider_name=provider_name,
            workload_access_token=workload_access_token,
        )
        response = self._sdk_client().get_resource_apikey(request)
        api_key = _first_text(response, "apikey", "api_key", "apiKey", "APIKey")
        if not api_key:
            raise RuntimeError("AgentIdentityData GetResourceAPIKey response missing APIKey")
        return api_key

    @staticmethod
    def _models() -> Any:
        from alibabacloud_agentidentitydata20251127 import models

        return models

    def _sdk_client(self) -> Any:
        if self._client is not None:
            return self._client
        if not self._endpoint:
            raise RuntimeError("runtime.yaml agentIdentityData.regionId or endpoint is required for AgentIdentityData SDK")
        Client = self._sdk_client_class()
        Config = self._open_api_config_class()

        sts = self._sts_provider.fetch()
        access_key = _first_text(sts, "access_key_id", "accessKeyId", "AccessKeyID")
        secret_key = _first_text(sts, "access_key_secret", "accessKeySecret", "AccessKeySecret")
        security_token = _first_text(sts, "security_token", "securityToken", "SecurityToken")
        if not access_key or not secret_key:
            raise RuntimeError("AgentIdentityData STS response missing access key fields")
        config = Config(
            access_key_id=access_key,
            access_key_secret=secret_key,
            security_token=security_token,
        )
        config.endpoint = self._endpoint
        self._client = Client(config)
        return self._client

    @staticmethod
    def _sdk_client_class() -> Any:
        from alibabacloud_agentidentitydata20251127.client import Client

        return Client

    @staticmethod
    def _open_api_config_class() -> Any:
        from alibabacloud_tea_openapi import models as open_api_models

        return open_api_models.Config
