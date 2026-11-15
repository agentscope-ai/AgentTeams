"""Parent tasks and execution dependencies are separate persisted relations."""
import importlib.util
import sys
from pathlib import Path

import pytest


def load_server():
    path = Path(__file__).resolve().parents[2] / "teamharness" / "mcp"
    spec = importlib.util.spec_from_file_location("hierarchy_server", path / "server.py")
    module = importlib.util.module_from_spec(spec)
    sys.path.insert(0, str(path))
    try:
        spec.loader.exec_module(module)
    finally:
        sys.path.remove(str(path))
    return module


def test_parent_survives_replan_and_can_be_cleared():
    server = load_server()
    child = server._normalize_task({"taskId": "p-dev", "parentTaskId": "p-main", "dependsOn": ["p-analysis"]})
    assert child["parent_task_id"] == "p-main"
    assert child["depends_on"] == ["p-analysis"]
    assert server._normalize_task({"taskId": "p-dev"}, child)["parent_task_id"] == "p-main"
    assert "parent_task_id" not in server._normalize_task({"taskId": "p-dev", "parentTaskId": ""}, child)


def test_parent_validation_and_ready_nodes():
    server = load_server()
    tasks = [
        {"task_id": "p-main", "status": "planned", "depends_on": []},
        {"task_id": "p-a", "status": "planned", "parent_task_id": "p-main", "depends_on": []},
        {"task_id": "p-b", "status": "planned", "parent_task_id": "p-main", "depends_on": ["p-a"]},
    ]
    server._validate_task_graph(tasks)
    assert [t["task_id"] for t in server._ready_nodes({"status": "active", "tasks": tasks})] == ["p-a"]
    tasks[0]["status"] = "completed"
    with pytest.raises(ValueError, match="unfinished subtasks"):
        server._validate_task_graph(tasks)
    tasks[0]["status"] = "planned"
    tasks[0]["parent_task_id"] = "p-b"
    with pytest.raises(ValueError, match="parent cycle"):
        server._validate_task_graph(tasks)
    tasks[0]["parent_task_id"] = "missing"
    with pytest.raises(ValueError, match="unknown parent"):
        server._validate_task_graph(tasks)
