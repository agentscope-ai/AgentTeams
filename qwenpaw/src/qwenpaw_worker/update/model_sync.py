"""Runtime desired-state update support for qwenpaw-worker."""

from __future__ import annotations

import json
import logging
import os
import time
import urllib.error
import urllib.request
from dataclasses import dataclass
from typing import Any, Dict, Iterable, Optional, Tuple
from urllib.parse import quote, urlencode

from qwenpaw_worker.update.constants import DEFAULT_AGENT_ID
from qwenpaw_worker.update.runtime_config import MemberRuntimeConfig
from qwenpaw_worker.update.utils import _duration_ms, _string

logger = logging.getLogger(__name__)

@dataclass(frozen=True)
class ApplyResult:
    runtime_config: MemberRuntimeConfig
    changed: bool
    agent_package_dir: Optional[Path]


@dataclass(frozen=True)
class _QwenPawApiResponse:
    status: int
    payload: Any
    text: str


class QwenPawModelRuntimeSync:
    """Sync runtime model state into the running QwenPaw app."""

    def __init__(
        self,
        port: int,
        agent_id: str = DEFAULT_AGENT_ID,
        timeout: float = 10.0,
    ) -> None:
        self.api_root = f"http://127.0.0.1:{port}/api/models"
        self.agent_id = agent_id
        self.timeout = timeout

    def __call__(self, runtime_config: MemberRuntimeConfig) -> None:
        self.sync(runtime_config)

    def sync(self, runtime_config: MemberRuntimeConfig) -> None:
        started_at = time.monotonic()
        fields = self._model_fields(runtime_config)
        if fields is None:
            return
        provider_id, model_name, provider_name, base_url, api_key, chat_model = fields
        if not base_url or not api_key:
            logger.info(
                "runtime model sync skipped component=update step=model_runtime_sync event=skip "
                "generation=%s provider_id=%s model=%s reason=missing_provider_config",
                runtime_config.generation,
                provider_id,
                model_name,
            )
            return

        provider_count = self._provider_count(self._request("GET", "").payload)
        provider_info = self._configure_provider(provider_id, provider_name, base_url, api_key, chat_model)
        if not self._provider_has_model(provider_info, model_name):
            provider_info = self._add_model(provider_id, model_name)
        self._set_active_model(provider_id, model_name)
        self._verify_active_model(provider_id, model_name)
        logger.info(
            "runtime model sync complete component=update step=model_runtime_sync event=complete "
            "generation=%s provider_id=%s model=%s provider_count=%s changed=%s duration_ms=%s",
            runtime_config.generation,
            provider_id,
            model_name,
            provider_count,
            True,
            _duration_ms(started_at),
        )

    def _model_fields(self, runtime_config: MemberRuntimeConfig) -> Optional[Tuple[str, str, str, str, str, str]]:
        model = runtime_config.model
        provider_id = _string(model.get("providerId") or model.get("provider_id") or model.get("provider"))
        model_name = _string(model.get("model") or model.get("name"))
        if not provider_id or not model_name:
            return None
        base_url = _string(
            model.get("baseUrl")
            or model.get("base_url")
            or model.get("gatewayUrl")
            or model.get("gateway_url")
            or model.get("endpoint")
            or os.getenv("AGENTTEAMS_AI_GATEWAY_URL")
        )
        api_key = _string(model.get("apiKey") or model.get("api_key"))
        api_key_env = _string(
            model.get("apiKeyEnv")
            or model.get("api_key_env")
            or runtime_config.credentials.get("gatewayKeyEnv")
            or "AGENTTEAMS_WORKER_GATEWAY_KEY"
        )
        if not api_key and api_key_env:
            api_key = _string(os.getenv(api_key_env))
        return (
            provider_id,
            model_name,
            _string(model.get("providerName") or model.get("provider_name") or provider_id),
            self._openai_compatible_base_url(base_url) if base_url else "",
            api_key,
            _string(model.get("chatModel") or model.get("chat_model") or "OpenAIChatModel"),
        )

    def _configure_provider(
        self,
        provider_id: str,
        provider_name: str,
        base_url: str,
        api_key: str,
        chat_model: str,
    ) -> Dict[str, Any]:
        payload = {"api_key": api_key, "base_url": base_url, "chat_model": chat_model}
        path = f"/{quote(provider_id, safe='')}/config"
        response = self._request("PUT", path, payload, ok_statuses=(200, 404))
        if response.status == 404:
            self._request(
                "POST",
                "/custom-providers",
                {
                    "id": provider_id,
                    "name": provider_name,
                    "default_base_url": base_url,
                    "api_key_prefix": "",
                    "chat_model": chat_model,
                    "models": [],
                },
                ok_statuses=(200, 201),
            )
            response = self._request("PUT", path, payload)
        return response.payload if isinstance(response.payload, dict) else {}

    def _add_model(self, provider_id: str, model_name: str) -> Dict[str, Any]:
        response = self._request(
            "POST",
            f"/{quote(provider_id, safe='')}/models",
            {"id": model_name, "name": model_name},
            ok_statuses=(200, 201, 400, 404, 409, 422),
        )
        if response.status >= 400 and not self._already_exists(response.text):
            raise RuntimeError(
                f"qwenpaw model runtime sync request failed: POST provider model HTTP {response.status}"
            )
        return response.payload if isinstance(response.payload, dict) else {}

    def _set_active_model(self, provider_id: str, model_name: str) -> None:
        self._request(
            "PUT",
            "/active",
            {
                "scope": "agent",
                "agent_id": self.agent_id,
                "provider_id": provider_id,
                "model": model_name,
            },
        )

    def _verify_active_model(self, provider_id: str, model_name: str) -> None:
        response = self._request(
            "GET",
            f"/active?{urlencode({'scope': 'effective', 'agent_id': self.agent_id})}",
        )
        payload = response.payload if isinstance(response.payload, dict) else {}
        active = payload.get("active_llm") if isinstance(payload.get("active_llm"), dict) else {}
        actual_provider = _string(active.get("provider_id") or active.get("providerId"))
        actual_model = _string(active.get("model"))
        if actual_provider != provider_id or actual_model != model_name:
            raise RuntimeError("qwenpaw model runtime sync verification failed")

    def _request(
        self,
        method: str,
        path: str,
        payload: Optional[Dict[str, Any]] = None,
        ok_statuses: Iterable[int] = (200, 201),
    ) -> _QwenPawApiResponse:
        body = json.dumps(payload).encode("utf-8") if payload is not None else None
        headers = {"Accept": "application/json"}
        if body is not None:
            headers["Content-Type"] = "application/json"
        request = urllib.request.Request(
            f"{self.api_root}{path}",
            data=body,
            headers=headers,
            method=method,
        )
        try:
            with urllib.request.urlopen(request, timeout=self.timeout) as response:
                text = response.read().decode("utf-8", errors="replace")
                return _QwenPawApiResponse(
                    status=response.status,
                    payload=self._json_payload(text, response.status),
                    text=text,
                )
        except urllib.error.HTTPError as exc:
            text = exc.read().decode("utf-8", errors="replace")
            if exc.code in ok_statuses:
                return _QwenPawApiResponse(
                    status=exc.code,
                    payload=self._json_payload(text, exc.code),
                    text=text,
                )
            raise RuntimeError(
                f"qwenpaw model runtime sync request failed: {method} {path.split('?', 1)[0]} HTTP {exc.code}"
            ) from None
        except urllib.error.URLError as exc:
            raise RuntimeError(
                f"qwenpaw model runtime sync request failed: {method} {path.split('?', 1)[0]} "
                f"{type(exc.reason).__name__}"
            ) from None

    def _json_payload(self, text: str, status: int) -> Any:
        if not text:
            return {}
        try:
            return json.loads(text)
        except json.JSONDecodeError as exc:
            if status >= 400:
                return {}
            raise RuntimeError("qwenpaw model runtime sync request failed: invalid JSON response") from exc

    def _provider_count(self, payload: Any) -> int:
        return len(payload) if isinstance(payload, list) else 0

    def _provider_has_model(self, payload: Dict[str, Any], model_name: str) -> bool:
        for key in ("models", "extra_models"):
            value = payload.get(key)
            if not isinstance(value, list):
                continue
            for item in value:
                if isinstance(item, dict) and _string(item.get("id")) == model_name:
                    return True
        return False

    def _already_exists(self, text: str) -> bool:
        lower = text.lower()
        return "already" in lower or "exist" in lower or "duplicate" in lower

    def _openai_compatible_base_url(self, base_url: str) -> str:
        value = base_url.rstrip("/")
        if value.endswith("/v1"):
            return value
        return f"{value}/v1"

