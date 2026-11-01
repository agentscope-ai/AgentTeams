import json
import logging
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from threading import Thread

import pytest

from qwenpaw_worker.api import QwenPawApiClient, QwenPawApiError


class _ApiHandler(BaseHTTPRequestHandler):
    channel = {"enabled": False, "client_secret": "existing-secret"}
    channel_readback_override = None
    agents = [
        {"id": "default", "enabled": True},
        {"id": "QwenPaw_QA_Agent_0.2", "enabled": True},
    ]
    acl = {
        "whitelist": {"@old:example.com": {"remark": "keep", "username": "old"}},
        "blacklist": {},
    }
    mcp_policy = {
        "default_effect": "deny",
        "client_overrides": [],
        "tool_defaults": [],
        "tool_overrides": [],
        "unmanaged_rules_count": 0,
    }
    mcp = {}
    mcp_tools_unavailable = 0
    mcp_tools_startup_502 = 0
    toggle_conflicts = 0
    providers = {}
    active_llm = None
    model_deletes = 0
    model_post_failures = 0
    model_delete_failures = 0
    # QwenPaw 2.2.2.post1 capability semantics.  Set
    # supports_model_config to False to model QwenPaw 2.2.1: the
    # model-config endpoint exists but rejects capability fields (422),
    # and model reads return raw stored values (no template resolution).
    supports_model_config = True
    # Slice of the app's global model template store (keyed by model id):
    # on readback, a capability field without a config_override falls back
    # to the template value (user intent beats template only via
    # config_overrides, which only the model-config PUT records).
    model_templates = {
        "qwen3.6-plus": {
            "supports_image": True,
            "supports_video": True,
            "supports_multimodal": True,
        },
        "deepseek-chat": {
            "supports_image": False,
            "supports_video": False,
            "supports_multimodal": False,
        },
    }
    #: Capability fields the app's ModelConfigRequest (the in-place model
    #: config endpoint) accepts — the app's contract, echoed here.
    MODEL_CONFIG_CAPABILITY_FIELDS = (
        "supports_image",
        "supports_video",
        "supports_audio",
        "supports_tool_calling",
        "thinking_control",
        "max_input_length",
        "template_id",
        "generate_kwargs",
    )
    model_config_puts = 0

    def log_message(self, _format, *_args):
        return

    def _payload(self):
        length = int(self.headers.get("Content-Length", "0"))
        return json.loads(self.rfile.read(length) or b"{}")

    def _reply(self, status, payload):
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/api/version":
            self._reply(200, {"version": "2.2.1"})
            return
        if self.path == "/api/config/channels/agentteams_matrix":
            self._reply(
                200,
                type(self).channel_readback_override or type(self).channel,
            )
            return
        if self.path == "/api/access-control/agentteams_matrix":
            self._reply(200, type(self).acl)
            return
        if self.path == "/api/mcp/policy/teamharness":
            self._reply(200, type(self).mcp_policy)
            return
        if self.path == "/api/mcp/tools/teamharness":
            if type(self).mcp_tools_startup_502:
                type(self).mcp_tools_startup_502 -= 1
                self._reply(502, {"detail": "MCP driver not active yet"})
                return
            if type(self).mcp_tools_unavailable:
                type(self).mcp_tools_unavailable -= 1
                self._reply(503, {"detail": "driver not active yet"})
                return
            self._reply(200, [{"name": "taskflow", "enabled": True}])
            return
        if self.path == "/api/mcp":
            self._reply(200, list(type(self).mcp.values()))
            return
        if self.path.startswith("/api/mcp/"):
            key = self.path.removeprefix("/api/mcp/")
            if key in type(self).mcp:
                self._reply(200, type(self).mcp[key])
            else:
                self._reply(404, {"detail": "missing"})
            return
        if self.path == "/api/agents":
            self._reply(200, {"agents": type(self).agents})
            return
        if self.path == "/api/models":
            self._reply(
                200,
                [self._resolve_provider(p) for p in type(self).providers.values()],
            )
            return
        if self.path == "/api/models/active?scope=agent&agent_id=default":
            self._reply(200, {"active_llm": type(self).active_llm})
            return
        self._reply(404, {"detail": "missing"})

    def _resolve_provider(self, provider):
        """Render a provider the way the app resolves it for reads.

        2.2.2.post1: a capability field without a config_override falls
        back to the global template value (returns a view; the stored
        entries are not mutated).  2.2.1: raw stored values are the
        readback.
        """
        if not type(self).supports_model_config:
            return provider
        resolved = dict(provider)
        for list_name in ("models", "extra_models"):
            entries = []
            for entry in provider.get(list_name) or []:
                template = type(self).model_templates.get(str(entry.get("id")))
                if not template:
                    entries.append(entry)
                    continue
                view = dict(entry)
                overrides = set(entry.get("config_overrides") or [])
                for field, value in template.items():
                    if field not in overrides:
                        view[field] = value
                entries.append(view)
            resolved[list_name] = entries
        return resolved

    def do_PUT(self):
        payload = self._payload()
        if self.path.startswith("/api/models/"):
            rest = self.path.removeprefix("/api/models/")
            if rest.endswith("/config") and "/models/" in rest:
                self._put_model_config(
                    urllib.parse.unquote(rest.removesuffix("/config")), payload
                )
                return
            if rest.endswith("/config"):
                provider = type(self).providers.get(
                    urllib.parse.unquote(rest.removesuffix("/config"))
                )
                if provider is None:
                    self._reply(404, {"detail": "missing"})
                    return
                provider.update(payload)
                self._reply(200, provider)
                return
        if self.path == "/api/models/active":
            type(self).active_llm = payload
            self._reply(200, payload)
            return
        if self.path == "/api/config/channels/agentteams_matrix":
            type(self).channel = payload
            self._reply(200, payload)
            return
        if self.path == "/api/mcp/policy/teamharness":
            type(self).mcp_policy = {**payload, "unmanaged_rules_count": 0}
            self._reply(200, type(self).mcp_policy)
            return
        if self.path.startswith("/api/mcp/"):
            key = self.path.removeprefix("/api/mcp/")
            type(self).mcp[key] = {**type(self).mcp[key], **payload, "key": key}
            self._reply(200, type(self).mcp[key])
            return
        self._reply(404, {"detail": "missing"})

    def _put_model_config(self, rest, payload):
        # rest: "{pid}/models/{mid}" — the in-place model config endpoint
        # (the app's ModelConfigRequest).  2.2.2.post1 records the
        # accepted capability fields in the entry's config_overrides;
        # 2.2.1's endpoint carries generation parameters only and
        # rejects capability fields with 422.
        provider_id, sep, model_id = rest.rpartition("/models/")
        if not sep:
            self._reply(404, {"detail": "missing"})
            return
        provider = type(self).providers.get(provider_id)
        if provider is None:
            self._reply(404, {"detail": "missing"})
            return
        capabilities = {
            key: value
            for key, value in payload.items()
            if key in type(self).MODEL_CONFIG_CAPABILITY_FIELDS
        }
        if not type(self).supports_model_config and capabilities:
            self._reply(422, {"detail": "capability fields are not supported"})
            return
        entry = next(
            (
                entry
                for entry in list(provider.get("models") or [])
                + list(provider.get("extra_models") or [])
                if str(entry.get("id")) == model_id
            ),
            None,
        )
        if entry is None:
            self._reply(404, {"detail": "missing"})
            return
        entry.update(capabilities)
        overrides = set(entry.get("config_overrides") or [])
        overrides.update(capabilities)
        entry["config_overrides"] = sorted(overrides)
        type(self).model_config_puts += 1
        self._reply(200, provider)

    def do_POST(self):
        payload = self._payload()
        if self.path == "/api/models/custom-providers":
            pid = payload["id"]
            provider = {
                "id": pid,
                "name": payload.get("name", pid),
                "base_url": payload.get("default_base_url", ""),
                "models": payload.get("models", []),
                "extra_models": [],
            }
            type(self).providers[pid] = provider
            self._reply(201, provider)
            return
        if self.path.startswith("/api/models/") and self.path.endswith("/models"):
            provider_id = self.path.removeprefix("/api/models/").removesuffix("/models")
            provider = type(self).providers.get(provider_id)
            if provider is None:
                self._reply(404, {"detail": "missing"})
                return
            if type(self).model_post_failures:
                type(self).model_post_failures -= 1
                self._reply(500, {"detail": "injected: model add failed"})
                return
            # Real contract: the app rejects a duplicate model id instead
            # of upserting (Provider.add_model -> "already exists").
            existing = {
                str(entry.get("id"))
                for entry in provider.get("models", [])
                + provider.get("extra_models", [])
            }
            if payload.get("id") in existing:
                self._reply(
                    404,
                    {"detail": f"Model '{payload.get('id')}' already exists"},
                )
                return
            provider["extra_models"].append(payload)
            self._reply(201, provider)
            return
        actions = {
            "/api/access-control/whitelist/add": ("whitelist", True),
            "/api/access-control/whitelist/remove": ("whitelist", False),
            "/api/access-control/blacklist/add": ("blacklist", True),
            "/api/access-control/blacklist/remove": ("blacklist", False),
        }
        if self.path in actions:
            list_name, adding = actions[self.path]
            entries = type(self).acl[list_name]
            for entry in payload["entries"]:
                user_id = entry["user_id"]
                if adding:
                    entries[user_id] = {
                        "remark": entry.get("remark", ""),
                        "username": entry.get("username", ""),
                    }
                else:
                    entries.pop(user_id, None)
            self._reply(200, {"success": True})
            return
        if self.path == "/api/mcp":
            key = payload["client_key"]
            type(self).mcp[key] = {"key": key, **payload["client"]}
            self._reply(200, type(self).mcp[key])
            return
        self._reply(404, {"detail": "missing"})

    def do_DELETE(self):
        if self.path.startswith("/api/mcp/"):
            key = self.path.removeprefix("/api/mcp/")
            type(self).mcp.pop(key, None)
            self._reply(200, {"success": True})
            return
        if self.path.startswith("/api/models/"):
            # Real contract (Provider.delete_model): removes the entry from
            # models + extra_models and remembers it in removed_model_ids
            # (a later re-add succeeds).
            rest = self.path.removeprefix("/api/models/")
            provider_id, sep, model_id = rest.partition("/models/")
            provider = type(self).providers.get(urllib.parse.unquote(provider_id))
            if not sep or provider is None:
                self._reply(404, {"detail": "missing"})
                return
            if type(self).model_delete_failures:
                type(self).model_delete_failures -= 1
                self._reply(500, {"detail": "injected: model delete failed"})
                return
            model_id = urllib.parse.unquote(model_id)
            for list_name in ("models", "extra_models"):
                provider[list_name] = [
                    entry
                    for entry in provider.get(list_name, [])
                    if str(entry.get("id")) != model_id
                ]
            provider.setdefault("removed_model_ids", []).append(model_id)
            type(self).model_deletes += 1
            self._reply(200, provider)
            return
        self._reply(404, {"detail": "missing"})

    def do_PATCH(self):
        payload = self._payload()
        if self.path == "/api/agents/QwenPaw_QA_Agent_0.2/toggle":
            if type(self).toggle_conflicts:
                type(self).toggle_conflicts -= 1
                self._reply(409, {"detail": "agent is still starting"})
                return
            for agent in type(self).agents:
                if agent["id"] == "QwenPaw_QA_Agent_0.2":
                    agent["enabled"] = payload["enabled"]
            self._reply(200, {"success": True, "enabled": payload["enabled"]})
            return
        self._reply(404, {"detail": "missing"})


