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
    monkeypatch.setattr(server, "_pull_task", lambda *_args, **_kwargs: True)
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


def test_coordinating_parent_uses_accepted_child_artifacts(tmp_path: Path) -> None:
    parent = _task_node("gate-main", "completed")
    child = {**_task_node(TASK_IDS[0], "completed"), "parent_task_id": "gate-main"}
    _write_project(tmp_path, [parent, child])
    _write_result(tmp_path, TASK_IDS[0])
    assert _complete(tmp_path)["ok"] is True


def test_parent_does_not_hide_child_without_delivery(tmp_path: Path) -> None:
    parent = _task_node("gate-main", "completed")
    child = {**_task_node(TASK_IDS[0], "completed"), "parent_task_id": "gate-main"}
    _write_project(tmp_path, [parent, child])
    rejected = _complete(tmp_path)
    assert rejected["ok"] is False
    assert f"{TASK_IDS[0]} (artifact=missing)" in rejected["error"]


def test_create_project_with_initial_tasks_returns_ready_work(tmp_path):
    result = _tool_payload("projectflow", {"action": "create_project", "workspaceDir": str(tmp_path), "payload": {
        "projectId": PROJECT_ID, "tasks": [
            {"taskId": "analysis"}, {"taskId": "dev", "dependsOn": ["analysis"]},
        ],
    }})
    assert result["ok"] is True
    assert [task["task_id"] for task in result["readyNodes"]] == ["analysis"]
    persisted = json.loads((tmp_path / "shared/projects" / PROJECT_ID / "meta.json").read_text())
    assert persisted["tasks"][1]["depends_on"] == ["analysis"]


def test_invalid_initial_plan_does_not_create_project(tmp_path):
    result = _tool_payload("projectflow", {"action": "create_project", "workspaceDir": str(tmp_path), "payload": {
        "projectId": PROJECT_ID, "tasks": [{"taskId": "dev", "dependsOn": ["missing"]}],
    }})
    assert result["ok"] is False
    assert not (tmp_path / "shared/projects" / PROJECT_ID / "meta.json").exists()


def test_check_task_returns_spec_and_report_in_one_response(tmp_path):
    task_id = TASK_IDS[0]
    _write_task_meta(tmp_path, task_id, "submitted", deliverables=[f"shared/tasks/{task_id}/result.md"])
    _write_result(tmp_path, task_id)
    (tmp_path / "shared/tasks" / task_id / "spec.md").write_text("Acceptance criteria.")
    result = _tool_payload("taskflow", {"role": "leader", "action": "check_task", "workspaceDir": str(tmp_path), "payload": {"taskId": task_id}})
    assert result["effective"] is True
    assert result["spec"] == "Acceptance criteria."
    assert result["report"] == "Delivered."


def test_acceptance_returns_next_ready_nodes_without_another_query(tmp_path, monkeypatch):
    first, second = TASK_IDS
    _write_project(tmp_path, [_task_node(first, "submitted"), {**_task_node(second, "planned"), "depends_on": [first]}])
    _write_task_meta(tmp_path, first, "submitted", deliverables=[f"shared/tasks/{first}/result.md"])
    _write_result(tmp_path, first)
    monkeypatch.setattr(server, "_sync_task", lambda *a, **k: True)
    result = _tool_payload("projectflow", {"role": "leader", "action": "accept_task_result", "workspaceDir": str(tmp_path), "payload": {"projectId": PROJECT_ID, "taskId": first, "resultStatus": "SUCCESS"}})
    assert result["ok"] is True
    assert [task["task_id"] for task in result["readyNodes"]] == [second]
    assert not result["project"].get("requester_report", {}).get("pending")


def test_final_report_is_sent_once_after_terminal_commit(tmp_path, monkeypatch):
    _write_project(tmp_path, [_task_node(TASK_IDS[0], "completed")])
    _write_result(tmp_path, TASK_IDS[0])
    path = tmp_path / "shared/projects" / PROJECT_ID / "meta.json"
    project = json.loads(path.read_text())
    project["reply_route"] = {"channel": "matrix", "target_session": "!manager:test", "target_user": "@manager:test"}
    project["requester_reply_route"] = {"channel": "matrix", "target_session": "!human:test", "target_user": "@human:test"}
    server._write_json(path, project)
    (path.parent / "result.md").write_text("Final acceptance report.")
    calls = []
    def send(arguments):
        assert _persisted_project_status(tmp_path) == "completed"
        calls.append(arguments)
        return {"ok": True, "messageId": "$final"}
    monkeypatch.setattr(server, "_message", send)
    first = _complete(tmp_path)
    assert first["requesterNotification"]["sent"] is True
    assert calls[0]["workflowEvent"] == {"type": "PROJECT_COMPLETED", "project_id": PROJECT_ID}
    assert calls[0]["requesterReplyRoute"]["target_session"] == "!human:test"
    assert "Final acceptance report." in calls[0]["message"]
    assert _complete(tmp_path)["requesterNotification"]["reused"] is True
    assert len(calls) == 1


