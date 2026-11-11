"""A trusted Worker must not acknowledge or submit another member's task."""
import json
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[3] / "teamharness" / "mcp"))
import server


@pytest.mark.parametrize("action", ["ack_task", "submit_task", "report_progress"])
@pytest.mark.parametrize("assigned_to", ["alice", "@alice:example.test"])
@pytest.mark.parametrize("actor,allowed", [("bob", False), ("alice", True)])
def test_worker_can_write_only_the_assigned_task(tmp_path, monkeypatch, action, assigned_to, actor, allowed):
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "worker")
    monkeypatch.setenv("AGENTTEAMS_WORKER_NAME", actor)
    monkeypatch.setattr(server, "_pull_project", lambda *a, **kw: True)
    monkeypatch.setattr(server, "_pull_task", lambda *a, **kw: True)
    monkeypatch.setattr(server, "_sync_task", lambda *a, **kw: True)
    monkeypatch.setattr(server, "_update_project_task", lambda *a, **kw: True)
    monkeypatch.setattr(server, "_publish_task_artifacts", lambda *a, **kw: [])
    monkeypatch.setattr(server, "_task_completion_notification", lambda *a, **kw: {"sent": True})
    task_path = tmp_path / "shared/tasks/p1-01/meta.json"
    server._write_json(task_path, {"task_id": "p1-01", "project_id": "p1",
                                 "status": "assigned", "assigned_to": assigned_to})
    before = task_path.read_bytes()

    result = server.call_tool("taskflow", {"action": action, "workspaceDir": str(tmp_path),
        "payload": {"taskId": "p1-01", "summary": "Not my task", "note": "half"}})
    data = json.loads(result["content"][0]["text"])

    assert data["ok"] is allowed
    if not allowed:
        assert "assigned to" in data["error"]
        assert task_path.read_bytes() == before
