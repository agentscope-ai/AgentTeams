"""CoPaw worker health state.

Strategy:
  - CoPaw owns its health semantics. The controller should not aggregate or
    infer CoPaw component health.
  - The public health state is a full snapshot of all CoPaw components, not a
    single event. Individual components may update only their own status, but
    the persisted state always contains the complete component table.
  - The snapshot always contains all components. Each component starts as
    unhealthy at process initialization and only becomes healthy after its
    concrete startup/runtime check succeeds.
  - Component health detection strategy:
      * copaw:
          Startup health:
            - check: start uvicorn.Server for "copaw.app._app:app".
            - check: after starting the server, the worker performs one
              bounded startup probe against the native CoPaw health endpoint.
            - healthy: the startup probe gets HTTP 200 from
              http://127.0.0.1:{console_port}/health.
            - unhealthy: server startup raises or server.serve() returns before
              a shutdown request, or the bounded startup probe cannot reach
              the native CoPaw health endpoint.
          Runtime health:
            - check: health report loop probes
              http://127.0.0.1:{console_port}/health periodically.
            - healthy: probe returns 200.
            - unhealthy: probe fails/times out, the FastAPI app exits
              unexpectedly, or server.serve() returns before requested
              shutdown.
      * sync:
          Startup health:
            - check: FileSync.mirror_all().
            - healthy: mirror_all() returns without raising.
            - unhealthy: mirror_all() raises, including storage authentication,
              bucket/object access, network, or local write failures.
            - meaning: startup mirror failure is a hard dependency failure for
              CoPaw startup because later stages depend on the restored
              standard sync root.
          Runtime health:
            - check: push_loop storage persistence of local changes to
              MinIO/OSS.
            - healthy: local changes can be persisted to MinIO/OSS.
            - unhealthy: storage push fails and local changes cannot be
              persisted. This should be reported/alerted because state may be
              lost, even if the CoPaw app is still serving normally.
            - boundary: sync health does not own bridge_runtime_to_standard()
              or bridge_standard_to_runtime(). Those functions may be called
              from sync code, but their failures belong to bridge health.
      * bridge:
          Startup health:
            - check: bridge_standard_to_runtime(local_dir, runtime_dir,
              openclaw_cfg, skill_names=...).
            - healthy: bridge_standard_to_runtime() returns without raising.
            - unhealthy: bridge_standard_to_runtime() raises. The bridge module
              owns the detailed standard-to-CoPaw conversion logic and should
              surface a useful error message.
          Runtime health:
            - check: bridge_runtime_to_standard(local_dir), currently invoked
              by push_local() before upload.
            - healthy: bridge_runtime_to_standard() returns without raising.
            - unhealthy: bridge_runtime_to_standard() raises while copying
              runtime state back into the standard sync root. The bridge module
              owns the detailed runtime-to-standard conversion logic and should
              surface a useful error message.
      * model:
          Startup health:
            - check: resolve the active provider/model from openclaw.json, then
              call POST {baseUrl}/chat/completions with the configured API key
              when present.
            - healthy: an active provider/baseUrl exist and a minimal chat
              completion request returns 2xx.
            - unhealthy: no active provider/baseUrl exists, the request raises,
              times out, or returns non-2xx.
          Runtime health:
            - check: worker API GET /worker/readyz repeats the same chat route
              preflight on demand.
            - healthy/unhealthy: same result rules as startup.
            - token cost: this is a real inference preflight with a deliberately
              tiny output budget. Controller polling must stay low-frequency or
              manual to avoid unnecessary token usage.
      * matrix:
          Startup health:
            - check: _matrix_relogin() POSTs to
              {homeserver}/_matrix/client/v3/login with the worker credentials.
            - healthy: login response contains a non-empty access_token.
            - unhealthy: homeserver/password is missing, login raises/times
              out, returns an error, or returns no access_token.
          Runtime health:
            - check: worker API GET /worker/readyz probes
              {homeserver}/_matrix/client/versions on demand.
            - healthy: probe returns 2xx.
            - unhealthy: probe raises, times out, or returns non-2xx.
            - scope: this runtime check only verifies homeserver endpoint
              reachability. It does not validate worker token state, room
              send/receive behavior, sync-loop quality, or E2EE key health.
            - future token-aware check: GET /_matrix/client/v3/account/whoami
              can be added only when token validity needs separate
              classification.
  - Top-level healthiness is derived locally: any unhealthy component makes the
    whole CoPaw worker unhealthy; otherwise it is healthy.
  - The first phase does not add degraded, severity, stage, controller reporting,
    CRD fields, or independent runtime health loops. CoPaw exposes health
    checks; external callers decide when to call them.
"""

from __future__ import annotations

from dataclasses import asdict, dataclass
from datetime import datetime, timezone
import json
from pathlib import Path
from typing import Any, Literal
import urllib.error
import urllib.request

