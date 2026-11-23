"""The Manager's narrow schema accepts QwenPaw's runtime route metadata."""

from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[3] / "teamharness/mcp"))
import server


def test_manager_handoff_accepts_runtime_injected_fields(monkeypatch):
    monkeypatch.setattr(server, "_runtime_role", lambda: "manager")
    schema = server._tool_schema("message")["inputSchema"]
    injected = {"originEventId", "requesterReplyRoute", "coordinatorReplyRoute"}
    assert injected <= schema["properties"].keys()
    assert schema["additionalProperties"] is False
    assert "sender" not in schema["properties"]
