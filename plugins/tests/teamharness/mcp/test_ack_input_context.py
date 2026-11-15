"""A Worker receives the accepted prerequisite context with its durable ACK."""
import json
from pathlib import Path
import sys

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[3] / "teamharness/mcp"))
import server


@pytest.fixture
def assignment(tmp_path, monkeypatch):
    monkeypatch.delenv("AGENTTEAMS_AGENT_ROLE", raising=False)
    monkeypatch.delenv("TEAMHARNESS_RUNTIME_CONFIG", raising=False)
    monkeypatch.setattr(server, "_runtime_identity", lambda: "")
    pulled = []
    monkeypatch.setattr(server, "_pull_project", lambda *a: True)
    monkeypatch.setattr(server, "_pull_task", lambda args, task_id: pulled.append(task_id) or True)
    monkeypatch.setattr(server, "_sync_task", lambda *a, **kw: True)
    monkeypatch.setattr(server, "_sync_project", lambda *a, **kw: True)
    project = {"project_id": "p", "status": "active", "dispatch_mode": "automatic", "tasks": [
        {"task_id": "upstream", "status": "completed"},
        {"task_id": "current", "status": "assigned", "depends_on": ["upstream"]}]}
    server._write_json(tmp_path / "shared/projects/p/meta.json", project)
    current = tmp_path / "shared/tasks/current"
    server._write_json(current / "meta.json", {"task_id": "current", "project_id": "p", "status": "assigned"})
    (current / "spec.md").write_text("Verify the accepted implementation.")
    upstream = tmp_path / "shared/tasks/upstream"
    server._write_json(upstream / "meta.json", {"task_id": "upstream", "project_id": "p", "status": "completed",
        "summary": "Implementation accepted", "deliverables": ["shared/tasks/upstream/solution.py"]})
    args = {"workspaceDir": str(tmp_path), "role": "worker", "action": "ack_task", "payload": {"taskId": "current"}}
    return args, pulled, upstream


def invoke(args):
    return json.loads(server.call_tool("taskflow", args)["content"][0]["text"])


def test_ack_pulls_inputs_and_returns_local_paths_in_one_receipt(assignment):
    args, pulled, upstream = assignment
    result = invoke(args)
    assert result["ok"] and result["synced"]
    assert Path(result["scratchDirectory"]).is_dir()
    assert Path(result["scratchDirectory"]).parent == upstream.parent / "current"
    assert "upstream" in pulled
    assert result["inputs"][0] == {"taskId": "upstream", "directory": str(upstream.resolve()),
        "readOnly": True, "summary": "Implementation accepted", "deliverables": ["shared/tasks/upstream/solution.py"]}


@pytest.mark.parametrize("status", ["submitted", "cancelled"])
def test_unaccepted_input_does_not_ack_the_worker(assignment, status):
    args, _, upstream = assignment
    state = json.loads((upstream / "meta.json").read_text())
    state["status"] = status
    (upstream / "meta.json").write_text(json.dumps(state))
    result = invoke(args)
    assert not result["ok"]
    task = json.loads((upstream.parent / "current/meta.json").read_text())
    assert task["status"] == "assigned"


@pytest.mark.parametrize("failure", ["pull", "project"])
def test_unavailable_or_foreign_inputs_fail_before_ack(assignment, monkeypatch, failure):
    args, _, upstream = assignment
    if failure == "pull":
        monkeypatch.setattr(server, "_pull_task", lambda args, task_id: task_id != "upstream")
    else:
        state = json.loads((upstream / "meta.json").read_text())
        state["project_id"] = "other"
        (upstream / "meta.json").write_text(json.dumps(state))
    assert not invoke(args)["ok"]
    assert json.loads((upstream.parent / "current/meta.json").read_text())["status"] == "assigned"


def test_scratch_symlink_escape_does_not_commit_ack(assignment):
    args, _, upstream = assignment
    (upstream.parent / "current/workspace").symlink_to(upstream, target_is_directory=True)
    assert not invoke(args)["ok"]
    assert json.loads((upstream.parent / "current/meta.json").read_text())["status"] == "assigned"