Healthiness = Literal["healthy", "unhealthy"]
HealthComponent = Literal["copaw", "sync", "bridge", "model", "matrix"]

COMPONENTS: tuple[HealthComponent, ...] = (
    "copaw",
    "sync",
    "bridge",
    "model",
    "matrix",
)

@dataclass(frozen=True)
class ComponentHealth:
    healthiness: Healthiness
    message: str = ""
    details: dict[str, Any] | None = None
    updated_at: str = ""


@dataclass(frozen=True)
class HealthSnapshot:
    healthiness: Healthiness
    message: str
    components: dict[HealthComponent, ComponentHealth]
    updated_at: str


class HealthState:
    """Maintain component health and persist a full CoPaw health snapshot."""

    def __init__(self, state_path: Path) -> None:
        self.state_path = state_path
        now = _now()
        self._components: dict[HealthComponent, ComponentHealth] = {
            component: ComponentHealth(
                healthiness="unhealthy",
                message="not checked yet",
                details={},
                updated_at=now,
            )
            for component in COMPONENTS
        }

    def update(
        self,
        component: HealthComponent,
        healthiness: Healthiness,
        message: str = "",
        details: dict[str, Any] | None = None,
    ) -> HealthSnapshot:
        if component not in COMPONENTS:
            raise ValueError(f"unknown health component: {component}")
        if healthiness not in ("healthy", "unhealthy"):
            raise ValueError(f"invalid healthiness: {healthiness}")
        self._components[component] = ComponentHealth(
            healthiness=healthiness,
            message=message,
            details=details or {},
            updated_at=_now(),
        )
        snapshot = self.snapshot()
        self.persist(snapshot)
        return snapshot

    def snapshot(self) -> HealthSnapshot:
        unhealthy = [
            (component, state)
            for component, state in self._components.items()
            if state.healthiness == "unhealthy"
        ]
        if unhealthy:
            component, state = unhealthy[0]
            healthiness: Healthiness = "unhealthy"
            message = f"{component} unhealthy"
            if state.message:
                message = f"{message}: {state.message}"
        else:
            healthiness = "healthy"
            message = "all components healthy"

        return HealthSnapshot(
            healthiness=healthiness,
            message=message,
            components=dict(self._components),
            updated_at=_now(),
        )

    def persist(self, snapshot: HealthSnapshot | None = None) -> HealthSnapshot:
        snapshot = snapshot or self.snapshot()
        self.state_path.parent.mkdir(parents=True, exist_ok=True)
        self.state_path.write_text(
            json.dumps(_snapshot_to_dict(snapshot), indent=2, ensure_ascii=False),
            encoding="utf-8",
        )
        return snapshot

    def to_dict(self) -> dict[str, Any]:
        return _snapshot_to_dict(self.snapshot())


def _snapshot_to_dict(snapshot: HealthSnapshot) -> dict[str, Any]:
    data = asdict(snapshot)
    return data


def check_model_service(
    openclaw_cfg: dict[str, Any],
    *,
    timeout: float = 20,
) -> ComponentHealth:
    active = _active_model_provider(openclaw_cfg)
    if active is None:
        return ComponentHealth(
            healthiness="unhealthy",
            message="no active model provider configured",
            details={"operation": "model_preflight"},
            updated_at=_now(),
        )

    provider_id, model_id, provider_cfg = active
    base_url = str(provider_cfg.get("baseUrl") or "").rstrip("/")
    api_key = str(provider_cfg.get("apiKey") or "")
    details = {
        "operation": "model_preflight",
        "provider": provider_id,
        "model": model_id,
    }
    if not base_url:
        return ComponentHealth(
            healthiness="unhealthy",
            message="active model provider has no baseUrl",
            details=details,
            updated_at=_now(),
        )

    chat_url = f"{base_url}/chat/completions"
    token_param = _max_tokens_param(model_id)
    payload: dict[str, Any] = {
        "model": model_id,
        "messages": [{"role": "user", "content": "ping"}],
        token_param: 1,
    }
    if _supports_enable_thinking(model_id):
        payload["enable_thinking"] = False
    body = json.dumps(payload).encode("utf-8")
    headers = {
        "Accept": "application/json",
        "Content-Type": "application/json",
    }
    if api_key:
        headers["Authorization"] = f"Bearer {api_key}"
    details = {
        **details,
        "endpoint": chat_url,
        "probe": "chat_completion",
        "token_cost": "minimal",
        "max_tokens_param": token_param,
    }
    try:
        req = urllib.request.Request(chat_url, data=body, headers=headers, method="POST")
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            code = int(getattr(resp, "status", 200))
        if 200 <= code < 300:
            return ComponentHealth(
                healthiness="healthy",
                message="model chat completion preflight succeeded",
                details={**details, "http_status": code},
                updated_at=_now(),
            )
        return ComponentHealth(
            healthiness="unhealthy",
            message=f"model chat completion preflight returned HTTP {code}",
            details={**details, "http_status": code},
            updated_at=_now(),
        )
    except urllib.error.HTTPError as exc:
        return ComponentHealth(
            healthiness="unhealthy",
            message=f"model chat completion preflight returned HTTP {exc.code}",
            details={**details, "http_status": exc.code},
            updated_at=_now(),
        )
    except Exception as exc:
        return ComponentHealth(
            healthiness="unhealthy",
            message=f"model chat completion preflight failed: {exc}",
            details={
                **details,
                "error_type": type(exc).__name__,
            },
            updated_at=_now(),
        )


