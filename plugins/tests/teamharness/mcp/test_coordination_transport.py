import importlib.util
import io
import json
from pathlib import Path
import sys
from unittest.mock import Mock

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[3] / "teamharness"))
from mcp.transport import coordination


def test_default_transport_is_unchanged(monkeypatch):
    monkeypatch.delenv("TEAMHARNESS_COORDINATION_URL", raising=False)
    assert coordination.deliver_event("task_assigned", task_id="t") is None


def test_event_uses_runtime_identity_and_scoped_credentials(tmp_path, monkeypatch):
    token = tmp_path / "token"
    token.write_text("private-token")
    monkeypatch.setenv("TEAMHARNESS_COORDINATION_URL", "http://coordination:8099")
    monkeypatch.setenv("AGENTTEAMS_AUTH_TOKEN_FILE", str(token))
    monkeypatch.setattr(coordination, "_matrix_user_id", lambda: "@actor:test")
    captured = []
    def receive(request, **kwargs):
        captured.append(request)
        return io.BytesIO(json.dumps({"accepted": True, "event_id": "evt-1"}).encode())
    monkeypatch.setattr(coordination.urllib.request, "urlopen", receive)
    result = coordination.deliver_event("task_assigned", task_id="t", recipient="@worker:test")
    assert result["sent"] and result["deliveryId"] == "evt-1"
    assert json.loads(captured[0].data)["actor"] == "@actor:test"
    assert captured[0].get_header("Authorization") == "Bearer private-token"


def test_transport_failure_never_claims_delivery(tmp_path, monkeypatch):
    monkeypatch.setenv("TEAMHARNESS_COORDINATION_URL", "http://coordination:8099")
    monkeypatch.setenv("AGENTTEAMS_AUTH_TOKEN_FILE", str(tmp_path / "missing"))
    assert coordination.deliver_event("task_submitted", task_id="t")["sent"] is False


@pytest.mark.parametrize("serialized", [False, True])
def test_task_handoff_uses_controller_without_opt_in_transport(tmp_path, monkeypatch, serialized):
    monkeypatch.delenv("TEAMHARNESS_COORDINATION_URL", raising=False)
    monkeypatch.setenv("AGENTTEAMS_CONTROLLER_URL", "http://controller:8090")
    token = tmp_path / "token"
    token.write_text("manager-token")
    monkeypatch.setenv("AGENTTEAMS_AUTH_TOKEN_FILE", str(token))
    captured = []

    def receive(request, **kwargs):
        captured.append(request)
        return io.BytesIO(b'{"accepted":true,"event_id":"handoff-1"}')

    monkeypatch.setattr(coordination.urllib.request, "urlopen", receive)
    tasks = [{"taskId":"main"}]
    result = coordination.deliver_event("handoff", recipient="@leader:test", tasks=json.dumps(tasks) if serialized else tasks)
    assert result["sent"]
    assert captured[0].full_url == "http://controller:8090/api/v1/coordination/events"
    assert json.loads(captured[0].data)["tasks"] == [{"taskId":"main"}]


def test_handoff_schema_declares_task_contract():
    from mcp.common.tool_schemas import TOOL_SCHEMAS
    schema = TOOL_SCHEMAS["message"]["inputSchema"]["properties"]["tasks"]
    assert schema["type"] == "array"
    assert set(schema["items"]["required"]) == {"taskId", "title", "spec"}


def test_invalid_handoff_tasks_fail_before_http(monkeypatch):
    receive = Mock()
    monkeypatch.setattr(coordination.urllib.request, "urlopen", receive)
    result = coordination.deliver_event("handoff", tasks="not JSON")
    assert not result["sent"] and "tasks" in result["error"]
    receive.assert_not_called()


@pytest.mark.parametrize("kind", ["task_assigned", "task_submitted"])
def test_self_execution_uses_controller_without_opt_in(tmp_path, monkeypatch, kind):
    monkeypatch.delenv("TEAMHARNESS_COORDINATION_URL", raising=False)
    monkeypatch.setenv("AGENTTEAMS_CONTROLLER_URL", "http://controller:8090")
    token = tmp_path / "token"
    token.write_text("leader-token")
    monkeypatch.setenv("AGENTTEAMS_AUTH_TOKEN_FILE", str(token))
    monkeypatch.setattr(coordination, "_matrix_user_id", lambda: "@leader:test")
    captured = []
    def receive(request, **kwargs):
        captured.append(request)
        return io.BytesIO(b'{"accepted":true,"event_id":"self-1"}')
    monkeypatch.setattr(coordination.urllib.request, "urlopen", receive)
    result = coordination.deliver_event(kind, recipient="@leader:test", task_id="t")
    assert result["sent"]
    assert captured[0].full_url == "http://controller:8090/api/v1/coordination/events"


@pytest.mark.parametrize("kind", ["task_assigned", "task_submitted"])
def test_missing_identities_do_not_enable_self_execution_transport(monkeypatch, kind):
    monkeypatch.delenv("TEAMHARNESS_COORDINATION_URL", raising=False)
    monkeypatch.setenv("AGENTTEAMS_CONTROLLER_URL", "http://controller:8090")
    monkeypatch.setattr(coordination, "_matrix_user_id", lambda: "")
    assert coordination.deliver_event(kind, recipient="", task_id="t") is None

@pytest.mark.parametrize("kind", ["task_assigned", "task_submitted"])
def test_main_task_peer_events_use_controller_without_opt_in(tmp_path, monkeypatch, kind):
    monkeypatch.delenv("TEAMHARNESS_COORDINATION_URL", raising=False)
    monkeypatch.setenv("AGENTTEAMS_CONTROLLER_URL", "http://controller:8090")
    token = tmp_path / "token"
    token.write_text("scoped-token")
    monkeypatch.setenv("AGENTTEAMS_AUTH_TOKEN_FILE", str(token))
    monkeypatch.setattr(coordination, "_matrix_user_id", lambda: "@actor:test")
    receive = Mock(return_value=io.BytesIO(b'{"accepted":true,"event_id":"peer-1"}'))
    monkeypatch.setattr(coordination.urllib.request, "urlopen", receive)
    result = coordination.deliver_event(kind, main_task_id="main", recipient="@peer:test", task_id="child")
    assert result["sent"]
    assert json.loads(receive.call_args.args[0].data)["main_task_id"] == "main"
