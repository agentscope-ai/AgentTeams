"""Small synchronous client for QwenPaw's localhost management API."""

from __future__ import annotations

import json
import time
from typing import Any, Iterable, Optional
import urllib.error
import urllib.parse
import urllib.request


class QwenPawApiError(RuntimeError):
    """Raised when QwenPaw rejects or fails to persist desired state."""


class QwenPawApiClient:
    def __init__(self, base_url: str, timeout: float = 10) -> None:
        self.base_url = base_url.rstrip("/")
        self.timeout = timeout

    def _request(
        self,
        method: str,
        path: str,
        payload: Any | None = None,
    ) -> Any:
        body = None if payload is None else json.dumps(payload).encode()
        request = urllib.request.Request(
            f"{self.base_url}{path}",
            data=body,
            method=method,
            headers={"Content-Type": "application/json"},
        )
        try:
            with urllib.request.urlopen(request, timeout=self.timeout) as response:
                raw = response.read()
        except urllib.error.HTTPError as exc:
            detail = ""
            if 500 <= exc.code < 600:
                try:
                    body = exc.read()
                except OSError:
                    body = b""
                finally:
                    exc.close()
                if body:
                    detail = (
                        f": {body.decode('utf-8', errors='replace').strip()}"
                    )
            raise QwenPawApiError(
                f"QwenPaw API {method} {path} failed with HTTP {exc.code}{detail}",
            ) from exc
        except (urllib.error.URLError, TimeoutError, OSError) as exc:
            raise QwenPawApiError(
                f"QwenPaw API {method} {path} unavailable: {type(exc).__name__}",
            ) from exc
        if not raw:
            return None
        try:
            return json.loads(raw)
        except json.JSONDecodeError as exc:
            raise QwenPawApiError(
                f"QwenPaw API {method} {path} returned invalid JSON",
            ) from exc

    def get_version(self) -> str:
        payload = self._request("GET", "/api/version")
        version = str((payload or {}).get("version") or "").strip()
        if not version:
            raise QwenPawApiError("QwenPaw API did not return a version")
        return version

    def require_version(self, expected: str) -> None:
        actual = self.get_version()
        if actual != expected:
            raise QwenPawApiError(
                f"expected QwenPaw {expected}, API reported {actual}",
            )

    def get_channel(self, channel: str) -> dict[str, Any]:
        try:
            return self._request(
                "GET",
                f"/api/config/channels/{urllib.parse.quote(channel, safe='')}",
            )
        except QwenPawApiError as exc:
            if "HTTP 404" in str(exc):
                return {}
            raise

    def put_channel(
        self,
        channel: str,
        desired: dict[str, Any],
        *,
        secret_fields: Iterable[str] = (),
    ) -> dict[str, Any]:
        current = self.get_channel(channel)
        payload = dict(desired)
        secret_fields = set(secret_fields)
        for field in secret_fields:
            if not payload.get(field) and current.get(field):
                payload[field] = current[field]
        path = f"/api/config/channels/{urllib.parse.quote(channel, safe='')}"
        self._request("PUT", path, payload)
        actual = self.get_channel(channel)
        mismatched = sorted(
            key
            for key, value in payload.items()
            if key not in secret_fields and actual.get(key) != value
        )
        if mismatched:
            raise QwenPawApiError(
                f"QwenPaw channel {channel} readback mismatch: {', '.join(mismatched)}",
            )
        return actual

    def get_acl(self, channel: str) -> dict[str, Any]:
        return self._request(
            "GET",
            f"/api/access-control/{urllib.parse.quote(channel, safe='')}",
        )

    def reconcile_acl(
        self,
        channel: str,
        whitelist: Iterable[str],
        blacklist: Iterable[str],
    ) -> dict[str, Any]:
        desired_white = set(whitelist)
        desired_black = set(blacklist)
        current = self.get_acl(channel)
        current_white = set((current.get("whitelist") or {}).keys())
        current_black = set((current.get("blacklist") or {}).keys())

        self._acl_action(
            "/api/access-control/whitelist/remove",
            channel,
            current_white - desired_white,
        )
        self._acl_action(
            "/api/access-control/blacklist/remove",
            channel,
            current_black - desired_black,
        )
        self._acl_action(
            "/api/access-control/whitelist/add",
            channel,
            desired_white - current_white,
        )
        self._acl_action(
            "/api/access-control/blacklist/add",
            channel,
            desired_black - current_black,
        )

        actual = self.get_acl(channel)
        if set((actual.get("whitelist") or {}).keys()) != desired_white:
            raise QwenPawApiError(
                f"QwenPaw ACL {channel} whitelist readback mismatch",
            )
        if set((actual.get("blacklist") or {}).keys()) != desired_black:
            raise QwenPawApiError(
                f"QwenPaw ACL {channel} blacklist readback mismatch",
            )
        return actual

    def _acl_action(
        self,
        path: str,
        channel: str,
        user_ids: Iterable[str],
    ) -> None:
        entries = [
            {"channel": channel, "user_id": user_id}
            for user_id in sorted(user_ids)
        ]
        if entries:
            self._request("POST", path, {"entries": entries})

    def list_mcp(self) -> list[dict[str, Any]]:
        return self._request("GET", "/api/mcp")

    def get_mcp(self, client_key: str) -> dict[str, Any]:
        return self._request(
            "GET",
            f"/api/mcp/{urllib.parse.quote(client_key, safe='')}",
        )

    def create_mcp(
        self,
        client_key: str,
        client: dict[str, Any],
    ) -> dict[str, Any]:
        self._request(
            "POST",
            "/api/mcp",
            {"client_key": client_key, "client": client},
        )
        return self._verify_mcp(client_key, client)

    def update_mcp(
        self,
        client_key: str,
        updates: dict[str, Any],
    ) -> dict[str, Any]:
        self._request(
            "PUT",
            f"/api/mcp/{urllib.parse.quote(client_key, safe='')}",
            updates,
        )
        return self._verify_mcp(client_key, updates)

    def _verify_mcp(
        self,
        client_key: str,
        desired: dict[str, Any],
    ) -> dict[str, Any]:
        actual = self.get_mcp(client_key)
        observable = {
            "name",
            "description",
            "enabled",
            "transport",
            "url",
            "command",
            "args",
            "cwd",
            "tools",
        }
        mismatched = sorted(
            key
            for key in observable & desired.keys()
            if actual.get(key) != desired.get(key)
        )
        if mismatched:
            raise QwenPawApiError(
                f"QwenPaw MCP {client_key} readback mismatch: {', '.join(mismatched)}",
            )
        return actual

    def delete_mcp(self, client_key: str) -> None:
        self._request(
            "DELETE",
            f"/api/mcp/{urllib.parse.quote(client_key, safe='')}",
        )
        if any(item.get("key") == client_key for item in self.list_mcp()):
            raise QwenPawApiError(
                f"QwenPaw MCP {client_key} delete readback mismatch",
            )

    def get_mcp_policy(self, client_key: str) -> dict[str, Any]:
        return self._request(
            "GET",
            f"/api/mcp/policy/{urllib.parse.quote(client_key, safe='')}",
        )

    def list_mcp_tools(self, client_key: str) -> list[dict[str, Any]]:
        return self._request(
            "GET",
            f"/api/mcp/tools/{urllib.parse.quote(client_key, safe='')}",
        )

    def wait_for_mcp_tools(
        self,
        client_key: str,
        *,
        # Startup window: driver activation is observed up to ~45s on a
        # loaded runner; the previous 30s default crashed worker startup.
        timeout: float = 120,
        interval: float = 0.5,
    ) -> list[dict[str, Any]]:
        deadline = time.monotonic() + timeout
        last_error: Exception | None = None
        while time.monotonic() < deadline:
            try:
                tools = self.list_mcp_tools(client_key)
                if tools:
                    return tools
            except QwenPawApiError as exc:
                last_error = exc
            time.sleep(interval)
        raise QwenPawApiError(
            f"QwenPaw MCP client {client_key} did not become callable: "
            f"{last_error or 'no tools returned'}",
        )

    def put_mcp_policy(
        self,
        client_key: str,
        desired: dict[str, Any],
    ) -> dict[str, Any]:
        self._request(
            "PUT",
            f"/api/mcp/policy/{urllib.parse.quote(client_key, safe='')}",
            desired,
        )
        actual = self.get_mcp_policy(client_key)
        mismatched = sorted(
            key for key, value in desired.items() if actual.get(key) != value
        )
        if mismatched:
            raise QwenPawApiError(
                f"QwenPaw MCP policy {client_key} readback mismatch: "
                f"{', '.join(mismatched)}",
            )
        return actual

    def configure_active_model(
        self,
        provider_id: str,
        model: str,
        *,
        base_url: str = "",
        api_key: str = "",
        provider_name: str = "",
        chat_model: str = "OpenAIChatModel",
        supports_image: Optional[bool] = None,
        supports_video: Optional[bool] = None,
        supports_multimodal: Optional[bool] = None,
        probe_source: Optional[str] = None,
    ) -> dict[str, Any]:
        providers = self._request("GET", "/api/models")
        provider = next(
            (item for item in providers if item.get("id") == provider_id),
            None,
        )
        model_payload: dict[str, Any] = {"id": model, "name": model}
        if supports_image is not None:
            model_payload["supports_image"] = supports_image
        if supports_video is not None:
            model_payload["supports_video"] = supports_video
        if supports_multimodal is not None:
            model_payload["supports_multimodal"] = supports_multimodal
        if probe_source is not None:
            model_payload["probe_source"] = probe_source
        if provider is None:
            self._request(
                "POST",
                "/api/models/custom-providers",
                {
                    "id": provider_id,
                    "name": provider_name or provider_id,
                    "default_base_url": base_url,
                    "chat_model": chat_model,
                    "models": [model_payload],
                },
            )
        else:
            known_models = {
                str(item.get("id"))
                for item in list(provider.get("models") or [])
                + list(provider.get("extra_models") or [])
            }
            if model not in known_models:
                self._request(
                    "POST",
                    f"/api/models/{urllib.parse.quote(provider_id, safe='')}/models",
                    model_payload,
                )
            else:
                # An already-registered entry keeps whatever capability
                # state it was stored with (often an earlier self-probe
                # result); the app exposes no in-place update for those
                # fields, so converge them only when the stored values
                # actually drift from the desired ones.
                self._reconcile_existing_model(
                    provider_id,
                    provider,
                    model,
                    model_payload,
                )
        config_payload: dict[str, Any] = {"chat_model": chat_model}
        if api_key:
            config_payload["api_key"] = api_key
        if base_url:
            config_payload["base_url"] = base_url
        self._request(
            "PUT",
            f"/api/models/{urllib.parse.quote(provider_id, safe='')}/config",
            config_payload,
        )
        self._request(
            "PUT",
            "/api/models/active",
            {
                "provider_id": provider_id,
                "model": model,
                "scope": "agent",
                "agent_id": "default",
            },
        )
        actual = self._request(
            "GET",
            "/api/models/active?scope=agent&agent_id=default",
        )
        active = (actual or {}).get("active_llm") or {}
        if active.get("provider_id") != provider_id or active.get("model") != model:
            raise QwenPawApiError("QwenPaw active model readback mismatch")
        providers = self._request("GET", "/api/models")
        provider = next(
            (item for item in providers if item.get("id") == provider_id),
            None,
        )
        if provider is None:
            raise QwenPawApiError("QwenPaw model provider readback mismatch")
        known_models = {
            str(item.get("id"))
            for item in list(provider.get("models") or [])
            + list(provider.get("extra_models") or [])
        }
        if model not in known_models:
            raise QwenPawApiError("QwenPaw provider model readback mismatch")
        if base_url and str(provider.get("base_url") or "").rstrip("/") != base_url.rstrip("/"):
            raise QwenPawApiError("QwenPaw provider base URL readback mismatch")
        stored = self._find_model_entry(provider, model)
        if stored is not None:
            mismatched = sorted(
                key
                for key in (
                    "supports_image",
                    "supports_video",
                    "supports_multimodal",
                    "probe_source",
                )
                if key in model_payload and stored.get(key) != model_payload[key]
            )
            if mismatched:
                raise QwenPawApiError(
                    f"QwenPaw model capability readback mismatch: "
                    f"{', '.join(mismatched)}"
                )
        return actual

    @staticmethod
    def _find_model_entry(
        provider: dict[str, Any],
        model: str,
    ) -> Optional[dict[str, Any]]:
        """Find the stored entry for ``model`` across ``models`` and
        ``extra_models`` (the two lists a provider persists user models
        in)."""
        return next(
            (
                item
                for item in list(provider.get("models") or [])
                + list(provider.get("extra_models") or [])
                if str(item.get("id")) == model
            ),
            None,
        )

    def _reconcile_existing_model(
        self,
        provider_id: str,
        provider: dict[str, Any],
        model: str,
        model_payload: dict[str, Any],
    ) -> None:
        """Converge capability fields on an already-registered model entry.

        The pinned QwenPaw app has no endpoint that updates an existing
        model entry's capability fields: ``POST /api/models/{pid}/models``
        rejects a duplicate id, and ``PUT .../models/{mid}/config`` only
        carries generation parameters. The only available convergence is
        delete + re-add, so it is used sparingly — only when a desired
        capability value actually differs from the stored one. Steady
        state stays a no-op, so repeated worker updates do not churn the
        entry.

        Failure safety: the re-add is wrapped in a compensating restore —
        if it fails after the delete, the original entry is re-posted and
        verified, so the Worker never ends up without its previously
        usable model; a failed restore raises an actionable error.
        """
        desired = {
            key: model_payload[key]
            for key in (
                "supports_image",
                "supports_video",
                "supports_multimodal",
                "probe_source",
            )
            if key in model_payload
        }
        if not desired:
            return
        entry = self._find_model_entry(provider, model)
        if entry is None or all(
            entry.get(key) == value for key, value in desired.items()
        ):
            return
        base = f"/api/models/{urllib.parse.quote(provider_id, safe='')}/models"
        self._request("DELETE", f"{base}/{urllib.parse.quote(model, safe='')}")
        readd = dict(model_payload)
        if entry.get("name"):
            # Re-add must not downgrade a human-readable display name to
            # the bare model id.
            readd["name"] = entry["name"]
        try:
            self._request("POST", base, readd)
        except QwenPawApiError as exc:
            # The entry is already deleted; a failed re-add must not leave
            # the Worker without its previously usable model.
            self._restore_model_entry(provider_id, base, entry, exc)

    def _restore_model_entry(
        self,
        provider_id: str,
        base: str,
        entry: dict[str, Any],
        readd_error: QwenPawApiError,
    ) -> None:
        """Compensating restore after a failed delete + re-add.

        Re-posts the original stored entry and verifies it in the
        response, so the previously usable model comes back; the next
        update cycle retries the convergence from the restored state.
        Always raises: the re-add error (with the restore confirmed) when
        recovery succeeded, or an actionable error when it did not.
        """
        model_id = str(entry.get("id") or "")
        restore = dict(entry)
        if not restore.get("name"):
            # The add endpoint requires a name; a malformed stored entry
            # must not block the restore.
            restore["name"] = model_id or "model"
        try:
            result = self._request("POST", base, restore)
        except QwenPawApiError as restore_error:
            raise QwenPawApiError(
                "QwenPaw model re-add failed "
                f"({readd_error}) and the compensating restore of the "
                f"original entry also failed ({restore_error}); model "
                f"'{model_id}' is no longer registered on provider "
                f"'{provider_id}' - re-run the worker update (or wait "
                "for the next reconcile) to re-register it"
            ) from readd_error
        if not isinstance(result, dict) or self._find_model_entry(
            result, model_id
        ) is None:
            raise QwenPawApiError(
                "QwenPaw model re-add failed "
                f"({readd_error}) and the restore response did not "
                f"confirm the entry; model '{model_id}' may be missing "
                f"from provider '{provider_id}' - re-run the worker "
                "update (or wait for the next reconcile) to re-register it"
            ) from readd_error
        raise QwenPawApiError(
            f"QwenPaw model re-add failed ({readd_error}); the original "
            "model entry was restored and verified, so the previously "
            "usable model remains available"
        ) from readd_error

    def configure_agent(
        self,
        agent_id: str,
        updates: dict[str, Any],
    ) -> dict[str, Any]:
        encoded = urllib.parse.quote(agent_id, safe="")
        current = self._request("GET", f"/api/agents/{encoded}")
        payload = {**current, **updates, "id": agent_id}
        self._request("PUT", f"/api/agents/{encoded}", payload)
        actual = self._request("GET", f"/api/agents/{encoded}")
        mismatched = [
            key for key, value in updates.items() if actual.get(key) != value
        ]
        if mismatched:
            raise QwenPawApiError(
                f"QwenPaw agent {agent_id} readback mismatch: "
                f"{', '.join(sorted(mismatched))}",
            )
        return actual

    def disable_agent_if_present(
        self,
        agent_id: str,
        *,
        retries: int = 120,
        retry_delay: float = 1.0,
    ) -> bool:
        agents = self._request("GET", "/api/agents").get("agents") or []
        current = next(
            (agent for agent in agents if agent.get("id") == agent_id),
            None,
        )
        if current is None:
            return False
        if current.get("enabled", True):
            encoded = urllib.parse.quote(agent_id, safe="")
            for attempt in range(retries + 1):
                try:
                    self._request(
                        "PATCH",
                        f"/api/agents/{encoded}/toggle",
                        {"enabled": False},
                    )
                    break
                except QwenPawApiError as exc:
                    if "HTTP 409" not in str(exc) or attempt == retries:
                        raise
                    time.sleep(retry_delay)
        agents = self._request("GET", "/api/agents").get("agents") or []
        actual = next(
            (agent for agent in agents if agent.get("id") == agent_id),
            None,
        )
        if actual is None or actual.get("enabled", True):
            raise QwenPawApiError(
                f"QwenPaw agent {agent_id} disable readback mismatch",
            )
        return True

    def sync_plugin(self, plugin_id: str) -> dict[str, Any]:
        encoded = urllib.parse.quote(plugin_id, safe="")
        return self._request("POST", f"/api/{encoded}/sync", {})

    def refresh_and_enable_skills(
        self,
        skill_names: Iterable[str],
    ) -> dict[str, Any]:
        self._request("POST", "/api/skills/refresh", {})
        names = sorted(set(skill_names))
        if not names:
            return {"results": {}}
        result = self._request("POST", "/api/skills/batch-enable", names)
        failed = sorted(
            name
            for name, value in (result.get("results") or {}).items()
            if not isinstance(value, dict) or not value.get("success")
        )
        if failed:
            raise QwenPawApiError(
                f"QwenPaw skill enable failed: {', '.join(failed)}",
            )
        return result
