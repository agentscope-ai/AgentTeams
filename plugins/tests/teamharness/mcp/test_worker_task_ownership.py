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


@pytest.mark.parametrize("status", ["planned", "prepared", "assigned"])
@pytest.mark.parametrize("runtime_role", ["worker", "remote-member"])
def test_controller_submission_requires_ack_before_any_mutation(tmp_path, monkeypatch, status, runtime_role):
    monkeypatch.setenv("AGENTTEAMS_CONTROLLER_URL", "http://controller.example.test")
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", runtime_role)
    monkeypatch.setenv("AGENTTEAMS_WORKER_NAME", "alice")
    monkeypatch.setattr(server, "_pull_project", lambda *a, **kw: True)
    monkeypatch.setattr(server, "_pull_task", lambda *a, **kw: True)

    def unexpected_write(*a, **kw):
        pytest.fail("submission must not write before a durable ACK")

    monkeypatch.setattr(server, "_sync_task", unexpected_write)
    monkeypatch.setattr(server, "_update_project_task", unexpected_write)
    monkeypatch.setattr(server, "_publish_task_artifacts", unexpected_write)
    task_path = tmp_path / "shared/tasks/p1-01/meta.json"
    server._write_json(task_path, {"task_id": "p1-01", "project_id": "p1",
                                 "status": status, "assigned_to": "alice"})
    before = task_path.read_bytes()
    response = server.call_tool("taskflow", {"action": "submit_task", "workspaceDir": str(tmp_path),
        "payload": {"taskId": "p1-01", "status": "SUCCESS", "summary": "Done"}})
    data = json.loads(response["content"][0]["text"])
    assert data["ok"] is False
    assert "ack_task" in json.dumps(data)
    assert task_path.read_bytes() == before


@pytest.mark.parametrize("project_pull_ok", [True, False])
def test_first_ack_loads_its_project_before_writing(tmp_path, monkeypatch, project_pull_ok):
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "worker")
    monkeypatch.setenv("AGENTTEAMS_WORKER_NAME", "alice")
    task_path = tmp_path / "shared/tasks/p1-01/meta.json"
    project_path = tmp_path / "shared/projects/p1/meta.json"
    assigned = {"task_id": "p1-01", "project_id": "p1", "status": "assigned", "assigned_to": "alice"}

    def pull_task(*args, **kwargs):
        server._write_json(task_path, assigned)
        return True

    def pull_project(*args, **kwargs):
        if project_pull_ok:
            server._write_json(project_path, {"project_id": "p1", "status": "active",
                "tasks": [{"task_id": "p1-01", "status": "assigned"}]})
        return project_pull_ok

    monkeypatch.setattr(server, "_pull_task", pull_task)
    monkeypatch.setattr(server, "_pull_project", pull_project)
    monkeypatch.setattr(server, "_sync_task", lambda *a, **kw: True)
    monkeypatch.setattr(server, "_sync_project", lambda *a, **kw: True)
    response = server.call_tool("taskflow", {"action": "ack_task", "workspaceDir": str(tmp_path),
        "payload": {"taskId": "p1-01"}})
    data = json.loads(response["content"][0]["text"])
    assert data["ok"] is project_pull_ok
    if project_pull_ok:
        assert server._read_json(project_path)["tasks"][0]["status"] == "in_progress"
    else:
        assert server._read_json(task_path)["status"] == "assigned"