@pytest.fixture()
def api_url():
    _ApiHandler.channel = {"enabled": False, "client_secret": "existing-secret"}
    _ApiHandler.channel_readback_override = None
    _ApiHandler.agents = [
        {"id": "default", "enabled": True},
        {"id": "QwenPaw_QA_Agent_0.2", "enabled": True},
    ]
    _ApiHandler.acl = {
        "whitelist": {"@old:example.com": {"remark": "keep", "username": "old"}},
        "blacklist": {},
    }
    _ApiHandler.mcp_policy = {
        "default_effect": "deny",
        "client_overrides": [],
        "tool_defaults": [],
        "tool_overrides": [],
        "unmanaged_rules_count": 0,
    }
    _ApiHandler.mcp = {}
    _ApiHandler.mcp_tools_unavailable = 0
    _ApiHandler.mcp_tools_startup_502 = 0
    _ApiHandler.toggle_conflicts = 0
    _ApiHandler.providers = {}
    _ApiHandler.active_llm = None
    _ApiHandler.model_deletes = 0
    _ApiHandler.model_post_failures = 0
    _ApiHandler.model_delete_failures = 0
    _ApiHandler.supports_model_config = True
    _ApiHandler.model_templates = {
        "qwen3.6-plus": {
            "supports_image": True,
            "supports_video": True,
            "supports_multimodal": True,
        },
        "deepseek-chat": {
            "supports_image": False,
            "supports_video": False,
            "supports_multimodal": False,
        },
    }
    _ApiHandler.model_config_puts = 0
    server = ThreadingHTTPServer(("127.0.0.1", 0), _ApiHandler)
    thread = Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}"
    finally:
        server.shutdown()
        thread.join()


