import json
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
    toggle_conflicts = 0

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
            self._reply(200, {"version": "2.2.0"})
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
        self._reply(404, {"detail": "missing"})

    def do_PUT(self):
        payload = self._payload()
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

    def do_POST(self):
        payload = self._payload()
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
    _ApiHandler.toggle_conflicts = 0
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


def test_require_version_rejects_unexpected_qwenpaw(api_url):
    client = QwenPawApiClient(api_url)

    with pytest.raises(QwenPawApiError, match="expected QwenPaw 2.0.0"):
        client.require_version("2.0.0")


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