def check_copaw_service(
    console_port: int,
    *,
    timeout: float = 5,
) -> ComponentHealth:
    endpoint = f"http://127.0.0.1:{console_port}/health"
    details = {
        "operation": "copaw_health_probe",
        "endpoint": endpoint,
    }
    try:
        req = urllib.request.Request(endpoint, headers={"Accept": "application/json"}, method="GET")
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            code = int(getattr(resp, "status", 200))
        if code == 200:
            return ComponentHealth(
                healthiness="healthy",
                message="copaw health endpoint reachable",
                details={**details, "http_status": code},
                updated_at=_now(),
            )
        return ComponentHealth(
            healthiness="unhealthy",
            message=f"copaw health endpoint returned HTTP {code}",
            details={**details, "http_status": code},
            updated_at=_now(),
        )
    except urllib.error.HTTPError as exc:
        return ComponentHealth(
            healthiness="unhealthy",
            message=f"copaw health endpoint returned HTTP {exc.code}",
            details={**details, "http_status": exc.code},
            updated_at=_now(),
        )
    except Exception as exc:
        return ComponentHealth(
            healthiness="unhealthy",
            message=f"copaw health endpoint is unreachable: {exc}",
            details={**details, "error_type": type(exc).__name__},
            updated_at=_now(),
        )


def check_matrix_service(
    homeserver: str,
    *,
    timeout: float = 5,
) -> ComponentHealth:
    base_url = str(homeserver or "").rstrip("/")
    details = {"operation": "matrix_endpoint_probe"}
    if not base_url:
        return ComponentHealth(
            healthiness="unhealthy",
            message="matrix homeserver is not configured",
            details=details,
            updated_at=_now(),
        )

    endpoint = f"{base_url}/_matrix/client/versions"
    try:
        req = urllib.request.Request(endpoint, headers={"Accept": "application/json"}, method="GET")
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            code = int(getattr(resp, "status", 200))
        if 200 <= code < 300:
            return ComponentHealth(
                healthiness="healthy",
                message="matrix homeserver reachable",
                details={**details, "endpoint": endpoint, "http_status": code},
                updated_at=_now(),
            )
        return ComponentHealth(
            healthiness="unhealthy",
            message=f"matrix homeserver returned HTTP {code}",
            details={**details, "endpoint": endpoint, "http_status": code},
            updated_at=_now(),
        )
    except urllib.error.HTTPError as exc:
        return ComponentHealth(
            healthiness="unhealthy",
            message=f"matrix homeserver returned HTTP {exc.code}",
            details={**details, "endpoint": endpoint, "http_status": exc.code},
            updated_at=_now(),
        )
    except Exception as exc:
        return ComponentHealth(
            healthiness="unhealthy",
            message=f"matrix homeserver is unreachable: {exc}",
            details={
                **details,
                "endpoint": endpoint,
                "error_type": type(exc).__name__,
            },
            updated_at=_now(),
        )


def _active_model_provider(
    openclaw_cfg: dict[str, Any],
) -> tuple[str, str, dict[str, Any]] | None:
    providers = openclaw_cfg.get("models", {}).get("providers", {})
    if not isinstance(providers, dict) or not providers:
        return None

    primary = (
        openclaw_cfg.get("agents", {})
        .get("defaults", {})
        .get("model", {})
        .get("primary", "")
    )
    if isinstance(primary, str) and "/" in primary:
        provider_id, model_id = primary.split("/", 1)
        provider_cfg = providers.get(provider_id)
        if isinstance(provider_cfg, dict):
            return provider_id, model_id, provider_cfg

    for provider_id, provider_cfg in providers.items():
        if not isinstance(provider_cfg, dict):
            continue
        models = provider_cfg.get("models") or []
        for model in models:
            if isinstance(model, dict) and model.get("id"):
                return str(provider_id), str(model["id"]), provider_cfg
    return None


_ENABLE_THINKING_MODELS = {"qwen", "qwq", "deepseek"}


def _supports_enable_thinking(model_id: str) -> bool:
    lower = model_id.lower()
    return any(lower.startswith(prefix) for prefix in _ENABLE_THINKING_MODELS)


def _max_tokens_param(model_id: str) -> str:
    if model_id.startswith("gpt-5"):
        return "max_completion_tokens"
    return "max_tokens"


def _now() -> str:
    return datetime.now(timezone.utc).isoformat()