def test_put_channel_preserves_empty_secret_and_reads_back(api_url):
    client = QwenPawApiClient(api_url)

    result = client.put_channel(
        "agentteams_matrix",
        {"enabled": True, "client_secret": ""},
        secret_fields={"client_secret"},
    )

    assert result == {"enabled": True, "client_secret": "existing-secret"}


def test_put_channel_rejects_readback_mismatch(api_url):
    client = QwenPawApiClient(api_url)
    _ApiHandler.channel_readback_override = {"enabled": False}

    with pytest.raises(QwenPawApiError, match="readback mismatch: enabled"):
        client.put_channel("agentteams_matrix", {"enabled": True})


@pytest.mark.parametrize("minimum", ["2.0.0", "2.2.1"])
def test_require_version_accepts_compatible_qwenpaw(api_url, minimum):
    client = QwenPawApiClient(api_url)

    # Minimum-version gate: the fake app reports 2.2.1, so a minimum above
    # it must be rejected (a minimum below it passes — in-place upgrades
    # to newer qwenpaw must not be blocked).
    with pytest.raises(
        QwenPawApiError,
        match="does not satisfy the required minimum 2.3.0",
    ):
        client.require_version("2.3.0")
    client.require_version("2.2.1")
def test_http_error_and_timeout_are_safe(api_url, monkeypatch):
    client = QwenPawApiClient(api_url)

    with pytest.raises(QwenPawApiError, match="HTTP 404"):
        client.get_acl("missing")

    def timeout(*_args, **_kwargs):
        raise TimeoutError("sensitive upstream detail")

    monkeypatch.setattr("urllib.request.urlopen", timeout)
    with pytest.raises(QwenPawApiError, match="unavailable: TimeoutError") as exc:
        client.get_version()
    assert "sensitive upstream detail" not in str(exc.value)


