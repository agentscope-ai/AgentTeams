import json
import threading
from pathlib import Path
from unittest.mock import MagicMock

import pytest

import copaw_worker.hooks.tools.projectflow as projectflow_tool
import copaw_worker.hooks.tools.taskflow as taskflow_tool
from copaw_worker.hooks.tools.projectflow import projectflow
from copaw_worker.hooks.tools.taskflow import taskflow
from copaw_worker.task import (
    FileSystemTaskStore,
    TaskflowError,
    add_tasks,
    create_project,
    parse_dag_tasks,
    parse_loop_plan,
)


def _response_json(response):
    item = response.content[0]
    text = item.get("text") if isinstance(item, dict) else item.text
    return json.loads(text)


def _set_actor(monkeypatch, actor: str) -> None:
    monkeypatch.setenv("AGENTTEAMS_MATRIX_USER_ID", actor)


def _mock_sync(monkeypatch) -> MagicMock:
    """Patch create_sync in taskflow to return a no-op FileSync mock."""
    mock = MagicMock()
    monkeypatch.setattr(taskflow_tool, "create_sync", lambda: mock)
    return mock


def _write_team_leader_runtime_config(base_dir: Path) -> None:
    runtime_dir = base_dir / "runtime"
    runtime_dir.mkdir(parents=True)
    (runtime_dir / "runtime.yaml").write_text(
        "kind: MemberRuntimeConfig\n"
        "member:\n"
        "  role: team_leader\n"
        "team:\n"
        "  teamRoomId: \"!team:domain\"\n"
        "  leaderDmRoomId: \"!leader-dm:domain\"\n",
    )


