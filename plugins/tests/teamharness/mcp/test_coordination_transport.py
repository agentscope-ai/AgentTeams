import importlib.util
import io
import json
from pathlib import Path
import sys

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


def test_task_handoff_uses_controller_without_opt_in_transport(tmp_path, monkeypatch):
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
    result = coordination.deliver_event("handoff", recipient="@leader:test", tasks=[{"taskId":"main"}])
    assert result["sent"]
    assert captured[0].full_url == "http://controller:8090/api/v1/coordination/events"
    assert json.loads(captured[0].data)["tasks"] == [{"taskId":"main"}]


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