def test_request_surfaces_5xx_response_body_in_error(api_url):
    _ApiHandler.mcp_tools_startup_502 = 1
    client = QwenPawApiClient(api_url)

    with pytest.raises(QwenPawApiError, match="MCP driver not active yet"):
        client.list_mcp_tools("teamharness")

    with pytest.raises(QwenPawApiError, match="HTTP 404") as exc:
        client.get_acl("missing")
    assert '"detail"' not in str(exc.value)


def test_acl_reconcile_parses_structured_entries_and_is_channel_scoped(api_url):
    client = QwenPawApiClient(api_url)

    result = client.reconcile_acl(
        "agentteams_matrix",
        ["@new:example.com"],
        ["@blocked:example.com"],
    )

    assert result["whitelist"] == {
        "@new:example.com": {"remark": "", "username": ""},
    }
    assert set(result["blacklist"]) == {"@blocked:example.com"}


def test_put_mcp_policy_uses_public_api_and_reads_back(api_url):
    client = QwenPawApiClient(api_url)

    result = client.put_mcp_policy(
        "teamharness",
        {
            "default_effect": "allow",
            "client_overrides": [],
            "tool_defaults": [],
            "tool_overrides": [],
        },
    )

    assert result["default_effect"] == "allow"


def test_list_mcp_tools_activates_driver_through_public_api(api_url):
    client = QwenPawApiClient(api_url)

    assert client.list_mcp_tools("teamharness") == [
        {"name": "taskflow", "enabled": True},
    ]


def test_wait_for_mcp_tools_retries_until_driver_is_active(api_url):
    _ApiHandler.mcp_tools_unavailable = 2
    client = QwenPawApiClient(api_url)

    assert client.wait_for_mcp_tools(
        "teamharness",
        timeout=1,
        interval=0.01,
    ) == [{"name": "taskflow", "enabled": True}]


def test_wait_for_mcp_tools_absorbs_slow_driver_activation(api_url, monkeypatch):
    # A loaded runner can take ~40s before the MCP driver activates;
    # simulate 2s of virtual time per poll so the default startup window
    # is exercised in milliseconds.
    _ApiHandler.mcp_tools_startup_502 = 20
    client = QwenPawApiClient(api_url)

    virtual_now = [0.0]

    def fake_monotonic():
        virtual_now[0] += 2.0
        return virtual_now[0]

    monkeypatch.setattr("qwenpaw_worker.api.time.monotonic", fake_monotonic)
    monkeypatch.setattr("qwenpaw_worker.api.time.sleep", lambda *_args: None)

    assert client.wait_for_mcp_tools("teamharness") == [
        {"name": "taskflow", "enabled": True},
    ]