@pytest.mark.asyncio
async def test_taskflow_project_assignment_and_completion(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@worker-a:domain")
    _mock_sync(monkeypatch)

    response = await projectflow(
        action="create_project",
        payload={
            "projectId": "tp-01",
            "title": "Research project",
            "source": "team-admin",
            "requester": "@admin:domain",
        },
    )
    assert _response_json(response)["ok"] is True

    response = await projectflow(
        action="plan_dag",
        payload={
            "projectId": "tp-01",
            "tasks": [
                {
                    "taskId": "st-01",
                    "title": "Collect sources",
                    "assignedTo": "@worker-a:domain",
                    "dependsOn": [],
                },
                {
                    "taskId": "st-02",
                    "title": "Summarize findings",
                    "assignedTo": "@worker-b:domain",
                    "dependsOn": ["st-01"],
                },
            ],
        },
    )
    payload = _response_json(response)
    assert payload["ok"] is True
    assert [task["task_id"] for task in payload["readyNodes"]] == ["st-01"]

    response = await taskflow(
        action="delegate_task",
        payload={
            "projectId": "tp-01",
            "taskId": "st-01",
            "roomId": "room:!worker-room:domain",
            "spec": "Collect sources and write shared/tasks/st-01/result.md.",
        },
    )
    payload = _response_json(response)
    assert payload["ok"] is True
    assert payload["task"]["status"] == "assigned"
    assert (workspace / "shared" / "tasks" / "st-01" / "spec.md").exists()

    response = await taskflow(action="ack_task", payload={"taskId": "st-01"})
    payload = _response_json(response)
    assert payload["ok"] is True
    assert payload["task"]["status"] == "in_progress"

    result_path = workspace / "shared" / "tasks" / "st-01" / "result.md"
    result_path.write_text(
        "STATUS: SUCCESS\n"
        "SUMMARY: Sources collected.\n\n"
        "DELIVERABLES:\n"
        "- shared/tasks/st-01/sources.md\n",
    )
    sources_path = workspace / "shared" / "tasks" / "st-01" / "sources.md"
    sources_path.write_text("source list\n", encoding="utf-8")

    response = await taskflow(action="submit_task", payload={"taskId": "st-01"})
    payload = _response_json(response)
    assert payload["ok"] is True
    assert payload["task"]["status"] == "submitted"

    response = await projectflow(action="ready_nodes", payload={"projectId": "tp-01"})
    payload = _response_json(response)
    assert payload["ok"] is True
    assert payload["readyNodes"] == []

    plan = (workspace / "shared" / "projects" / "tp-01" / "plan.md").read_text()
    tasks = {task.task_id: task for task in parse_dag_tasks(plan)}
    assert tasks["st-01"].status == "delegated"
    assert tasks["st-02"].status == "pending"

    plan_path = workspace / "shared" / "projects" / "tp-01" / "plan.md"
    plan_path.write_text(plan.replace("- [~] st-01", "- [x] st-01"))

    response = await projectflow(action="ready_nodes", payload={"projectId": "tp-01"})
    payload = _response_json(response)
    assert payload["ok"] is True
    assert [task["task_id"] for task in payload["readyNodes"]] == ["st-02"]


@pytest.mark.asyncio
async def test_projectflow_check_active_tasks_reports_idle_worker(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@leader:domain")
    _mock_sync(monkeypatch)

    response = await projectflow(
        action="create_project",
        payload={
            "projectId": "tp-watch",
            "title": "Watched project",
            "source": "team-admin",
            "requester": "@admin:domain",
        },
    )
    assert _response_json(response)["ok"] is True

    response = await projectflow(
        action="plan_dag",
        payload={
            "projectId": "tp-watch",
            "tasks": [
                {
                    "taskId": "tp-watch-01",
                    "title": "Long task",
                    "assignedTo": "@worker-a:domain",
                    "dependsOn": [],
                }
            ],
        },
    )
    assert _response_json(response)["ok"] is True

    response = await taskflow(
        action="delegate_task",
        payload={
            "projectId": "tp-watch",
            "taskId": "tp-watch-01",
            "roomId": "room:!team:domain",
            "spec": "Do a long task.",
        },
    )
    assert _response_json(response)["ok"] is True

    async def fake_runtime(worker_name: str, *, timeout_seconds: int):
        assert worker_name == "@worker-a:domain"
        assert timeout_seconds == 3
        return {
            "runtimeStatus": "idle",
            "runtimeStatusSource": "test",
            "runningSessionCount": 0,
            "sessionCount": 1,
        }

    monkeypatch.setattr(projectflow_tool, "_fetch_worker_runtime_status", fake_runtime)

    response = await projectflow(action="check_active_tasks", payload={"projectId": "tp-watch"})
    payload = _response_json(response)

    assert payload["ok"] is True
    assert payload["checkedProjects"] == 1
    assert len(payload["issues"]) == 1
    assert payload["issues"][0]["issueType"] == "task_not_running"
    assert payload["issues"][0]["runtimeStatus"] == "idle"
    assert payload["issues"][0]["taskId"] == "tp-watch-01"


@pytest.mark.asyncio
async def test_projectflow_check_active_tasks_ignores_running_worker(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@leader:domain")
    _mock_sync(monkeypatch)

    assert _response_json(
        await projectflow(
            action="create_project",
            payload={"projectId": "tp-running", "title": "Running project"},
        )
    )["ok"] is True
    assert _response_json(
        await projectflow(
            action="plan_dag",
            payload={
                "projectId": "tp-running",
                "tasks": [
                    {
                        "taskId": "tp-running-01",
                        "title": "Long task",
                        "assignedTo": "worker-a",
                        "dependsOn": [],
                    }
                ],
            },
        )
    )["ok"] is True
    assert _response_json(
        await taskflow(
            action="delegate_task",
            payload={
                "projectId": "tp-running",
                "taskId": "tp-running-01",
                "roomId": "room:!team:domain",
                "spec": "Do a long task.",
            },
        )
    )["ok"] is True

    async def fake_runtime(worker_name: str, *, timeout_seconds: int):
        return {
            "runtimeStatus": "running",
            "runtimeStatusSource": "test",
            "runningSessionCount": 1,
            "sessionCount": 1,
        }

    monkeypatch.setattr(projectflow_tool, "_fetch_worker_runtime_status", fake_runtime)

    response = await projectflow(action="check_active_tasks", payload={"projectId": "tp-running"})
    payload = _response_json(response)

    assert payload["ok"] is True
    assert payload["checkedProjects"] == 1
    assert payload["issues"] == []


@pytest.mark.asyncio
async def test_projectflow_check_active_tasks_reports_pending_result(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@leader:domain")
    _mock_sync(monkeypatch)

    assert _response_json(
        await projectflow(
            action="create_project",
            payload={"projectId": "tp-result", "title": "Result project"},
        )
    )["ok"] is True
    assert _response_json(
        await projectflow(
            action="plan_dag",
            payload={
                "projectId": "tp-result",
                "tasks": [
                    {
                        "taskId": "tp-result-01",
                        "title": "Task with result",
                        "assignedTo": "worker-a",
                        "dependsOn": [],
                    }
                ],
            },
        )
    )["ok"] is True
    assert _response_json(
        await taskflow(
            action="delegate_task",
            payload={
                "projectId": "tp-result",
                "taskId": "tp-result-01",
                "roomId": "room:!team:domain",
                "spec": "Do work.",
            },
        )
    )["ok"] is True

    (workspace / "shared" / "tasks" / "tp-result-01" / "result.md").write_text(
        "STATUS: SUCCESS\n"
        "SUMMARY: Done.\n\n"
        "DELIVERABLES:\n"
        "- shared/tasks/tp-result-01/workspace/done.md\n",
    )

    response = await projectflow(action="check_active_tasks", payload={"projectId": "tp-result"})
    payload = _response_json(response)

    assert payload["ok"] is True
    assert payload["issues"][0]["issueType"] == "task_result_pending_check"
    assert payload["issues"][0]["resultStatus"] == "SUCCESS"


@pytest.mark.asyncio
async def test_projectflow_check_active_tasks_reports_ready_tasks_pending(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))

    assert _response_json(
        await projectflow(
            action="create_project",
            payload={"projectId": "tp-ready", "title": "Ready project"},
        )
    )["ok"] is True
    assert _response_json(
        await projectflow(
            action="plan_dag",
            payload={
                "projectId": "tp-ready",
                "tasks": [
                    {
                        "taskId": "tp-ready-01",
                        "title": "Ready task",
                        "assignedTo": "worker-a",
                        "dependsOn": [],
                    }
                ],
            },
        )
    )["ok"] is True

    response = await projectflow(action="check_active_tasks", payload={"projectId": "tp-ready"})
    payload = _response_json(response)

    assert payload["ok"] is True
    assert payload["issues"][0]["issueType"] == "ready_tasks_pending"
    assert payload["issues"][0]["readyTasks"][0]["taskId"] == "tp-ready-01"


@pytest.mark.asyncio
async def test_projectflow_check_active_tasks_reports_project_completion_pending(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))

    assert _response_json(
        await projectflow(
            action="create_project",
            payload={"projectId": "tp-complete", "title": "Completion project"},
        )
    )["ok"] is True
    assert _response_json(
        await projectflow(
            action="plan_dag",
            payload={
                "projectId": "tp-complete",
                "tasks": [
                    {
                        "taskId": "tp-complete-01",
                        "title": "Done task",
                        "assignedTo": "worker-a",
                        "dependsOn": [],
                    }
                ],
            },
        )
    )["ok"] is True

    plan_path = workspace / "shared" / "projects" / "tp-complete" / "plan.md"
    plan_path.write_text(plan_path.read_text().replace("- [ ] tp-complete-01", "- [x] tp-complete-01"))

    response = await projectflow(action="check_active_tasks", payload={"projectId": "tp-complete"})
    payload = _response_json(response)

    assert payload["ok"] is True
    assert payload["issues"][0]["issueType"] == "project_completion_pending"


@pytest.mark.asyncio
async def test_projectflow_check_active_tasks_reports_loop_iteration_decision_pending(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))

    assert _response_json(
        await projectflow(
            action="create_project",
            payload={"projectId": "tp-loop-decision", "title": "Loop decision project"},
        )
    )["ok"] is True
    assert _response_json(
        await projectflow(
            action="plan_loop",
            payload={
                "projectId": "tp-loop-decision",
                "goal": "Improve until accepted.",
                "maxIterations": 3,
                "stopCondition": "Accepted.",
                "iterationTemplate": "Do one wave.",
                "tasks": [
                    {
                        "taskId": "tp-loop-decision-i001-01",
                        "title": "Iteration task",
                        "assignedTo": "worker-a",
                        "dependsOn": [],
                    }
                ],
            },
        )
    )["ok"] is True

    plan_path = workspace / "shared" / "projects" / "tp-loop-decision" / "plan.md"
    plan_path.write_text(
        plan_path.read_text().replace(
            "- [ ] tp-loop-decision-i001-01",
            "- [x] tp-loop-decision-i001-01",
        )
    )

    response = await projectflow(action="check_active_tasks", payload={"projectId": "tp-loop-decision"})
    payload = _response_json(response)

    assert payload["ok"] is True
    assert payload["issues"][0]["issueType"] == "loop_iteration_decision_pending"
    assert payload["issues"][0]["maxIterations"] == 3


@pytest.mark.asyncio
async def test_delegate_task_requires_room_id(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@leader:domain")

    response = await projectflow(
        action="create_project",
        payload={
            "projectId": "tp-room",
            "title": "Room-bound project",
            "source": "team-admin",
            "requester": "@admin:domain",
        },
    )
    assert _response_json(response)["ok"] is True

    response = await projectflow(
        action="plan_dag",
        payload={
            "projectId": "tp-room",
            "tasks": [
                {
                    "taskId": "tp-room-01",
                    "title": "Room-bound task",
                    "assignedTo": "@worker:domain",
                    "dependsOn": [],
                }
            ],
        },
    )
    assert _response_json(response)["ok"] is True

    response = await taskflow(
        action="delegate_task",
        payload={
            "projectId": "tp-room",
            "taskId": "tp-room-01",
            "spec": "Do work.",
        },
    )
    payload = _response_json(response)

    assert payload["ok"] is False
    assert payload["error"] == "payload.roomId is required"


@pytest.mark.asyncio
async def test_delegate_task_rejects_team_leader_dm_room(tmp_path, monkeypatch):
    leader_dir = tmp_path / "leader"
    working_dir = leader_dir / ".copaw"
    _write_team_leader_runtime_config(leader_dir)
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@leader:domain")

    assert _response_json(
        await projectflow(
            action="create_project",
            payload={"projectId": "tp-team-room", "title": "Team room project"},
        )
    )["ok"] is True
    assert _response_json(
        await projectflow(
            action="plan_dag",
            payload={
                "projectId": "tp-team-room",
                "tasks": [
                    {
                        "taskId": "tp-team-room-01",
                        "title": "Team task",
                        "assignedTo": "@worker:domain",
                        "dependsOn": [],
                    }
                ],
            },
        )
    )["ok"] is True

    response = await taskflow(
        action="delegate_task",
        payload={
            "projectId": "tp-team-room",
            "taskId": "tp-team-room-01",
            "roomId": "room:!leader-dm:domain",
            "spec": "Do work.",
        },
    )
    payload = _response_json(response)

    assert payload["ok"] is False
    assert "must use the Team Room room:!team:domain" in payload["error"]


@pytest.mark.asyncio
async def test_delegate_task_accepts_team_leader_team_room(tmp_path, monkeypatch):
    leader_dir = tmp_path / "leader"
    working_dir = leader_dir / ".copaw"
    _write_team_leader_runtime_config(leader_dir)
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@leader:domain")
    _mock_sync(monkeypatch)

    assert _response_json(
        await projectflow(
            action="create_project",
            payload={"projectId": "tp-team-ok", "title": "Team room project"},
        )
    )["ok"] is True
    assert _response_json(
        await projectflow(
            action="plan_dag",
            payload={
                "projectId": "tp-team-ok",
                "tasks": [
                    {
                        "taskId": "tp-team-ok-01",
                        "title": "Team task",
                        "assignedTo": "@worker:domain",
                        "dependsOn": [],
                    }
                ],
            },
        )
    )["ok"] is True

    response = await taskflow(
        action="delegate_task",
        payload={
            "projectId": "tp-team-ok",
            "taskId": "tp-team-ok-01",
            "roomId": "room:!team:domain",
            "spec": "Do work.",
        },
    )
    payload = _response_json(response)

    assert payload["ok"] is True
    assert payload["task"]["room_id"] == "room:!team:domain"


@pytest.mark.asyncio
async def test_submit_task_writes_structured_result(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@worker:domain")
    _mock_sync(monkeypatch)

    task_dir = workspace / "shared" / "tasks" / "st-01"
    task_dir.mkdir(parents=True)
    (task_dir / "meta.json").write_text(
        json.dumps(
            {
                "task_id": "st-01",
                "project_id": "tp-01",
                "task_title": "Task",
                "assigned_to": "@worker:domain",
                "room_id": "room:!team-room:domain",
                "status": "in_progress",
                "depends_on": [],
            },
        ),
    )
    deliverable_path = task_dir / "workspace" / "api-design.md"
    deliverable_path.parent.mkdir(parents=True, exist_ok=True)
    deliverable_path.write_text("api design\n", encoding="utf-8")

    response = await taskflow(
        action="submit_task",
        payload={
            "taskId": "st-01",
            "status": "SUCCESS",
            "summary": "API design completed.",
            "deliverables": [
                "shared/tasks/st-01/workspace/api-design.md",
            ],
        },
    )
    payload = _response_json(response)

    assert payload["ok"] is True
    assert payload["task"]["status"] == "submitted"
    assert payload["result"] == {
        "status": "SUCCESS",
        "summary": "API design completed.",
        "deliverables": ["shared/tasks/st-01/workspace/api-design.md"],
        "notes": [],
    }
    assert (task_dir / "result.md").read_text() == (
        "STATUS: SUCCESS\n"
        "SUMMARY: API design completed.\n\n"
        "DELIVERABLES:\n"
        "- shared/tasks/st-01/workspace/api-design.md\n"
    )


@pytest.mark.asyncio
async def test_ack_task_rejects_wrong_worker(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@wrong-worker:domain")
    _mock_sync(monkeypatch)

    task_dir = workspace / "shared" / "tasks" / "st-01"
    task_dir.mkdir(parents=True)
    (task_dir / "meta.json").write_text(
        json.dumps(
            {
                "task_id": "st-01",
                "project_id": "tp-01",
                "task_title": "Task",
                "assigned_to": "@worker:domain",
                "room_id": "room:!team-room:domain",
                "status": "assigned",
                "depends_on": [],
            },
        ),
    )

    response = await taskflow(action="ack_task", payload={"taskId": "st-01"})
    payload = _response_json(response)

    assert payload["ok"] is False
    assert "assigned to @worker:domain" in payload["error"]


@pytest.mark.asyncio
async def test_ack_task_rejects_missing_room_id(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@worker:domain")
    _mock_sync(monkeypatch)

    task_dir = workspace / "shared" / "tasks" / "st-01"
    task_dir.mkdir(parents=True)
    (task_dir / "meta.json").write_text(
        json.dumps(
            {
                "task_id": "st-01",
                "project_id": "tp-01",
                "task_title": "Task",
                "assigned_to": "@worker:domain",
                "status": "assigned",
                "depends_on": [],
            },
        ),
    )

    response = await taskflow(action="ack_task", payload={"taskId": "st-01"})
    payload = _response_json(response)

    assert payload["ok"] is False
    assert payload["error"] == "task st-01 is missing room_id"


@pytest.mark.asyncio
async def test_ack_task_accepts_canonical_worker_identity(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@backend-platform-engineer:domain")
    _mock_sync(monkeypatch)

    task_dir = workspace / "shared" / "tasks" / "st-01"
    task_dir.mkdir(parents=True)
    (task_dir / "meta.json").write_text(
        json.dumps(
            {
                "task_id": "st-01",
                "project_id": "tp-01",
                "task_title": "Task",
                "assigned_to": "backend-platform-engineer",
                "room_id": "room:!team-room:domain",
                "status": "assigned",
                "depends_on": [],
            },
        ),
    )
    (task_dir / "spec.md").write_text("# Task spec\nDo the work.\n")

    response = await taskflow(action="ack_task", payload={"taskId": "st-01"})
    payload = _response_json(response)

    assert payload["ok"] is True
    assert payload["task"]["status"] == "in_progress"
    assert "spec" in payload
    assert "Do the work." in payload["spec"]


@pytest.mark.asyncio
async def test_ack_task_accepts_display_name_worker_identity(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "backend-platform-engineer 💕")
    _mock_sync(monkeypatch)

    task_dir = workspace / "shared" / "tasks" / "st-01"
    task_dir.mkdir(parents=True)
    (task_dir / "meta.json").write_text(
        json.dumps(
            {
                "task_id": "st-01",
                "project_id": "tp-01",
                "task_title": "Task",
                "assigned_to": "backend-platform-engineer",
                "room_id": "room:!team-room:domain",
                "status": "assigned",
                "depends_on": [],
            },
        ),
    )
    (task_dir / "spec.md").write_text("# Task spec\nDo the work.\n")

    response = await taskflow(action="ack_task", payload={"taskId": "st-01"})
    payload = _response_json(response)

    assert payload["ok"] is True
    assert payload["task"]["status"] == "in_progress"
    assert "spec" in payload


@pytest.mark.asyncio
async def test_submit_task_rejects_wrong_worker(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@wrong-worker:domain")

    task_dir = workspace / "shared" / "tasks" / "st-01"
    task_dir.mkdir(parents=True)
    (task_dir / "meta.json").write_text(
        json.dumps(
            {
                "task_id": "st-01",
                "project_id": "tp-01",
                "task_title": "Task",
                "assigned_to": "@worker:domain",
                "room_id": "room:!team-room:domain",
                "status": "in_progress",
                "depends_on": [],
            },
        ),
    )

    response = await taskflow(
        action="submit_task",
        payload={
            "taskId": "st-01",
            "status": "SUCCESS",
            "summary": "Done.",
            "deliverables": [],
        },
    )
    payload = _response_json(response)

    assert payload["ok"] is False
    assert "assigned to @worker:domain" in payload["error"]
    assert not (task_dir / "result.md").exists()


@pytest.mark.asyncio
async def test_projectflow_plan_dag_accepts_json_string(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))

    response = await projectflow(
        action="create_project",
        payload={
            "projectId": "tp-json",
            "title": "JSON tasks project",
        },
    )
    assert _response_json(response)["ok"] is True

    response = await projectflow(
        action="plan_dag",
        payload={
            "projectId": "tp-json",
            "tasks": json.dumps(
                [
                    {
                        "taskId": "st-01",
                        "title": "Design API",
                        "assignedTo": "@worker:domain",
                        "dependsOn": [],
                    },
                ],
            ),
        },
    )
    payload = _response_json(response)

    assert payload["ok"] is True
    assert payload["tasks"][0]["task_id"] == "st-01"
    assert payload["readyNodes"][0]["task_id"] == "st-01"


@pytest.mark.asyncio
async def test_projectflow_plan_dag_generates_ready_nodes(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))

    response = await projectflow(
        action="create_project",
        payload={
            "projectId": "tp-plan",
            "title": "Plan DAG project",
        },
    )
    assert _response_json(response)["ok"] is True

    response = await projectflow(
        action="plan_dag",
        payload={
            "projectId": "tp-plan",
            "tasks": [
                {
                    "taskId": "tp-plan-01",
                    "title": "Root task",
                    "assignedTo": "@worker-a:domain",
                    "dependsOn": [],
                },
                {
                    "taskId": "tp-plan-02",
                    "title": "Follow-up task",
                    "assignedTo": "@worker-b:domain",
                    "dependsOn": ["tp-plan-01"],
                },
            ],
        },
    )
    payload = _response_json(response)

    assert payload["ok"] is True
    assert payload["action"] == "plan_dag"
    assert [task["task_id"] for task in payload["readyNodes"]] == ["tp-plan-01"]


@pytest.mark.asyncio
async def test_projectflow_plan_loop_generates_ready_loop_nodes(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))

    response = await projectflow(
        action="create_project",
        payload={
            "projectId": "tp-loop",
            "title": "Loop project",
        },
    )
    assert _response_json(response)["ok"] is True

    response = await projectflow(
        action="plan_loop",
        payload={
            "projectId": "tp-loop",
            "goal": "Improve answers until quality passes.",
            "maxIterations": 100,
            "stopCondition": "Stop when evaluator accepts the answer.",
            "iterationTemplate": "Generate one answer and verify it.",
            "tasks": [
                {
                    "taskId": "tp-loop-i001-01",
                    "title": "Generate candidate answer",
                    "assignedTo": "@writer:domain",
                    "dependsOn": [],
                },
                {
                    "taskId": "tp-loop-i001-02",
                    "title": "Verify candidate answer",
                    "assignedTo": "@reviewer:domain",
                    "dependsOn": ["tp-loop-i001-01"],
                },
            ],
        },
    )
    payload = _response_json(response)

    assert payload["ok"] is True
    assert payload["action"] == "plan_loop"
    assert payload["loop"]["max_iterations"] == 100
    assert [task["task_id"] for task in payload["readyNodes"]] == ["tp-loop-i001-01"]

    plan = (workspace / "shared" / "projects" / "tp-loop" / "plan.md").read_text()
    assert "**Plan Type**: loop" in plan
    assert "**Iteration**: 0 / 100" in plan
    loop = parse_loop_plan(plan)
    assert loop is not None
    assert loop.goal == "Improve answers until quality passes."
    assert [task.task_id for task in loop.tasks] == ["tp-loop-i001-01", "tp-loop-i001-02"]


@pytest.mark.asyncio
async def test_loop_task_submission_waits_for_leader_acceptance(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@writer:domain")
    _mock_sync(monkeypatch)

    assert _response_json(
        await projectflow(
            action="create_project",
            payload={"projectId": "tp-loop-delegate", "title": "Loop delegation project"},
        ),
    )["ok"] is True

    assert _response_json(
        await projectflow(
            action="plan_loop",
            payload={
                "projectId": "tp-loop-delegate",
                "goal": "Iterate until accepted.",
                "maxIterations": 10,
                "stopCondition": "Reviewer accepts.",
                "iterationTemplate": "Write then review.",
                "tasks": [
                    {
                        "taskId": "tp-loop-delegate-i001-01",
                        "title": "Write draft",
                        "assignedTo": "@writer:domain",
                        "dependsOn": [],
                    },
                    {
                        "taskId": "tp-loop-delegate-i001-02",
                        "title": "Review draft",
                        "assignedTo": "@reviewer:domain",
                        "dependsOn": ["tp-loop-delegate-i001-01"],
                    },
                ],
            },
        ),
    )["ok"] is True

    response = await taskflow(
        action="delegate_task",
        payload={
            "projectId": "tp-loop-delegate",
            "taskId": "tp-loop-delegate-i001-01",
            "roomId": "room:!team-room:domain",
            "spec": "Write the draft.",
        },
    )
    payload = _response_json(response)
    assert payload["ok"] is True
    assert payload["task"]["status"] == "assigned"

    response = await taskflow(action="ack_task", payload={"taskId": "tp-loop-delegate-i001-01"})
    assert _response_json(response)["ok"] is True
    draft_path = (
        workspace / "shared" / "tasks" / "tp-loop-delegate-i001-01" / "workspace" / "draft.md"
    )
    draft_path.parent.mkdir(parents=True, exist_ok=True)
    draft_path.write_text("draft content\n", encoding="utf-8")
    response = await taskflow(
        action="submit_task",
        payload={
            "taskId": "tp-loop-delegate-i001-01",
            "status": "SUCCESS",
            "summary": "Draft written.",
            "deliverables": [
                "shared/tasks/tp-loop-delegate-i001-01/workspace/draft.md",
            ],
        },
    )
    assert _response_json(response)["ok"] is True

    response = await projectflow(
        action="ready_loop_nodes",
        payload={"projectId": "tp-loop-delegate"},
    )
    payload = _response_json(response)
    assert payload["ok"] is True
    assert payload["readyNodes"] == []

    plan = (workspace / "shared" / "projects" / "tp-loop-delegate" / "plan.md").read_text()
    loop = parse_loop_plan(plan)
    assert loop is not None
    assert {task.task_id: task.status for task in loop.tasks}["tp-loop-delegate-i001-01"] == "delegated"

    plan_path = workspace / "shared" / "projects" / "tp-loop-delegate" / "plan.md"
    plan_path.write_text(plan.replace("- [~] tp-loop-delegate-i001-01", "- [x] tp-loop-delegate-i001-01"))

    response = await projectflow(
        action="ready_loop_nodes",
        payload={"projectId": "tp-loop-delegate"},
    )
    payload = _response_json(response)
    assert payload["ok"] is True
    assert [task["task_id"] for task in payload["readyNodes"]] == [
        "tp-loop-delegate-i001-02",
    ]


@pytest.mark.asyncio
async def test_dag_and_loop_ready_actions_are_separate(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))

    assert _response_json(
        await projectflow(
            action="create_project",
            payload={"projectId": "tp-loop-boundary", "title": "Loop boundary project"},
        ),
    )["ok"] is True
    assert _response_json(
        await projectflow(
            action="plan_loop",
            payload={
                "projectId": "tp-loop-boundary",
                "goal": "Iterate until accepted.",
                "maxIterations": 10,
                "stopCondition": "Accepted.",
                "iterationTemplate": "Do one wave.",
                "tasks": [
                    {
                        "taskId": "tp-loop-boundary-i001-01",
                        "title": "Loop task",
                        "assignedTo": "@worker:domain",
                        "dependsOn": [],
                    },
                ],
            },
        ),
    )["ok"] is True

    response = await projectflow(
        action="ready_nodes",
        payload={"projectId": "tp-loop-boundary"},
    )
    payload = _response_json(response)
    assert payload["ok"] is False
    assert payload["error"] == "project plan is not a DAG: tp-loop-boundary"

    response = await projectflow(
        action="ready_loop_nodes",
        payload={"projectId": "tp-loop-boundary"},
    )
    payload = _response_json(response)
    assert payload["ok"] is True
    assert [task["task_id"] for task in payload["readyNodes"]] == [
        "tp-loop-boundary-i001-01",
    ]


@pytest.mark.asyncio
async def test_projectflow_record_loop_iteration_updates_history(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))

    assert _response_json(
        await projectflow(
            action="create_project",
            payload={"projectId": "tp-loop-history", "title": "Loop history project"},
        ),
    )["ok"] is True
    assert _response_json(
        await projectflow(
            action="plan_loop",
            payload={
                "projectId": "tp-loop-history",
                "goal": "Research until enough evidence exists.",
                "maxIterations": 3,
                "stopCondition": "Coverage sufficient.",
                "iterationTemplate": "Research, synthesize, decide.",
            },
        ),
    )["ok"] is True

    response = await projectflow(
        action="record_loop_iteration",
        payload={
            "projectId": "tp-loop-history",
            "iteration": 1,
            "decision": "continue",
            "summary": "Coverage insufficient.",
            "nextAction": "Run a second research wave.",
        },
    )
    payload = _response_json(response)
    assert payload["ok"] is True
    assert payload["loop"]["status"] == "running"
    assert payload["loop"]["current_iteration"] == 1

    plan = (workspace / "shared" / "projects" / "tp-loop-history" / "plan.md").read_text()
    assert "- Iteration 1: continue — Coverage insufficient. Next: Run a second research wave." in plan


@pytest.mark.asyncio
async def test_project_lifecycle_actions_only_update_meta_status(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))

    response = await projectflow(
        action="create_project",
        payload={
            "projectId": "tp-status",
            "title": "Lifecycle project",
        },
    )
    assert _response_json(response)["ok"] is True

    plan = (workspace / "shared" / "projects" / "tp-status" / "plan.md").read_text()
    assert "**Status**:" not in plan

    response = await projectflow(action="pause_project", payload={"projectId": "tp-status"})
    payload = _response_json(response)
    assert payload["ok"] is True
    assert payload["project"]["status"] == "paused"
    meta = json.loads((workspace / "shared" / "projects" / "tp-status" / "meta.json").read_text())
    assert meta["status"] == "paused"
    plan = (workspace / "shared" / "projects" / "tp-status" / "plan.md").read_text()
    assert "**Status**:" not in plan

    response = await projectflow(action="resume_project", payload={"projectId": "tp-status"})
    payload = _response_json(response)
    assert payload["ok"] is True
    assert payload["project"]["status"] == "active"
    meta = json.loads((workspace / "shared" / "projects" / "tp-status" / "meta.json").read_text())
    assert meta["status"] == "active"
    plan = (workspace / "shared" / "projects" / "tp-status" / "plan.md").read_text()
    assert "**Status**:" not in plan

    response = await projectflow(action="complete_project", payload={"projectId": "tp-status"})
    payload = _response_json(response)
    assert payload["ok"] is True
    assert payload["project"]["status"] == "completed"
    meta = json.loads((workspace / "shared" / "projects" / "tp-status" / "meta.json").read_text())
    assert meta["status"] == "completed"
    plan = (workspace / "shared" / "projects" / "tp-status" / "plan.md").read_text()
    assert "**Status**:" not in plan


@pytest.mark.asyncio
async def test_check_task_reports_interrupted_as_ineffective(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@worker:domain")
    _mock_sync(monkeypatch)

    task_dir = workspace / "shared" / "tasks" / "st-01"
    task_dir.mkdir(parents=True)
    (task_dir / "meta.json").write_text(
        json.dumps(
            {
                "task_id": "st-01",
                "project_id": "tp-01",
                "task_title": "Task",
                "assigned_to": "@worker:domain",
                "room_id": "room:!team-room:domain",
                "status": "in_progress",
                "depends_on": [],
            },
        ),
    )

    response = await taskflow(
        action="submit_task",
        payload={
            "taskId": "st-01",
            "status": "INTERRUPTED",
            "summary": "Coordinator interrupted this attempt.",
            "deliverables": [],
        },
    )
    assert _response_json(response)["ok"] is True

    response = await taskflow(action="check_task", payload={"taskId": "st-01"})
    payload = _response_json(response)

    assert payload["ok"] is True
    assert payload["result"]["status"] == "INTERRUPTED"
    assert payload["effective"] is False


@pytest.mark.asyncio
async def test_projectflow_ready_nodes_rejects_ineffective_dependency(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@worker:domain")
    _mock_sync(monkeypatch)

    response = await projectflow(
        action="create_project",
        payload={
            "projectId": "tp-interrupt",
            "title": "Interrupted project",
        },
    )
    assert _response_json(response)["ok"] is True

    response = await projectflow(
        action="plan_dag",
        payload={
            "projectId": "tp-interrupt",
            "tasks": [
                {
                    "taskId": "tp-interrupt-01",
                    "title": "Interruptible task",
                    "assignedTo": "@worker:domain",
                    "dependsOn": [],
                },
                {
                    "taskId": "tp-interrupt-02",
                    "title": "Blocked follow-up",
                    "assignedTo": "@worker:domain",
                    "dependsOn": ["tp-interrupt-01"],
                },
            ],
        },
    )
    assert _response_json(response)["ok"] is True

    response = await taskflow(
        action="delegate_task",
        payload={
            "projectId": "tp-interrupt",
            "taskId": "tp-interrupt-01",
            "roomId": "room:!team-room:domain",
            "spec": "Do work.",
        },
    )
    assert _response_json(response)["ok"] is True

    result_path = workspace / "shared" / "tasks" / "tp-interrupt-01" / "result.md"
    result_path.write_text(
        "STATUS: INTERRUPTED\n"
        "SUMMARY: Coordinator interrupted this attempt.\n\n"
        "DELIVERABLES:\n",
    )

    response = await projectflow(action="ready_nodes", payload={"projectId": "tp-interrupt"})
    payload = _response_json(response)

    assert payload["ok"] is True
    assert payload["readyNodes"] == []


def test_add_tasks_rejects_unknown_dependency(tmp_path):
    store = FileSystemTaskStore(tmp_path)
    create_project(store, project_id="tp-01", title="Bad graph")

    with pytest.raises(TaskflowError, match="unknown task"):
        add_tasks(
            store,
            project_id="tp-01",
            tasks=[
                {
                    "taskId": "st-02",
                    "title": "Blocked task",
                    "assignedTo": "@worker:domain",
                    "dependsOn": ["st-01"],
                },
            ],
        )


@pytest.mark.asyncio
async def test_submit_task_rejects_invalid_deliverable_path(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@worker:domain")

    task_dir = workspace / "shared" / "tasks" / "st-01"
    task_dir.mkdir(parents=True)
    (task_dir / "meta.json").write_text(
        json.dumps(
            {
                "task_id": "st-01",
                "project_id": "tp-01",
                "task_title": "Task",
                "assigned_to": "@worker:domain",
                "room_id": "room:!team-room:domain",
                "status": "in_progress",
                "depends_on": [],
            },
        ),
    )
    (task_dir / "result.md").write_text(
        "STATUS: SUCCESS\n"
        "SUMMARY: Done.\n\n"
        "DELIVERABLES:\n"
        "- shared/projects/tp-01/result.md\n",
    )

    response = await taskflow(action="submit_task", payload={"taskId": "st-01"})
    payload = _response_json(response)
    assert payload["ok"] is False
    assert "deliverable must be under shared/tasks/st-01/" in payload["error"]


@pytest.mark.asyncio
async def test_ack_task_returns_spec_and_calls_sync(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@worker:domain")
    mock = _mock_sync(monkeypatch)

    task_dir = workspace / "shared" / "tasks" / "st-01"
    task_dir.mkdir(parents=True)
    (task_dir / "meta.json").write_text(
        json.dumps(
            {
                "task_id": "st-01",
                "project_id": "tp-01",
                "task_title": "Task",
                "assigned_to": "@worker:domain",
                "room_id": "room:!team-room:domain",
                "status": "assigned",
                "depends_on": [],
            },
        ),
    )
    (task_dir / "spec.md").write_text("# Research Task\n\nCollect sources and summarize.\n")

    response = await taskflow(action="ack_task", payload={"taskId": "st-01"})
    payload = _response_json(response)

    assert payload["ok"] is True
    assert payload["task"]["status"] == "in_progress"
    assert payload["spec"] == "# Research Task\n\nCollect sources and summarize.\n"
    mock.pull_shared_path.assert_called_once_with("shared/tasks/st-01/")
    mock.push_shared_path.assert_called_once_with(
        "shared/tasks/st-01/", exclude=["spec.md", "base/"],
    )


@pytest.mark.asyncio
async def test_submit_task_calls_sync_and_stat(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@worker:domain")
    mock = _mock_sync(monkeypatch)

    task_dir = workspace / "shared" / "tasks" / "st-01"
    task_dir.mkdir(parents=True)
    (task_dir / "meta.json").write_text(
        json.dumps(
            {
                "task_id": "st-01",
                "project_id": "tp-01",
                "task_title": "Task",
                "assigned_to": "@worker:domain",
                "room_id": "room:!team-room:domain",
                "status": "in_progress",
                "depends_on": [],
            },
        ),
    )
    deliverable_path = task_dir / "workspace" / "output.md"
    deliverable_path.parent.mkdir(parents=True, exist_ok=True)
    deliverable_path.write_text("output artifact\n", encoding="utf-8")

    response = await taskflow(
        action="submit_task",
        payload={
            "taskId": "st-01",
            "status": "SUCCESS",
            "summary": "Done.",
            "deliverables": ["shared/tasks/st-01/workspace/output.md"],
        },
    )
    payload = _response_json(response)

    assert payload["ok"] is True
    assert payload["task"]["status"] == "submitted"
    assert payload["synced"] is True
    assert payload["verified"] is True
    mock.push_shared_path.assert_called_once_with(
        "shared/tasks/st-01/", exclude=["spec.md", "base/"],
    )
    assert mock.stat_shared_path.call_count == 2
    mock.stat_shared_path.assert_any_call("shared/tasks/st-01/result.md")
    mock.stat_shared_path.assert_any_call("shared/tasks/st-01/workspace/output.md")


@pytest.mark.asyncio
async def test_submit_task_fails_when_deliverable_missing_locally(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@worker:domain")
    mock = _mock_sync(monkeypatch)

    task_dir = workspace / "shared" / "tasks" / "st-01"
    task_dir.mkdir(parents=True)
    (task_dir / "meta.json").write_text(
        json.dumps(
            {
                "task_id": "st-01",
                "project_id": "tp-01",
                "task_title": "Task",
                "assigned_to": "@worker:domain",
                "room_id": "room:!team-room:domain",
                "status": "in_progress",
                "depends_on": [],
            },
        ),
    )

    response = await taskflow(
        action="submit_task",
        payload={
            "taskId": "st-01",
            "status": "SUCCESS",
            "summary": "Done.",
            "deliverables": ["shared/tasks/st-01/workspace/missing.md"],
        },
    )
    payload = _response_json(response)

    assert payload["ok"] is False
    assert "artifact verification failed" in payload["error"]
    meta = json.loads((task_dir / "meta.json").read_text())
    assert meta["status"] == "in_progress"
    mock.push_shared_path.assert_not_called()


@pytest.mark.asyncio
async def test_check_task_pulls_and_returns_meta(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    mock = _mock_sync(monkeypatch)

    task_dir = workspace / "shared" / "tasks" / "st-01"
    task_dir.mkdir(parents=True)
    (task_dir / "meta.json").write_text(
        json.dumps(
            {
                "task_id": "st-01",
                "project_id": "tp-01",
                "task_title": "Task",
                "assigned_to": "@worker:domain",
                "room_id": "room:!team-room:domain",
                "status": "submitted",
                "depends_on": [],
            },
        ),
    )
    (task_dir / "result.md").write_text(
        "STATUS: SUCCESS\n"
        "SUMMARY: Work completed.\n\n"
        "DELIVERABLES:\n"
        "- shared/tasks/st-01/workspace/output.md\n",
    )

    response = await taskflow(action="check_task", payload={"taskId": "st-01"})
    payload = _response_json(response)

    assert payload["ok"] is True
    assert payload["task"]["status"] == "submitted"
    assert payload["task"]["assigned_to"] == "@worker:domain"
    assert payload["result"]["status"] == "SUCCESS"
    assert payload["result"]["summary"] == "Work completed."
    assert payload["effective"] is True
    mock.pull_shared_path.assert_called_once_with("shared/tasks/st-01/")


@pytest.mark.asyncio
async def test_delegate_task_pushes_after_creation(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@leader:domain")
    mock = _mock_sync(monkeypatch)

    response = await projectflow(
        action="create_project",
        payload={"projectId": "tp-push", "title": "Push test project"},
    )
    assert _response_json(response)["ok"] is True

    response = await projectflow(
        action="plan_dag",
        payload={
            "projectId": "tp-push",
            "tasks": [
                {
                    "taskId": "tp-push-01",
                    "title": "Pushable task",
                    "assignedTo": "@worker:domain",
                    "dependsOn": [],
                },
            ],
        },
    )
    assert _response_json(response)["ok"] is True

    response = await taskflow(
        action="delegate_task",
        payload={
            "projectId": "tp-push",
            "taskId": "tp-push-01",
            "roomId": "room:!team-room:domain",
            "spec": "Do the work.",
        },
    )
    payload = _response_json(response)

    assert payload["ok"] is True
    assert payload["task"]["status"] == "assigned"
    assert payload["synced"] is True
    mock.push_shared_path.assert_called_once_with("shared/tasks/tp-push-01/")


@pytest.mark.asyncio
async def test_ack_task_missing_spec_does_not_write_in_progress(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@worker:domain")
    _mock_sync(monkeypatch)

    task_dir = workspace / "shared" / "tasks" / "st-01"
    task_dir.mkdir(parents=True)
    (task_dir / "meta.json").write_text(
        json.dumps(
            {
                "task_id": "st-01",
                "project_id": "tp-01",
                "task_title": "Task",
                "assigned_to": "@worker:domain",
                "room_id": "room:!team-room:domain",
                "status": "assigned",
                "depends_on": [],
            },
        ),
    )

    response = await taskflow(action="ack_task", payload={"taskId": "st-01"})
    payload = _response_json(response)

    assert payload["ok"] is False
    assert "spec" in payload["error"]
    meta = json.loads((task_dir / "meta.json").read_text())
    assert meta["status"] == "assigned"


@pytest.mark.asyncio
async def test_delegate_task_runs_blocking_sync_off_event_loop(tmp_path, monkeypatch):
    """Regression test for event-loop blocking: push_shared_path must run in a worker thread."""
    working_dir = tmp_path / "worker" / ".copaw"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@leader:domain")

    main_thread = threading.current_thread()
    seen = {}

    class FakeSync:
        def push_shared_path(self, path, exclude=None):
            seen["thread"] = threading.current_thread()
            seen["path"] = path

    monkeypatch.setattr(taskflow_tool, "create_sync", lambda: FakeSync())

    response = await projectflow(
        action="create_project",
        payload={"projectId": "tp-thread", "title": "Thread test project"},
    )
    assert _response_json(response)["ok"] is True

    response = await projectflow(
        action="plan_dag",
        payload={
            "projectId": "tp-thread",
            "tasks": [
                {
                    "taskId": "tp-thread-01",
                    "title": "Threaded task",
                    "assignedTo": "@worker:domain",
                    "dependsOn": [],
                },
            ],
        },
    )
    assert _response_json(response)["ok"] is True

    response = await taskflow(
        action="delegate_task",
        payload={
            "projectId": "tp-thread",
            "taskId": "tp-thread-01",
            "roomId": "room:!team-room:domain",
            "spec": "Do the work.",
        },
    )
    payload = _response_json(response)

    assert payload["ok"] is True
    assert seen["thread"] is not main_thread
    assert seen["path"] == "shared/tasks/tp-thread-01/"
