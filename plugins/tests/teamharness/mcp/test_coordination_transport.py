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