def test_mcp_create_update_delete_each_reads_back(api_url):
    client = QwenPawApiClient(api_url)

    created = client.create_mcp(
        "owned",
        {"name": "owned", "enabled": True, "transport": "stdio"},
    )
    assert created["enabled"] is True
    updated = client.update_mcp("owned", {"enabled": False})
    assert updated["enabled"] is False
    client.delete_mcp("owned")
    assert client.list_mcp() == []


def test_disable_agent_retries_startup_conflict(api_url):
    client = QwenPawApiClient(api_url)
    _ApiHandler.toggle_conflicts = 2

    assert client.disable_agent_if_present(
        "QwenPaw_QA_Agent_0.2",
        retries=2,
        retry_delay=0,
    ) is True
    assert _ApiHandler.agents[1]["enabled"] is False


def test_configure_active_model_creates_provider_with_capabilities(api_url):
    """Worker startup (CI scenario): the global template for qwen3.6-plus
    says supports_video=True, so the declared supports_video=False only
    survives readback if the create path records it as a config_override
    in place — exactly what 2.2.2.post1 CI failed on."""
    client = QwenPawApiClient(api_url)

    result = client.configure_active_model(
        "agentteams-gateway",
        "qwen3.6-plus",
        base_url="http://gateway.example.com/v1",
        api_key="secret",
        provider_name="AgentTeams Gateway",
        supports_image=True,
        supports_video=False,
        supports_multimodal=True,
        probe_source="manual",
    )

    provider = _ApiHandler.providers["agentteams-gateway"]
    created = provider["models"][0]
    assert created["id"] == "qwen3.6-plus"
    assert created["supports_image"] is True
    assert created["supports_video"] is False
    assert created["supports_multimodal"] is True
    assert created["probe_source"] == "manual"
    # The declared capabilities were recorded in place; without the
    # overrides the template's supports_video=True would win on readback
    # (the 2.2.2.post1 CI failure).
    assert "supports_image" in created["config_overrides"]
    assert "supports_video" in created["config_overrides"]
    assert _ApiHandler.model_deletes == 0
    assert provider["base_url"] == "http://gateway.example.com/v1"
    assert _ApiHandler.active_llm == {
        "provider_id": "agentteams-gateway",
        "model": "qwen3.6-plus",
        "scope": "agent",
        "agent_id": "default",
    }
    assert result["active_llm"]["provider_id"] == "agentteams-gateway"


def test_configure_active_model_without_capabilities_omits_support_fields(api_url):
    client = QwenPawApiClient(api_url)

    client.configure_active_model(
        "agentteams-gateway",
        "qwen3.6-plus",
        base_url="http://gateway.example.com/v1",
        api_key="secret",
    )

    provider = _ApiHandler.providers["agentteams-gateway"]
    created = provider["models"][0]
    assert "supports_image" not in created
    assert "supports_video" not in created
    assert "supports_multimodal" not in created
    assert "probe_source" not in created
    # No capability fields to record -> no in-place config write at all.
    assert _ApiHandler.model_config_puts == 0


def test_configure_active_model_existing_provider_appends_model_with_capabilities(
    api_url,
):
    _ApiHandler.providers["agentteams-gateway"] = {
        "id": "agentteams-gateway",
        "name": "AgentTeams Gateway",
        "base_url": "http://gateway.example.com/v1",
        "models": [],
        "extra_models": [],
    }
    client = QwenPawApiClient(api_url)

    client.configure_active_model(
        "agentteams-gateway",
        "qwen3.6-plus",
        base_url="http://gateway.example.com/v1",
        api_key="secret",
        supports_image=True,
        supports_multimodal=True,
        probe_source="manual",
    )

    provider = _ApiHandler.providers["agentteams-gateway"]
    added = provider["extra_models"][0]
    assert added["id"] == "qwen3.6-plus"
    assert added["supports_image"] is True
    assert added["supports_multimodal"] is True
    assert added["probe_source"] == "manual"
    assert "supports_image" in added["config_overrides"]
    assert _ApiHandler.model_deletes == 0


