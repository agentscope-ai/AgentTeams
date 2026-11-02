from __future__ import annotations

import json
from pathlib import Path
import sys
from typing import Any

import pytest


MCP_DIR = Path(__file__).resolve().parents[3] / "teamharness" / "mcp"
if str(MCP_DIR) not in sys.path:
    sys.path.insert(0, str(MCP_DIR))

import server  # noqa: E402


PROJECT_ID = "gate-project"
TASK_IDS = ("gate-project-01", "gate-project-02")


def _tool_payload(name: str, arguments: dict[str, Any]) -> dict[str, Any]:
    response = server.call_tool(name, arguments)
    return json.loads(response["content"][0]["text"])


def _task_node(task_id: str, status: str) -> dict[str, Any]:
    return {
        "task_id": task_id,
        "title": f"Task {task_id}",
        "assigned_to": "@worker:example.test",
        "depends_on": [],
        "status": status,
    }


def _write_project(workspace: Path, tasks: list[dict[str, Any]]) -> None:
    project = {
        "project_id": PROJECT_ID,
        "title": "Completion gate",
        "status": "active",
        "tasks": tasks,
    }
    server._write_json(workspace / "shared" / "projects" / PROJECT_ID / "meta.json", project)


def _write_task_meta(
    workspace: Path,
    task_id: str,
    status: str,
    *,
    deliverables: list[str] | None = None,
) -> None:
    task: dict[str, Any] = {
        "task_id": task_id,
        "project_id": PROJECT_ID,
        "status": status,
    }
    if deliverables is not None:
        task.update(
            {
                "result_status": "SUCCESS",
                "summary": "The result is ready.",
                "deliverables": deliverables,
            }
        )
    server._write_json(workspace / "shared" / "tasks" / task_id / "meta.json", task)


def _write_result(workspace: Path, task_id: str) -> None:
    result_path = workspace / "shared" / "tasks" / task_id / "result.md"
    result_path.parent.mkdir(parents=True, exist_ok=True)
    result_path.write_text("Delivered.", encoding="utf-8")


def _complete(workspace: Path) -> dict[str, Any]:
    return _tool_payload(
        "projectflow",
        {
            "action": "complete_project",
            "workspaceDir": str(workspace),
            "payload": {"projectId": PROJECT_ID},
        },
    )


def _persisted_project_status(workspace: Path) -> str:
    project_path = workspace / "shared" / "projects" / PROJECT_ID / "meta.json"
    return json.loads(project_path.read_text(encoding="utf-8"))["status"]


@pytest.fixture(autouse=True)
def gate_side_effects(monkeypatch: pytest.MonkeyPatch) -> None:
    """Keep the gate tests off the shared-storage and Matrix boundaries.

    ``_pull_project``/``_sync_project`` are refs proxies resolved against
    the server namespace at call time, so server-level patches stay visible
    to projectflow. ``_send_project_completion_notification`` is a direct
    import inside project/flow.py, so it is patched on the flow module
    itself (no real Matrix, no shared storage).
    """
    monkeypatch.setattr(server, "_pull_project", lambda *_args, **_kwargs: True)
    monkeypatch.setattr(server, "_sync_project", lambda *_args, **_kwargs: True)
    monkeypatch.setattr(
        server.flow,
        "_send_project_completion_notification",
        lambda *_args, **_kwargs: {"sent": False, "skipped": True},
    )


def test_complete_project_succeeds_when_every_task_is_completed_with_artifacts(
    tmp_path: Path,
) -> None:
    _write_project(tmp_path, [_task_node(task_id, "completed") for task_id in TASK_IDS])
    for task_id in TASK_IDS:
        _write_task_meta(
            tmp_path,
            task_id,
            "completed",
            deliverables=[f"shared/tasks/{task_id}/result.md"],
        )
        _write_result(tmp_path, task_id)

    completed = _complete(tmp_path)

    assert completed["ok"] is True
    assert completed["project"]["status"] == "completed"
    assert _persisted_project_status(tmp_path) == "completed"


def test_complete_project_rejects_a_cancelled_task_without_mutating_state(
    tmp_path: Path,
) -> None:
    _write_project(tmp_path, [_task_node(TASK_IDS[0], "completed"), _task_node(TASK_IDS[1], "cancelled")])
    _write_task_meta(
        tmp_path,
        TASK_IDS[0],
        "completed",
        deliverables=[f"shared/tasks/{TASK_IDS[0]}/result.md"],
    )
    _write_result(tmp_path, TASK_IDS[0])
    _write_task_meta(tmp_path, TASK_IDS[1], "cancelled")
    project_path = tmp_path / "shared" / "projects" / PROJECT_ID / "meta.json"
    before = project_path.read_bytes()

    rejected = _complete(tmp_path)

    assert rejected["ok"] is False
    assert f"{TASK_IDS[1]} (status=cancelled)" in rejected["error"]
    # Validate-then-write: the rejection must leave the project untouched
    # (no status change, no PROJECT_COMPLETED side effects).
    assert project_path.read_bytes() == before
    assert _persisted_project_status(tmp_path) != "completed"


def test_complete_project_rejects_a_completed_task_without_any_artifact(
    tmp_path: Path,
) -> None:
    _write_project(tmp_path, [_task_node(task_id, "completed") for task_id in TASK_IDS])
    _write_task_meta(
        tmp_path,
        TASK_IDS[0],
        "completed",
        deliverables=[f"shared/tasks/{TASK_IDS[0]}/result.md"],
    )
    _write_result(tmp_path, TASK_IDS[0])
    # Completed on paper: no result.md in the workspace and no
    # deliverables in the task meta — the status alone is not evidence.
    _write_task_meta(tmp_path, TASK_IDS[1], "completed")
    project_path = tmp_path / "shared" / "projects" / PROJECT_ID / "meta.json"
    before = project_path.read_bytes()

    rejected = _complete(tmp_path)

    assert rejected["ok"] is False
    assert f"{TASK_IDS[1]} (artifact=missing)" in rejected["error"]
    assert project_path.read_bytes() == before
    assert _persisted_project_status(tmp_path) != "completed"


def test_complete_project_rejects_a_revision_task_without_mutating_state(
    tmp_path: Path,
) -> None:
    _write_project(tmp_path, [_task_node(TASK_IDS[0], "completed"), _task_node(TASK_IDS[1], "revision")])
    _write_task_meta(
        tmp_path,
        TASK_IDS[0],
        "completed",
        deliverables=[f"shared/tasks/{TASK_IDS[0]}/result.md"],
    )
    _write_result(tmp_path, TASK_IDS[0])
    _write_task_meta(tmp_path, TASK_IDS[1], "revision")
    project_path = tmp_path / "shared" / "projects" / PROJECT_ID / "meta.json"
    before = project_path.read_bytes()

    rejected = _complete(tmp_path)

    assert rejected["ok"] is False
    assert f"{TASK_IDS[1]} (status=revision)" in rejected["error"]
    assert project_path.read_bytes() == before
    assert _persisted_project_status(tmp_path) != "completed"


def test_complete_project_still_allows_an_empty_project(tmp_path: Path) -> None:
    # Non-goal of the C-1 gate: a project with zero tasks keeps the legacy
    # behavior — "every task is completed" is vacuously true, and an empty
    # project is not the fake-completion shape this gate removes.
    _write_project(tmp_path, [])

    completed = _complete(tmp_path)

    assert completed["ok"] is True
    assert completed["project"]["status"] == "completed"
    assert _persisted_project_status(tmp_path) == "completed"