@pytest.mark.parametrize("deliverables", [[""], ["  "], ["shared/tasks/unrelated-task/result.md"], ["../../outside.md"]])
def test_invalid_registered_deliverables_do_not_complete_project(tmp_path, deliverables):
    task_id = TASK_IDS[0]
    _write_project(tmp_path, [_task_node(task_id, "completed")])
    _write_task_meta(tmp_path, task_id, "completed", deliverables=deliverables)
    project_path = tmp_path / "shared/projects" / PROJECT_ID / "meta.json"
    before = project_path.read_bytes()
    rejected = _complete(tmp_path)
    assert rejected["ok"] is False
    assert project_path.read_bytes() == before


def test_valid_registered_deliverables_can_complete_without_local_result_md(tmp_path):
    task_id = TASK_IDS[0]
    _write_project(tmp_path, [_task_node(task_id, "completed")])
    _write_task_meta(tmp_path, task_id, "completed", deliverables=[f"shared/tasks/{task_id}/report.txt"])
    assert _complete(tmp_path)["ok"] is True


def test_remote_task_registration_is_pulled_before_completion(tmp_path, monkeypatch):
    task_id = TASK_IDS[0]
    _write_project(tmp_path, [_task_node(task_id, "completed")])
    def pull(_arguments, pulled_id):
        assert pulled_id == task_id
        _write_task_meta(tmp_path, task_id, "completed", deliverables=[f"shared/tasks/{task_id}/report.txt"])
        return True
    monkeypatch.setattr(server, "_pull_task", pull)
    assert _complete(tmp_path)["ok"] is True


def test_failed_task_pull_cannot_use_cached_registration(tmp_path, monkeypatch):
    task_id = TASK_IDS[0]
    _write_project(tmp_path, [_task_node(task_id, "completed")])
    _write_task_meta(tmp_path, task_id, "completed", deliverables=[f"shared/tasks/{task_id}/report.txt"])
    monkeypatch.setattr(server, "_pull_task", lambda *_: False)
    monkeypatch.setattr(server, "_filesync", lambda *_: {"ok": False, "error": "storage unavailable"})
    project_path = tmp_path / "shared/projects" / PROJECT_ID / "meta.json"
    before = project_path.read_bytes()
    assert _complete(tmp_path)["ok"] is False
    assert project_path.read_bytes() == before


@pytest.mark.parametrize("status", ["revision", "cancelled"])
def test_loop_completion_retains_terminal_task_contract(tmp_path: Path, status: str) -> None:
    _write_project(tmp_path, [])
    path = tmp_path / "shared/projects" / PROJECT_ID / "meta.json"
    project = json.loads(path.read_text())
    project["plan_type"] = "loop"
    project["loop"] = {"tasks": [_task_node(TASK_IDS[0], status)]}
    server._write_json(path, project)
    assert _complete(tmp_path)["ok"] is True


@pytest.mark.parametrize("leaf_status", ["completed", "submitted"])
def test_leader_report_finalizes_parents_only_after_all_leaves_are_accepted(tmp_path, leaf_status):
    root = _task_node("parent", "planned")
    leaf = {**_task_node(TASK_IDS[0], leaf_status), "parent_task_id": "parent"}
    _write_project(tmp_path, [root, leaf])
    _write_task_meta(tmp_path, TASK_IDS[0], leaf_status)
    _write_result(tmp_path, TASK_IDS[0])
    result = _tool_payload("projectflow", {"role": "leader", "action": "complete_project", "workspaceDir": str(tmp_path),
        "payload": {"projectId": PROJECT_ID, "report": "Accepted requirements, implementation, independent QA and usage documentation."}})
    assert result["ok"] is (leaf_status == "completed")
    saved = json.loads((tmp_path / "shared/projects" / PROJECT_ID / "meta.json").read_text())
    assert saved["tasks"][0]["status"] == ("completed" if leaf_status == "completed" else "planned")
    assert (tmp_path / "shared/projects" / PROJECT_ID / "result.md").exists() is (leaf_status == "completed")


def test_final_report_does_not_override_cancelled_parent(tmp_path):
    root = _task_node("parent", "cancelled")
    leaf = {**_task_node(TASK_IDS[0], "completed"), "parent_task_id": "parent"}
    _write_project(tmp_path, [root, leaf])
    _write_result(tmp_path, TASK_IDS[0])
    result = _tool_payload("projectflow", {"role": "leader", "action": "complete_project", "workspaceDir": str(tmp_path),
        "payload": {"projectId": PROJECT_ID, "report": "All leaf results accepted."}})
    assert not result["ok"]
    assert _persisted_project_status(tmp_path) == "active"
    assert not (tmp_path / "shared/projects" / PROJECT_ID / "result.md").exists()