def test_configure_active_model_existing_model_converges_in_place(
    api_url,
):
    """Regression: an already-registered entry whose stored capabilities
    drift from the desired ones must converge IN PLACE (2.2.2+:
    PUT model config records the values as config_overrides) — no
    delete/re-add churn, display name untouched, and the readback must
    reflect the desired values even against a disagreeing template
    (qwen3.6-plus template says supports_video=True; the entry stored
    False).

    probe_source is only writable on model add/create, so the drift from
    the stored "probed" to the desired "documentation" must be tolerated
    (logged), not a failure."""
    _ApiHandler.providers["agentteams-gateway"] = {
        "id": "agentteams-gateway",
        "name": "AgentTeams Gateway",
        "base_url": "http://gateway.example.com/v1",
        "models": [
            {
                "id": "qwen3.6-plus",
                "name": "Qwen3.6 Plus",
                "supports_image": False,
                "supports_video": False,
                "supports_multimodal": False,
                "probe_source": "probed",
            },
        ],
        "extra_models": [],
    }
    client = QwenPawApiClient(api_url)

    client.configure_active_model(
        "agentteams-gateway",
        "qwen3.6-plus",
        base_url="http://gateway.example.com/v1",
        api_key="secret",
        supports_image=True,
        supports_video=False,
        supports_multimodal=True,
        probe_source="documentation",
    )

    provider = _ApiHandler.providers["agentteams-gateway"]
    entries = provider["models"] + provider["extra_models"]
    assert [entry["id"] for entry in entries] == ["qwen3.6-plus"]
    updated = entries[0]
    # Converged in place: the stored values now carry the desired
    # capabilities, recorded as overrides that beat the template.
    assert updated["supports_image"] is True
    assert updated["supports_video"] is False
    assert "supports_image" in updated["config_overrides"]
    assert "supports_video" in updated["config_overrides"]
    # probe_source could not be rewritten in place: the original value
    # is kept (drift is logged, not raised).
    assert updated["probe_source"] == "probed"
    # The human-readable display name is untouched.
    assert updated["name"] == "Qwen3.6 Plus"
    # No delete/re-add churn.
    assert _ApiHandler.model_deletes == 0


def test_configure_active_model_existing_model_no_drift_is_noop(api_url):
    """Steady state: desired values already stored -> no delete/re-add
    churn on repeated worker updates."""
    _ApiHandler.providers["agentteams-gateway"] = {
        "id": "agentteams-gateway",
        "name": "AgentTeams Gateway",
        "base_url": "http://gateway.example.com/v1",
        "models": [
            {
                "id": "qwen3.6-plus",
                "name": "Qwen3.6 Plus",
                "supports_image": True,
                "supports_multimodal": True,
                "probe_source": "documentation",
            },
        ],
        "extra_models": [],
    }
    client = QwenPawApiClient(api_url)

    client.configure_active_model(
        "agentteams-gateway",
        "qwen3.6-plus",
        base_url="http://gateway.example.com/v1",
        api_key="secret",
        supports_image=True,
        supports_multimodal=True,
        probe_source="documentation",
    )

    assert _ApiHandler.model_deletes == 0
    assert _ApiHandler.model_config_puts == 0
    provider = _ApiHandler.providers["agentteams-gateway"]
    assert provider["models"][0]["supports_image"] is True
    assert provider["extra_models"] == []


def _seed_stale_provider() -> None:
    _ApiHandler.providers["agentteams-gateway"] = {
        "id": "agentteams-gateway",
        "name": "AgentTeams Gateway",
        "base_url": "http://gateway.example.com/v1",
        "models": [
            {
                "id": "qwen3.6-plus",
                "name": "Qwen3.6 Plus",
                "supports_image": False,
                "supports_video": False,
                "supports_multimodal": False,
                "probe_source": "probed",
            },
        ],
        "extra_models": [],
    }


def test_configure_active_model_readd_failure_restores_original_entry(api_url):
    """Regression (QwenPaw 2.2.1 fallback path): a failed re-add must not
    leave the Worker without its previously usable model — the
    compensating restore brings back the original entry.

    Pinned against 2.2.1 semantics: on 2.2.2 the in-place PUT converges
    first and the delete/re-add path is never reached."""
    _seed_stale_provider()
    _ApiHandler.supports_model_config = False
    _ApiHandler.model_post_failures = 1
    client = QwenPawApiClient(api_url)

    with pytest.raises(QwenPawApiError, match="restored and verified"):
        client.configure_active_model(
            "agentteams-gateway",
            "qwen3.6-plus",
            base_url="http://gateway.example.com/v1",
            api_key="secret",
            supports_image=True,
            supports_video=False,
            supports_multimodal=True,
            probe_source="documentation",
        )

    provider = _ApiHandler.providers["agentteams-gateway"]
    entries = provider["models"] + provider["extra_models"]
    assert [entry["id"] for entry in entries] == ["qwen3.6-plus"]
    # The restored entry keeps the ORIGINAL stored state, not the desired
    # values — convergence is retried by the next update cycle.
    assert entries[0]["supports_image"] is False
    assert entries[0]["probe_source"] == "probed"
    assert entries[0]["name"] == "Qwen3.6 Plus"
    assert _ApiHandler.model_deletes == 1


