"""Agent handoffs resolve a sender-owned room, not another user's personal room."""
import io
import json
from pathlib import Path
import sys
import urllib.error

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[3] / "teamharness"))
from mcp import message_tool


class Response:
    def __init__(self, data): self.data = data
    def __enter__(self): return self
    def __exit__(self, *args): return False
    def read(self): return json.dumps(self.data).encode()


@pytest.mark.parametrize("exists", [True, False])
def test_direct_room_is_reused_or_created_with_only_the_recipient(monkeypatch, exists):
    calls = []
    def send(request, timeout):
        calls.append(request)
        if request.full_url.endswith("/account/whoami"):
            return Response({"user_id": "@manager:test"})
        if "/directory/room/" in request.full_url:
            if exists: return Response({"room_id": "!pair:test"})
            raise urllib.error.HTTPError(request.full_url, 404, "missing", {}, io.BytesIO())
        if request.full_url.endswith("/joined_members"):
            return Response({"joined": {"@manager:test": {}, "@leader:test": {}}})
        assert request.full_url.endswith("/createRoom")
        body = json.loads(request.data)
        assert body["invite"] == ["@leader:test"]
        assert body["is_direct"] is True and body["preset"] == "private_chat"
        return Response({"room_id": "!pair:test"})
    monkeypatch.setattr(message_tool.urllib.request, "urlopen", send)
    assert message_tool._direct_message_room("http://matrix", "token", "@leader:test") == "!pair:test"
    assert len(calls) == 3


def test_existing_room_with_another_member_is_not_used(monkeypatch):
    def send(request, timeout):
        if request.full_url.endswith("/account/whoami"):
            return Response({"user_id": "@manager:test"})
        if "/directory/room/" in request.full_url:
            return Response({"room_id": "!pair:test"})
        return Response({"joined": {"@manager:test": {}, "@leader:test": {}, "@other:test": {}}})
    monkeypatch.setattr(message_tool.urllib.request, "urlopen", send)
    with pytest.raises(ValueError, match="membership"):
        message_tool._direct_message_room("http://matrix", "token", "@leader:test")


def test_permission_failure_is_not_treated_as_absent_room(monkeypatch):
    def send(request, timeout):
        if request.full_url.endswith("/account/whoami"):
            return Response({"user_id": "@manager:test"})
        raise urllib.error.HTTPError(request.full_url, 403, "denied", {}, io.BytesIO())
    monkeypatch.setattr(message_tool.urllib.request, "urlopen", send)
    with pytest.raises(urllib.error.HTTPError):
        message_tool._direct_message_room("http://matrix", "token", "@leader:test")