def test_configure_active_model_readd_and_restore_failure_surfaces_actionable_error(api_url):
    """When both the re-add and the compensating restore fail, the error
    must name the broken state and how to recover — and the very next
    update re-registers the missing model via the add path.

    Pinned against QwenPaw 2.2.1 semantics (see the sibling restore
    test): the delete/re-add fallback only runs when the in-place
    capability update is unavailable."""
    _seed_stale_provider()
    _ApiHandler.supports_model_config = False
    _ApiHandler.model_post_failures = 2
    client = QwenPawApiClient(api_url)

    with pytest.raises(
        QwenPawApiError,
        match=r"no longer registered.*re-run the worker update",
    ):
        client.configure_active_model(
            "agentteams-gateway",
            "qwen3.6-plus",
            base_url="http://gateway.example.com/v1",
            api_key="secret",
            supports_image=True,
            supports_video=False,
            supports_multimodal=True,
            probe_source="documentation",
        )

    provider = _ApiHandler.providers["agentteams-gateway"]
    assert provider["models"] == []
    assert provider["extra_models"] == []

    # Recovery: the next update sees the missing model and re-adds it with
    # the desired capabilities.
    _ApiHandler.model_post_failures = 0
    client.configure_active_model(
        "agentteams-gateway",
        "qwen3.6-plus",
        base_url="http://gateway.example.com/v1",
        api_key="secret",
        supports_image=True,
        supports_video=False,
        supports_multimodal=True,
        probe_source="documentation",
    )
    entries = provider["models"] + provider["extra_models"]
    assert [entry["id"] for entry in entries] == ["qwen3.6-plus"]
    assert entries[0]["supports_image"] is True
    assert entries[0]["probe_source"] == "documentation"


def test_configure_active_model_delete_failure_keeps_entry_and_raises(api_url):
    """A rejected DELETE must not trigger any restore: the entry is still
    there, so the only correct outcome is the original error.

    Pinned against QwenPaw 2.2.1 semantics (the delete/re-add fallback
    only runs when the in-place capability update is unavailable)."""
    _seed_stale_provider()
    _ApiHandler.supports_model_config = False
    _ApiHandler.model_delete_failures = 1
    client = QwenPawApiClient(api_url)

    with pytest.raises(QwenPawApiError, match="HTTP 500"):
        client.configure_active_model(
            "agentteams-gateway",
            "qwen3.6-plus",
            base_url="http://gateway.example.com/v1",
            api_key="secret",
            supports_image=True,
            supports_video=False,
            supports_multimodal=True,
            probe_source="documentation",
        )

    provider = _ApiHandler.providers["agentteams-gateway"]
    entries = provider["models"] + provider["extra_models"]
    assert [entry["id"] for entry in entries] == ["qwen3.6-plus"]
    # Untouched: still the original stored state.
    assert entries[0]["supports_image"] is False
    assert _ApiHandler.model_deletes == 0


def test_configure_active_model_video_true_override_beats_template(api_url):
    """Reverse direction of the CI failure: a desired supports_video=True
    must actually take effect on a model whose global template says
    supports_video=False (deepseek-chat) — the in-place override write
    is what makes the readback honour the declaration."""
    client = QwenPawApiClient(api_url)

    client.configure_active_model(
        "agentteams-gateway",
        "deepseek-chat",
        base_url="http://gateway.example.com/v1",
        api_key="secret",
        supports_image=False,
        supports_video=True,
        supports_multimodal=True,
        probe_source="manual",
    )

    provider = _ApiHandler.providers["agentteams-gateway"]
    created = provider["models"][0]
    # Raw stored value plus the recorded override; without the override
    # the template's supports_video=False would win on readback.
    assert created["supports_video"] is True
    assert "supports_video" in created["config_overrides"]
    assert _ApiHandler.model_deletes == 0
    assert _ApiHandler.active_llm["model"] == "deepseek-chat"


def test_configure_active_model_probe_source_drift_on_existing_model_is_not_fatal(
    api_url,
    caplog,
):
    """An existing model whose writable capabilities already match the
    desired values, but whose probe_source drifts, must be a no-op with a
    logged warning — probe_source is only writable on model add/create,
    so the drift is recorded, not a failure condition."""
    _ApiHandler.providers["agentteams-gateway"] = {
        "id": "agentteams-gateway",
        "name": "AgentTeams Gateway",
        "base_url": "http://gateway.example.com/v1",
        "models": [
            {
                "id": "qwen3.6-plus",
                "name": "Qwen3.6 Plus",
                "supports_image": True,
                "supports_video": False,
                "supports_multimodal": True,
                "probe_source": "probed",
                "config_overrides": ["supports_image", "supports_video"],
            },
        ],
        "extra_models": [],
    }
    client = QwenPawApiClient(api_url)

    with caplog.at_level(logging.WARNING, logger="qwenpaw_worker.api"):
        client.configure_active_model(
            "agentteams-gateway",
            "qwen3.6-plus",
            base_url="http://gateway.example.com/v1",
            api_key="secret",
            supports_image=True,
            supports_video=False,
            supports_multimodal=True,
            probe_source="manual",
        )

    assert _ApiHandler.model_deletes == 0
    assert _ApiHandler.model_config_puts == 0
    entry = _ApiHandler.providers["agentteams-gateway"]["models"][0]
    assert entry["probe_source"] == "probed"
    assert any("kept probe_source" in r.message for r in caplog.records)


def test_configure_active_model_2_2_1_fallback_converges_via_readd(api_url):
    """Control (QwenPaw 2.2.1, NOT a production regression): the app's
    model-config endpoint rejects capability fields (422), so the client
    falls back to delete + re-add — which is self-consistent on 2.2.1
    because raw stored values are the readback (no template resolution).
    The run must end converged, with no exception."""
    _seed_stale_provider()
    _ApiHandler.supports_model_config = False
    client = QwenPawApiClient(api_url)

    client.configure_active_model(
        "agentteams-gateway",
        "qwen3.6-plus",
        base_url="http://gateway.example.com/v1",
        api_key="secret",
        supports_image=True,
        supports_video=False,
        supports_multimodal=True,
        probe_source="documentation",
    )

    provider = _ApiHandler.providers["agentteams-gateway"]
    entries = provider["models"] + provider["extra_models"]
    assert [entry["id"] for entry in entries] == ["qwen3.6-plus"]
    updated = entries[0]
    # Eventual consistency: the re-add carries the desired capabilities
    # and the display name survived.
    assert updated["supports_image"] is True
    assert updated["supports_video"] is False
    assert updated["supports_multimodal"] is True
    assert updated["probe_source"] == "documentation"
    assert updated["name"] == "Qwen3.6 Plus"
    assert _ApiHandler.model_deletes == 1
    assert _ApiHandler.model_config_puts == 0


def test_configure_active_model_manager_style_registration_converges_in_place(
    api_url,
):
    """Manager startup: the QwenPaw Manager registration (bridge
    providers.json: modality-derived capabilities, no probe_source, no
    overrides) must converge through the same client contract the Worker
    uses — in place, without delete/re-add churn and without a
    probe_source failure (the bridge never writes that field)."""
    _ApiHandler.providers["agentteams-gateway"] = {
        "id": "agentteams-gateway",
        "name": "AgentTeams Gateway",
        "base_url": "http://gateway.example.com/v1",
        "models": [
            {
                "id": "qwen3.6-plus",
                "name": "Qwen3.6 Plus",
                "supports_image": True,
                "supports_video": False,
                "supports_multimodal": True,
            },
        ],
        "extra_models": [],
    }
    client = QwenPawApiClient(api_url)

    client.configure_active_model(
        "agentteams-gateway",
        "qwen3.6-plus",
        base_url="http://gateway.example.com/v1",
        api_key="secret",
        supports_image=True,
        supports_video=False,
        supports_multimodal=True,
        probe_source="manual",
    )

    entry = _ApiHandler.providers["agentteams-gateway"]["models"][0]
    # Converged in place: the override records the declaration so the
    # template's supports_video=True cannot win on readback.
    assert entry["supports_video"] is False
    assert "supports_video" in entry["config_overrides"]
    assert _ApiHandler.model_deletes == 0
    assert _ApiHandler.active_llm["model"] == "qwen3.6-plus"


@pytest.mark.parametrize("status", [None, "pending", "starting", "failed", "disabled"])
def test_require_agent_running_rejects_unready_agent(api_url, status):
    client = QwenPawApiClient(api_url)
    _ApiHandler.agents = [{"id": "default", "startup_status": status}]
    with pytest.raises(QwenPawApiError, match="is not running"):
        client.require_agent_running("default")


def test_require_agent_running_accepts_running_agent(api_url):
    client = QwenPawApiClient(api_url)
    _ApiHandler.agents = [{"id": "default", "startup_status": "running"}]
    client.require_agent_running("default")
