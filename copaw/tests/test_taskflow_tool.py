from concurrent.futures import ThreadPoolExecutor
import json
import os
from pathlib import Path
import threading
from unittest.mock import MagicMock
from uuid import UUID

import pytest

import copaw_worker.hooks.tools.projectflow as projectflow_tool
import copaw_worker.hooks.tools.taskflow as taskflow_tool
from copaw_worker.hooks.tools.projectflow import projectflow
from copaw_worker.hooks.tools.taskflow import taskflow
from copaw_worker.task import (
    FileSystemTaskStore,
    TaskMeta,
    TaskResult,
    TaskflowError,
    accept_task_result,
    add_tasks,
    cancel_task,
    create_project,
    parse_dag_tasks,
    parse_loop_plan,
    submit_task,
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


def _mock_project_sync(monkeypatch) -> MagicMock:
    mock = MagicMock()
    monkeypatch.setattr(projectflow_tool, "create_sync", lambda: mock)
    return mock


def _write_submitted_task(
    workspace: Path,
    *,
    project_id: str = "tp-decision",
    task_id: str = "st-decision",
    result_status: str = "SUCCESS",
) -> dict:
    store = FileSystemTaskStore(workspace)
    create_project(store, project_id=project_id, title="Decision project")
    from copaw_worker.task import plan_dag
    plan_dag(
        store,
        project_id=project_id,
        tasks=[{
            "taskId": task_id,
            "title": "Decide this result",
            "assignedTo": "@worker:domain",
            "dependsOn": [],
        }],
    )
    task_dir = workspace / "shared" / "tasks" / task_id
    task_dir.mkdir(parents=True)
    store.write_task_meta(TaskMeta(
        task_id=task_id,
        project_id=project_id,
        task_title="Decide this result",
        assigned_to="@worker:domain",
        room_id="room:!team:domain",
        status="in_progress",
    ))
    submitted = submit_task(
        store,
        task_id=task_id,
        actor="@worker:domain",
        result=TaskResult(status=result_status, summary="Worker result."),
    )
    return submitted.__dict__


@pytest.mark.asyncio
@pytest.mark.parametrize("action", ["accept_task_result", "cancel_task"])
@pytest.mark.parametrize("dry_run", [False, True])
async def test_projectflow_decision_requires_submission_id_before_side_effects(
    tmp_path,
    monkeypatch,
    action,
    dry_run,
):
    leader_dir = tmp_path / "leader"
    working_dir = leader_dir / ".copaw"
    workspace = working_dir / "workspaces/default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _write_team_leader_runtime_config(leader_dir)
    _set_actor(monkeypatch, "@leader:domain")
    sync = _mock_project_sync(monkeypatch)
    _write_submitted_task(workspace)
    task_meta_path = workspace / "shared/tasks/st-decision/meta.json"
    result_path = workspace / "shared/tasks/st-decision/result.md"
    plan_path = workspace / "shared/projects/tp-decision/plan.md"
    before = {
        task_meta_path: task_meta_path.read_bytes(),
        result_path: result_path.read_bytes(),
        plan_path: plan_path.read_bytes(),
    }
    payload = {
        "projectId": "tp-decision",
        "taskId": "st-decision",
        **(
            {"accepted": True}
            if action == "accept_task_result"
            else {"reason": "No longer needed."}
        ),
    }

    response = _response_json(await projectflow(
        action=action,
        payload=payload,
        dryRun=dry_run,
    ))

    assert response["ok"] is False
    assert "payload.submissionId is required" in response["error"]
    assert {path: path.read_bytes() for path in before} == before
    assert sync.method_calls == []


@pytest.mark.parametrize("operation", ["accept", "cancel"])
def test_domain_decision_requires_submission_id_before_state_write(
    tmp_path,
    operation,
):
    submitted = _write_submitted_task(tmp_path)
    store = FileSystemTaskStore(tmp_path)
    task_meta_path = tmp_path / "shared/tasks/st-decision/meta.json"
    result_path = tmp_path / "shared/tasks/st-decision/result.md"
    plan_path = tmp_path / "shared/projects/tp-decision/plan.md"
    before = {
        task_meta_path: task_meta_path.read_bytes(),
        result_path: result_path.read_bytes(),
        plan_path: plan_path.read_bytes(),
    }

    with pytest.raises(TaskflowError, match="submissionId is required"):
        if operation == "accept":
            accept_task_result(
                store,
                project_id="tp-decision",
                task_id="st-decision",
                accepted=True,
                submission_id=None,
            )
        else:
            cancel_task(
                store,
                project_id="tp-decision",
                task_id="st-decision",
                reason="No longer needed.",
                submission_id=None,
            )

    assert {path: path.read_bytes() for path in before} == before
    assert submitted["submission_id"]


@pytest.mark.asyncio
async def test_leader_accepts_submitted_result_and_resolves_continuation(
    tmp_path,
    monkeypatch,
):
    leader_dir = tmp_path / "leader"
    working_dir = leader_dir / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _write_team_leader_runtime_config(leader_dir)
    _set_actor(monkeypatch, "@leader:domain")
    sync = _mock_project_sync(monkeypatch)
    submitted = _write_submitted_task(workspace)

    response = await projectflow(
        action="accept_task_result",
        payload={
            "projectId": "tp-decision",
            "taskId": "st-decision",
            "submissionId": submitted["submission_id"],
            "accepted": True,
        },
    )
    payload = _response_json(response)

    assert payload["ok"] is True
    assert payload["task"]["status"] == "completed"
    continuation = payload["task"]["continuation"]
    assert continuation["delivery_id"] == submitted["continuation"]["delivery_id"]
    assert continuation["status"] == "resolved"
    assert continuation["resolution"] == "completed"
    assert continuation["resolved_at"]
    plan = (workspace / "shared/projects/tp-decision/plan.md").read_text()
    assert "- [x] st-decision" in plan
    assert sync.push_shared_path.call_args_list == [
        (("shared/projects/tp-decision/",), {}),
        (("shared/tasks/st-decision/",), {"exclude": ["spec.md", "base/"]}),
    ]


@pytest.mark.asyncio
@pytest.mark.parametrize(
    ("result_status", "accepted", "expected_status"),
    [
        ("SUCCESS_WITH_NOTES", True, "completed"),
        ("REVISION_NEEDED", True, "revision"),
        ("BLOCKED", True, "blocked"),
        ("INTERRUPTED", True, "blocked"),
        ("SUCCESS", False, "revision"),
        ("SUCCESS_WITH_NOTES", False, "revision"),
        ("REVISION_NEEDED", False, "revision"),
        ("BLOCKED", False, "blocked"),
        ("INTERRUPTED", False, "blocked"),
    ],
)
async def test_leader_decision_maps_result_status_to_terminal_state(
    tmp_path,
    monkeypatch,
    result_status,
    accepted,
    expected_status,
):
    leader_dir = tmp_path / "leader"
    working_dir = leader_dir / ".copaw"
    workspace = working_dir / "workspaces/default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _write_team_leader_runtime_config(leader_dir)
    _set_actor(monkeypatch, "@leader:domain")
    _mock_project_sync(monkeypatch)
    submitted = _write_submitted_task(workspace, result_status=result_status)

    payload = _response_json(await projectflow(
        action="accept_task_result",
        payload={
            "projectId": "tp-decision",
            "taskId": "st-decision",
            "submissionId": submitted["submission_id"],
            "accepted": accepted,
        },
    ))

    assert payload["ok"] is True
    assert payload["task"]["status"] == expected_status
    assert payload["task"]["continuation"]["resolution"] == expected_status


@pytest.mark.asyncio
async def test_leader_cancels_submitted_task_and_retry_is_idempotent(tmp_path, monkeypatch):
    leader_dir = tmp_path / "leader"
    working_dir = leader_dir / ".copaw"
    workspace = working_dir / "workspaces/default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _write_team_leader_runtime_config(leader_dir)
    _set_actor(monkeypatch, "@leader:domain")
    _mock_project_sync(monkeypatch)
    submitted = _write_submitted_task(workspace)
    request = {
        "projectId": "tp-decision",
        "taskId": "st-decision",
        "submissionId": submitted["submission_id"],
        "reason": "No longer needed.",
        "replacementTaskId": "st-replacement",
    }

    first = _response_json(await projectflow(action="cancel_task", payload=request))
    second = _response_json(await projectflow(action="cancel_task", payload=request))

    assert first["ok"] is True
    assert first["reused"] is False
    assert first["task"]["status"] == "cancelled"
    assert first["task"]["continuation"]["resolution"] == "cancelled"
    assert first["task"]["cancel_reason"] == "No longer needed."
    assert first["task"]["replacement_task_id"] == "st-replacement"
    assert first["task"]["cancelled_at"]
    assert second["ok"] is True
    assert second["reused"] is True
    assert second["task"] == first["task"]

    meta_path = workspace / "shared/tasks/st-decision/meta.json"
    original_bytes = meta_path.read_bytes()
    conflict = _response_json(await projectflow(
        action="cancel_task",
        payload={**request, "reason": "Different reason."},
    ))
    assert conflict["ok"] is False
    assert "conflicting cancellation" in conflict["error"]
    assert meta_path.read_bytes() == original_bytes


@pytest.mark.asyncio
@pytest.mark.parametrize("result_artifact", ["missing", "tampered"])
async def test_cancel_task_does_not_depend_on_result_artifact_integrity(
    tmp_path,
    monkeypatch,
    result_artifact,
):
    leader_dir = tmp_path / "leader"
    working_dir = leader_dir / ".copaw"
    workspace = working_dir / "workspaces/default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _write_team_leader_runtime_config(leader_dir)
    _set_actor(monkeypatch, "@leader:domain")
    _mock_project_sync(monkeypatch)
    submitted = _write_submitted_task(workspace)
    result_path = workspace / "shared/tasks/st-decision/result.md"
    if result_artifact == "missing":
        result_path.unlink()
    else:
        result_path.write_text(
            "STATUS: SUCCESS\nSUMMARY: Tampered after submission.\n\nDELIVERABLES:\n",
        )

    response = _response_json(await projectflow(
        action="cancel_task",
        payload={
            "projectId": "tp-decision",
            "taskId": "st-decision",
            "submissionId": submitted["submission_id"],
            "reason": "The work is no longer required.",
        },
    ))

    assert response["ok"] is True
    assert response["task"]["status"] == "cancelled"
    assert response["task"]["continuation"]["status"] == "resolved"
    assert response["task"]["continuation"]["resolution"] == "cancelled"
    plan = (workspace / "shared/projects/tp-decision/plan.md").read_text()
    assert "- [-] st-decision" in plan


@pytest.mark.asyncio
async def test_cancel_task_still_rejects_stale_submission_when_result_is_missing(
    tmp_path,
    monkeypatch,
):
    leader_dir = tmp_path / "leader"
    working_dir = leader_dir / ".copaw"
    workspace = working_dir / "workspaces/default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _write_team_leader_runtime_config(leader_dir)
    _set_actor(monkeypatch, "@leader:domain")
    sync = _mock_project_sync(monkeypatch)
    _write_submitted_task(workspace)
    (workspace / "shared/tasks/st-decision/result.md").unlink()

    response = _response_json(await projectflow(
        action="cancel_task",
        payload={
            "projectId": "tp-decision",
            "taskId": "st-decision",
            "submissionId": "stale-submission",
            "reason": "The work is no longer required.",
        },
    ))

    assert response["ok"] is False
    assert "stale submissionId" in response["error"]
    persisted = json.loads(
        (workspace / "shared/tasks/st-decision/meta.json").read_text(),
    )
    assert persisted["status"] == "submitted"
    plan = (workspace / "shared/projects/tp-decision/plan.md").read_text()
    assert "- [ ] st-decision" in plan
    assert sync.method_calls == []


@pytest.mark.asyncio
async def test_cancel_task_tool_requires_reason(tmp_path, monkeypatch):
    leader_dir = tmp_path / "leader"
    working_dir = leader_dir / ".copaw"
    workspace = working_dir / "workspaces/default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _write_team_leader_runtime_config(leader_dir)
    _set_actor(monkeypatch, "@leader:domain")
    _mock_project_sync(monkeypatch)
    submitted = _write_submitted_task(workspace)

    response = _response_json(await projectflow(
        action="cancel_task",
        payload={
            "projectId": "tp-decision",
            "taskId": "st-decision",
            "submissionId": submitted["submission_id"],
        },
    ))

    assert response["ok"] is False
    assert "payload.reason is required" in response["error"]


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "replacement_task_id",
    ["../st-next", "", " ", "st next", "st/next", r"st\next"],
)
async def test_cancel_task_rejects_unsafe_replacement_id_without_side_effects(
    tmp_path,
    monkeypatch,
    replacement_task_id,
):
    leader_dir = tmp_path / "leader"
    working_dir = leader_dir / ".copaw"
    workspace = working_dir / "workspaces/default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _write_team_leader_runtime_config(leader_dir)
    _set_actor(monkeypatch, "@leader:domain")
    sync = _mock_project_sync(monkeypatch)
    submitted = _write_submitted_task(workspace)

    plan_path = workspace / "shared/projects/tp-decision/plan.md"
    meta_path = workspace / "shared/tasks/st-decision/meta.json"
    result_path = workspace / "shared/tasks/st-decision/result.md"
    original_plan = plan_path.read_bytes()
    original_meta = meta_path.read_bytes()
    original_result = result_path.read_bytes()

    response = _response_json(await projectflow(
        action="cancel_task",
        payload={
            "projectId": "tp-decision",
            "taskId": "st-decision",
            "submissionId": submitted["submission_id"],
            "reason": "Replace the task safely.",
            "replacementTaskId": replacement_task_id,
        },
    ))

    assert response["ok"] is False
    assert "invalid id" in response["error"]
    assert plan_path.read_bytes() == original_plan
    assert meta_path.read_bytes() == original_meta
    assert result_path.read_bytes() == original_result
    assert sync.method_calls == []


@pytest.mark.asyncio
async def test_accept_task_result_retry_is_idempotent_but_conflicting_decision_is_rejected(
    tmp_path,
    monkeypatch,
):
    leader_dir = tmp_path / "leader"
    working_dir = leader_dir / ".copaw"
    workspace = working_dir / "workspaces/default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _write_team_leader_runtime_config(leader_dir)
    _set_actor(monkeypatch, "@leader:domain")
    _mock_project_sync(monkeypatch)
    submitted = _write_submitted_task(workspace)
    request = {
        "projectId": "tp-decision",
        "taskId": "st-decision",
        "submissionId": submitted["submission_id"],
        "accepted": True,
    }

    first = _response_json(await projectflow(action="accept_task_result", payload=request))
    retry = _response_json(await projectflow(action="accept_task_result", payload=request))
    conflict = _response_json(await projectflow(
        action="accept_task_result",
        payload={**request, "accepted": False},
    ))

    assert first["ok"] is True and first["reused"] is False
    assert retry["ok"] is True and retry["reused"] is True
    assert retry["task"] == first["task"]
    assert conflict["ok"] is False
    assert "conflicting decision" in conflict["error"]


@pytest.mark.asyncio
@pytest.mark.parametrize("case", ["stale", "missing", "tampered", "wrong_project"])
async def test_accept_task_result_rejects_invalid_submission_evidence(
    tmp_path,
    monkeypatch,
    case,
):
    leader_dir = tmp_path / "leader"
    working_dir = leader_dir / ".copaw"
    workspace = working_dir / "workspaces/default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _write_team_leader_runtime_config(leader_dir)
    _set_actor(monkeypatch, "@leader:domain")
    sync = _mock_project_sync(monkeypatch)
    submitted = _write_submitted_task(workspace)
    request = {
        "projectId": "tp-decision",
        "taskId": "st-decision",
        "submissionId": submitted["submission_id"],
        "accepted": True,
    }
    if case == "stale":
        request["submissionId"] = "stale-submission"
    elif case == "missing":
        (workspace / "shared/tasks/st-decision/result.md").unlink()
    elif case == "tampered":
        (workspace / "shared/tasks/st-decision/result.md").write_text(
            "STATUS: SUCCESS\nSUMMARY: Tampered.\n\nDELIVERABLES:\n",
        )
    else:
        request["projectId"] = "tp-other"

    response = _response_json(await projectflow(action="accept_task_result", payload=request))

    assert response["ok"] is False
    assert response["error"]
    assert sync.push_shared_path.call_count == 0
    meta = json.loads((workspace / "shared/tasks/st-decision/meta.json").read_text())
    assert meta["status"] == "submitted"


@pytest.mark.asyncio
async def test_task_result_decision_requires_team_leader_role(tmp_path, monkeypatch):
    worker_dir = tmp_path / "worker"
    working_dir = worker_dir / ".copaw"
    workspace = working_dir / "workspaces/default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    runtime_dir = worker_dir / "runtime"
    runtime_dir.mkdir(parents=True)
    (runtime_dir / "runtime.yaml").write_text("member:\n  role: worker\n")
    _set_actor(monkeypatch, "@worker:domain")
    _mock_project_sync(monkeypatch)
    submitted = _write_submitted_task(workspace)

    response = _response_json(await projectflow(
        action="accept_task_result",
        payload={
            "projectId": "tp-decision",
            "taskId": "st-decision",
            "submissionId": submitted["submission_id"],
            "accepted": True,
        },
    ))

    assert response["ok"] is False
    assert "requires team_leader role" in response["error"]


@pytest.mark.asyncio
async def test_accept_task_result_sync_failure_returns_retryable_persisted_state(
    tmp_path,
    monkeypatch,
):
    leader_dir = tmp_path / "leader"
    working_dir = leader_dir / ".copaw"
    workspace = working_dir / "workspaces/default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _write_team_leader_runtime_config(leader_dir)
    _set_actor(monkeypatch, "@leader:domain")
    sync = _mock_project_sync(monkeypatch)
    sync.push_shared_path.side_effect = RuntimeError("remote unavailable")
    submitted = _write_submitted_task(workspace)

    response = _response_json(await projectflow(
        action="accept_task_result",
        payload={
            "projectId": "tp-decision",
            "taskId": "st-decision",
            "submissionId": submitted["submission_id"],
            "accepted": True,
        },
    ))

    assert response["ok"] is False
    assert response["retryable"] is True
    assert response["statePersisted"] is True
    assert response["synced"] is False
    assert response["task"]["status"] == "completed"
    persisted = json.loads((workspace / "shared/tasks/st-decision/meta.json").read_text())
    assert persisted["status"] == "completed"


@pytest.mark.asyncio
async def test_project_plan_terminal_fences_opposite_retry_after_task_meta_write_failure(
    tmp_path,
    monkeypatch,
):
    leader_dir = tmp_path / "leader"
    working_dir = leader_dir / ".copaw"
    workspace = working_dir / "workspaces/default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _write_team_leader_runtime_config(leader_dir)
    _set_actor(monkeypatch, "@leader:domain")
    _mock_project_sync(monkeypatch)
    submitted = _write_submitted_task(workspace)
    original_write = FileSystemTaskStore.write_task_meta
    failures = {"remaining": 1}

    def fail_terminal_task_meta_once(store, meta):
        if meta.status == "completed" and failures["remaining"]:
            failures["remaining"] -= 1
            raise OSError("simulated task-meta write failure")
        return original_write(store, meta)

    monkeypatch.setattr(FileSystemTaskStore, "write_task_meta", fail_terminal_task_meta_once)
    request = {
        "projectId": "tp-decision",
        "taskId": "st-decision",
        "submissionId": submitted["submission_id"],
        "accepted": True,
    }

    first = _response_json(await projectflow(action="accept_task_result", payload=request))
    plan_after_failure = (workspace / "shared/projects/tp-decision/plan.md").read_text()
    meta_after_failure = json.loads(
        (workspace / "shared/tasks/st-decision/meta.json").read_text(),
    )
    opposite = _response_json(await projectflow(
        action="accept_task_result",
        payload={**request, "accepted": False},
    ))
    same_retry = _response_json(await projectflow(action="accept_task_result", payload=request))

    assert first["ok"] is False
    assert "simulated task-meta write failure" in first["error"]
    assert first["retryable"] is True
    assert first["statePersisted"] is True
    assert first["synced"] is False
    assert first["task"]["status"] == "completed"
    assert "- [x] st-decision" in plan_after_failure
    assert meta_after_failure["status"] == "submitted"
    assert opposite["ok"] is False
    assert "conflicting decision" in opposite["error"]
    assert (workspace / "shared/projects/tp-decision/plan.md").read_text() == plan_after_failure
    assert same_retry["ok"] is True
    assert same_retry["task"]["status"] == "completed"
    assert same_retry["task"]["continuation"]["resolution"] == "completed"


def _mock_notify(monkeypatch) -> None:
    """Patch _notify_task_assignment to return a success result."""

    async def fake_notify(**kwargs):
        return {
            "sent": True,
            "eventId": "$fake-event-id",
            "roomId": kwargs.get("room_id", ""),
            "assignee": "@worker:domain",
        }

    monkeypatch.setattr(taskflow_tool, "_notify_task_assignment", fake_notify)


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
    _mock_notify(monkeypatch)

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
    _mock_notify(monkeypatch)

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
    _mock_notify(monkeypatch)

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
    _mock_notify(monkeypatch)

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
    _mock_notify(monkeypatch)

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
    # Auto-notification path (PR #1095): delegate_task sends the Matrix
    # notification itself with a stable txn_id, then records event_id and
    # marks the task assigned. No notificationRequired/nextAction handoff.
    assert payload["notification"] == {
        "sent": True,
        "eventId": "$fake-event-id",
        "roomId": "room:!team:domain",
        "assignee": "@worker:domain",
    }
    assert payload["task"]["status"] == "assigned"
    assert payload["task"]["event_id"] == "$fake-event-id"


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
async def test_submit_task_reuses_identity_for_same_result(tmp_path, monkeypatch):
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
    request = {
        "taskId": "st-01",
        "status": "SUCCESS",
        "summary": "API design\ncompleted.",
        "deliverables": ["shared/tasks/st-01/workspace/api-design.md"],
    }

    first = _response_json(await taskflow(action="submit_task", payload=request))
    first_meta = (task_dir / "meta.json").read_bytes()
    first_result = (task_dir / "result.md").read_bytes()
    second = _response_json(await taskflow(action="submit_task", payload=request))

    assert first["ok"] is True
    assert first["reused"] is False
    UUID(first["task"]["submission_id"])
    assert first["task"]["submitted_at"]
    continuation = first["task"]["continuation"]
    assert continuation["status"] == "pending"
    assert len(continuation["delivery_id"]) == 64
    int(continuation["delivery_id"], 16)
    assert second["ok"] is True
    assert second["reused"] is True
    assert second["task"]["submission_id"] == first["task"]["submission_id"]
    assert second["task"]["submitted_at"] == first["task"]["submitted_at"]
    assert second["task"]["continuation"] == continuation
    assert (task_dir / "meta.json").read_bytes() == first_meta
    assert (task_dir / "result.md").read_bytes() == first_result


def test_submit_task_domain_api_still_returns_task_meta(tmp_path):
    store = FileSystemTaskStore(tmp_path)
    task_dir = tmp_path / "shared" / "tasks" / "st-01"
    task_dir.mkdir(parents=True)
    (task_dir / "meta.json").write_text(
        json.dumps(
            {
                "task_id": "st-01",
                "project_id": "tp-01",
                "task_title": "Task",
                "assigned_to": "worker",
                "room_id": "room:!team-room:domain",
                "status": "in_progress",
                "depends_on": [],
            },
        ),
    )

    meta = submit_task(
        store,
        task_id="st-01",
        actor="worker",
        result=TaskResult(status="SUCCESS", summary="Done."),
    )

    assert isinstance(meta, TaskMeta)
    assert meta.status == "submitted"


def test_task_meta_legacy_positional_event_id_round_trips(tmp_path):
    """The eleventh positional argument remains the legacy Matrix event ID."""
    meta = TaskMeta(
        "st-positional",
        "tp-positional",
        "Preserve positional API",
        "worker-a",
        "room:!team-room:domain",
        "assigned",
        ["st-prerequisite"],
        "2026-08-13T01:00:00Z",
        "2026-08-13T01:01:00Z",
        "2026-08-13T01:02:00Z",
        "$legacy-event-id",
    )

    assert meta.event_id == "$legacy-event-id"
    assert meta.submission_id is None
    assert meta.result_digest is None
    assert meta.continuation is None

    store = FileSystemTaskStore(tmp_path)
    store.write_task_meta(meta)

    assert store.read_task_meta("st-positional") == meta


def test_submit_task_fails_closed_for_legacy_submission_without_identity(
    tmp_path,
):
    store = FileSystemTaskStore(tmp_path)
    task_dir = tmp_path / "shared" / "tasks" / "st-legacy-submitted"
    task_dir.mkdir(parents=True)
    meta_path = task_dir / "meta.json"
    result_path = task_dir / "result.md"
    meta_path.write_text(
        json.dumps(
            {
                "task_id": "st-legacy-submitted",
                "project_id": "tp-01",
                "task_title": "Legacy submitted task",
                "assigned_to": "worker",
                "room_id": "room:!team-room:domain",
                "status": "submitted",
                "submitted_at": "2026-08-13T01:02:00Z",
            },
        ),
    )
    result_path.write_text(
        "STATUS: SUCCESS\nSUMMARY: Legacy persisted result.\n\nDELIVERABLES:\n",
    )
    original_meta = meta_path.read_bytes()
    original_result = result_path.read_bytes()

    with pytest.raises(
        TaskflowError,
        match="submission identity is missing; cannot reuse safely",
    ):
        submit_task(
            store,
            task_id="st-legacy-submitted",
            actor="worker",
            result=TaskResult(status="SUCCESS", summary="Conflicting retry result."),
        )

    assert meta_path.read_bytes() == original_meta
    assert result_path.read_bytes() == original_result


def test_submit_task_adopts_matching_legacy_submission_deterministically(tmp_path):
    store = FileSystemTaskStore(tmp_path)
    task_dir = tmp_path / "shared" / "tasks" / "st-legacy-adopt"
    task_dir.mkdir(parents=True)
    meta_path = task_dir / "meta.json"
    submitted_at = "2026-08-13T01:02:00Z"
    meta_path.write_text(
        json.dumps(
            {
                "task_id": "st-legacy-adopt",
                "project_id": "tp-01",
                "task_title": "Adopt legacy submitted task",
                "assigned_to": "worker",
                "room_id": "room:!team-room:domain",
                "status": "submitted",
                "submitted_at": submitted_at,
            },
        ),
    )
    persisted_result = TaskResult(
        status="SUCCESS",
        summary="Legacy persisted result.",
    )
    store.write_task_result("st-legacy-adopt", persisted_result)

    adopted = submit_task(
        store,
        task_id="st-legacy-adopt",
        actor="worker",
        result=persisted_result,
    )

    assert adopted.submission_id == (
        "legacy-3aad8de167e5f9132fcb4cbef84c967b2efc476b028495b19c7a55e3ba7b0cd5"
    )
    assert adopted.submitted_at == submitted_at
    assert adopted.result_digest
    assert adopted.continuation
    assert adopted.continuation["status"] == "pending"
    assert store.read_task_meta("st-legacy-adopt") == adopted


def test_submit_task_cannot_adopt_legacy_submission_without_timestamp(tmp_path):
    store = FileSystemTaskStore(tmp_path)
    task_dir = tmp_path / "shared" / "tasks" / "st-legacy-no-time"
    task_dir.mkdir(parents=True)
    meta_path = task_dir / "meta.json"
    result_path = task_dir / "result.md"
    meta_path.write_text(
        json.dumps(
            {
                "task_id": "st-legacy-no-time",
                "project_id": "tp-01",
                "task_title": "Legacy task without timestamp",
                "assigned_to": "worker",
                "room_id": "room:!team-room:domain",
                "status": "submitted",
            },
        ),
    )
    result = TaskResult(status="SUCCESS", summary="Legacy persisted result.")
    store.write_task_result("st-legacy-no-time", result)
    original_meta = meta_path.read_bytes()
    original_result = result_path.read_bytes()

    with pytest.raises(
        TaskflowError,
        match="submission identity is missing; cannot reuse safely",
    ):
        submit_task(
            store,
            task_id="st-legacy-no-time",
            actor="worker",
            result=result,
        )

    assert meta_path.read_bytes() == original_meta
    assert result_path.read_bytes() == original_result


def test_submit_task_cannot_adopt_legacy_submission_without_explicit_result(tmp_path):
    store = FileSystemTaskStore(tmp_path)
    task_dir = tmp_path / "shared" / "tasks" / "st-legacy-no-request-result"
    task_dir.mkdir(parents=True)
    meta_path = task_dir / "meta.json"
    result_path = task_dir / "result.md"
    meta_path.write_text(
        json.dumps(
            {
                "task_id": "st-legacy-no-request-result",
                "project_id": "tp-01",
                "task_title": "Legacy task without retry evidence",
                "assigned_to": "worker",
                "room_id": "room:!team-room:domain",
                "status": "submitted",
                "submitted_at": "2026-08-13T01:02:00Z",
            },
        ),
    )
    store.write_task_result(
        "st-legacy-no-request-result",
        TaskResult(status="SUCCESS", summary="Legacy persisted result."),
    )
    original_meta = meta_path.read_bytes()
    original_result = result_path.read_bytes()

    with pytest.raises(
        TaskflowError,
        match="submission identity is missing; cannot reuse safely",
    ):
        submit_task(
            store,
            task_id="st-legacy-no-request-result",
            actor="worker",
        )

    assert meta_path.read_bytes() == original_meta
    assert result_path.read_bytes() == original_result


def test_legacy_adoption_write_failure_is_atomic_and_retry_identity_is_stable(
    tmp_path,
    monkeypatch,
):
    store = FileSystemTaskStore(tmp_path)
    task_dir = tmp_path / "shared" / "tasks" / "st-legacy-retry"
    task_dir.mkdir(parents=True)
    meta_path = task_dir / "meta.json"
    meta_path.write_text(
        json.dumps(
            {
                "task_id": "st-legacy-retry",
                "project_id": "tp-01",
                "task_title": "Crash-safe legacy adoption",
                "assigned_to": "worker",
                "room_id": "room:!team-room:domain",
                "status": "submitted",
                "submitted_at": "2026-08-13T01:02:00Z",
            },
        ),
    )
    result = TaskResult(status="SUCCESS", summary="Legacy persisted result.")
    store.write_task_result("st-legacy-retry", result)
    original_meta = meta_path.read_bytes()
    real_fsync = os.fsync

    def fail_fsync(_fd):
        raise OSError("simulated adoption flush failure")

    monkeypatch.setattr(os, "fsync", fail_fsync)
    with pytest.raises(OSError, match="simulated adoption flush failure"):
        submit_task(
            store,
            task_id="st-legacy-retry",
            actor="worker",
            result=result,
        )

    assert meta_path.read_bytes() == original_meta
    assert [path.name for path in task_dir.iterdir()] == ["meta.json", "result.md"]

    monkeypatch.setattr(os, "fsync", real_fsync)
    adopted = submit_task(
        store,
        task_id="st-legacy-retry",
        actor="worker",
        result=result,
    )
    persisted = store.read_task_meta("st-legacy-retry")

    assert adopted.submission_id == persisted.submission_id
    assert adopted.submission_id == (
        "legacy-b81af822e4be5b2d31dd64df29c7bcefbf780b5aac0aad87c49d8c13753bcf22"
    )
    assert adopted.continuation == persisted.continuation


def test_submit_task_backfills_result_identity_without_rotating_submission_id(tmp_path):
    store = FileSystemTaskStore(tmp_path)
    task_dir = tmp_path / "shared" / "tasks" / "st-backfill"
    task_dir.mkdir(parents=True)
    meta_path = task_dir / "meta.json"
    original_submission_id = "a73c43fd-1dda-4d42-88dc-18e598bed353"
    submitted_at = "2026-08-13T01:02:00Z"
    meta_path.write_text(
        json.dumps(
            {
                "task_id": "st-backfill",
                "project_id": "tp-01",
                "task_title": "Backfill submitted task",
                "assigned_to": "worker",
                "room_id": "room:!team-room:domain",
                "status": "submitted",
                "submitted_at": submitted_at,
                "submission_id": original_submission_id,
            },
        ),
    )
    persisted_result = TaskResult(
        status="SUCCESS",
        summary="Legacy persisted result.",
    )
    store.write_task_result("st-backfill", persisted_result)

    meta = submit_task(
        store,
        task_id="st-backfill",
        actor="worker",
        result=persisted_result,
    )

    assert meta.submission_id == original_submission_id
    assert meta.submitted_at == submitted_at
    assert meta.result_digest
    assert meta.continuation == {
        "status": "pending",
        "delivery_id": (
            "68f56bfa3f68393588c6507ddaa4c65c259572e8ab01ff2db8757fea71be6f19"
        ),
    }
    assert store.read_task_meta("st-backfill") == meta


def test_submit_task_uses_cross_runtime_canonical_result_digest(tmp_path):
    store = FileSystemTaskStore(tmp_path)
    task_dir = tmp_path / "shared" / "tasks" / "st-digest"
    task_dir.mkdir(parents=True)
    (task_dir / "meta.json").write_text(
        json.dumps(
            {
                "task_id": "st-digest",
                "project_id": "tp-01",
                "task_title": "Canonical result identity",
                "assigned_to": "worker",
                "room_id": "room:!team-room:domain",
                "status": "in_progress",
                "depends_on": [],
            },
        ),
    )

    meta = submit_task(
        store,
        task_id="st-digest",
        actor="worker",
        result=TaskResult(
            status="SUCCESS",
            summary="  完成\n  API\t设计  ",
            deliverables=[
                "shared/tasks/st-digest/workspace/b.md",
                "shared/tasks/st-digest/workspace/a.md",
            ],
            # Notes are runtime prose and deliberately excluded from the
            # shared structured-result identity.
            notes=["这段文字不应改变摘要。"],
        ),
    )

    assert meta.result_digest == (
        "cb1daffd3cf60982383e60cf0a09a719abb2a2bf378471a494cebad0bf1fbec7"
    )


def test_two_different_concurrent_submissions_cannot_both_succeed(tmp_path):
    class ConcurrentReadStore(FileSystemTaskStore):
        def __init__(self, workspace_dir):
            super().__init__(workspace_dir)
            self.first_reads = threading.Barrier(2)

        def read_task_meta(self, task_id):
            meta = super().read_task_meta(task_id)
            if meta.status == "in_progress":
                try:
                    self.first_reads.wait(timeout=0.25)
                except threading.BrokenBarrierError:
                    pass
            return meta

    store = ConcurrentReadStore(tmp_path)
    task_dir = tmp_path / "shared" / "tasks" / "st-race"
    task_dir.mkdir(parents=True)
    (task_dir / "meta.json").write_text(
        json.dumps(
            {
                "task_id": "st-race",
                "project_id": "tp-01",
                "task_title": "Racing task",
                "assigned_to": "worker",
                "room_id": "room:!team-room:domain",
                "status": "in_progress",
                "depends_on": [],
            },
        ),
    )
    results = (
        TaskResult(status="SUCCESS", summary="First competing result."),
        TaskResult(status="SUCCESS", summary="Second competing result."),
    )

    def attempt(result):
        try:
            return submit_task(
                store,
                task_id="st-race",
                actor="worker",
                result=result,
            )
        except TaskflowError as exc:
            return exc

    with ThreadPoolExecutor(max_workers=2) as executor:
        outcomes = list(executor.map(attempt, results))

    successes = [outcome for outcome in outcomes if isinstance(outcome, TaskMeta)]
    conflicts = [outcome for outcome in outcomes if isinstance(outcome, TaskflowError)]
    assert len(successes) == 1
    assert len(conflicts) == 1
    assert "already submitted with a different result" in str(conflicts[0])
    persisted_meta = store.read_task_meta("st-race")
    persisted_result = store.read_task_result("st-race")
    assert persisted_meta.submission_id == successes[0].submission_id
    assert persisted_result in results


@pytest.mark.asyncio
async def test_submit_task_rejects_different_result_without_overwriting(tmp_path, monkeypatch):
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
    original = {
        "taskId": "st-01",
        "status": "SUCCESS",
        "summary": "API design completed.",
        "deliverables": ["shared/tasks/st-01/workspace/api-design.md"],
    }
    changed = {
        **original,
        "summary": "A different result must not replace the submitted one.",
    }

    first = _response_json(await taskflow(action="submit_task", payload=original))
    first_meta = (task_dir / "meta.json").read_bytes()
    first_result = (task_dir / "result.md").read_bytes()
    second = _response_json(await taskflow(action="submit_task", payload=changed))

    assert first["ok"] is True
    assert second["ok"] is False
    assert "already submitted with a different result" in second["error"]
    assert (task_dir / "meta.json").read_bytes() == first_meta
    assert (task_dir / "result.md").read_bytes() == first_result


@pytest.mark.asyncio
async def test_submit_task_rejects_tampered_persisted_result_without_sync(
    tmp_path,
    monkeypatch,
):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@worker:domain")
    sync = _mock_sync(monkeypatch)

    task_dir = workspace / "shared" / "tasks" / "st-tampered"
    task_dir.mkdir(parents=True)
    meta_path = task_dir / "meta.json"
    result_path = task_dir / "result.md"
    original_request = {
        "taskId": "st-tampered",
        "status": "SUCCESS",
        "summary": "Original trusted result.",
        "deliverables": [],
    }
    meta_path.write_text(
        json.dumps(
            {
                "task_id": "st-tampered",
                "project_id": "tp-01",
                "task_title": "Detect local result tampering",
                "assigned_to": "@worker:domain",
                "room_id": "room:!team-room:domain",
                "status": "in_progress",
            },
        ),
    )

    first = _response_json(
        await taskflow(action="submit_task", payload=original_request),
    )
    assert first["ok"] is True

    result_path.write_text(
        "STATUS: SUCCESS\nSUMMARY: Tampered local result.\n\nDELIVERABLES:\n",
    )
    tampered_meta = meta_path.read_bytes()
    tampered_result = result_path.read_bytes()
    sync.reset_mock()

    retry = _response_json(
        await taskflow(action="submit_task", payload=original_request),
    )

    assert retry["ok"] is False
    assert "persisted result does not match submitted digest" in retry["error"]
    assert meta_path.read_bytes() == tampered_meta
    assert result_path.read_bytes() == tampered_result
    sync.push_shared_path.assert_not_called()
    sync.stat_shared_path.assert_not_called()


@pytest.mark.asyncio
@pytest.mark.parametrize("terminal_status", ["completed", "revision", "blocked", "cancelled"])
async def test_submit_task_rejects_terminal_task_without_rotating_identity(
    tmp_path,
    monkeypatch,
    terminal_status,
):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@worker:domain")
    _mock_sync(monkeypatch)

    task_dir = workspace / "shared" / "tasks" / "st-terminal"
    task_dir.mkdir(parents=True)
    meta_path = task_dir / "meta.json"
    result_path = task_dir / "result.md"
    meta_path.write_text(
        json.dumps(
            {
                "task_id": "st-terminal",
                "project_id": "tp-01",
                "task_title": "Finished task",
                "assigned_to": "@worker:domain",
                "room_id": "room:!team-room:domain",
                "status": terminal_status,
                "depends_on": [],
                "submission_id": "submission-original",
                "submitted_at": "2026-08-13T01:00:00Z",
                "continuation": {
                    "status": "resolved",
                    "delivery_id": "delivery-original",
                },
            },
        ),
    )
    result_path.write_text(
        "STATUS: SUCCESS\n"
        "SUMMARY: Original accepted result.\n\n"
        "DELIVERABLES:\n",
    )
    original_meta = meta_path.read_bytes()
    original_result = result_path.read_bytes()

    response = await taskflow(
        action="submit_task",
        payload={
            "taskId": "st-terminal",
            "status": "SUCCESS",
            "summary": "Late result must not replace the accepted one.",
            "deliverables": [],
        },
    )
    payload = _response_json(response)

    assert payload["ok"] is False
    assert f"submit_task cannot update terminal task: {terminal_status}" in payload["error"]
    assert meta_path.read_bytes() == original_meta
    assert result_path.read_bytes() == original_result


@pytest.mark.asyncio
async def test_submit_task_retry_repairs_missing_result_after_sync_failure(
    tmp_path,
    monkeypatch,
):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@worker:domain")
    sync = _mock_sync(monkeypatch)
    # The result upload succeeds, then publishing submitted meta fails.  This
    # is the dangerous split-commit window the retry protocol must repair.
    sync.push_shared_path.side_effect = [None, RuntimeError("remote unavailable"), None, None]

    task_dir = workspace / "shared" / "tasks" / "st-repair"
    task_dir.mkdir(parents=True)
    meta_path = task_dir / "meta.json"
    result_path = task_dir / "result.md"
    meta_path.write_text(
        json.dumps(
            {
                "task_id": "st-repair",
                "project_id": "tp-01",
                "task_title": "Repairable task",
                "assigned_to": "@worker:domain",
                "room_id": "room:!team-room:domain",
                "status": "in_progress",
                "depends_on": [],
            },
        ),
    )
    request = {
        "taskId": "st-repair",
        "status": "SUCCESS",
        "summary": "Result survives a retried remote commit.",
        "deliverables": [],
    }

    first = _response_json(await taskflow(action="submit_task", payload=request))
    assert first["ok"] is False
    assert first["retryable"] is True
    assert first["statePersisted"] is True
    assert sync.push_shared_path.call_args_list[0].args == (
        "shared/tasks/st-repair/result.md",
    )
    assert sync.push_shared_path.call_args_list[1].args == (
        "shared/tasks/st-repair/meta.json",
    )
    submission_id = first["task"]["submission_id"]
    result_digest = first["task"]["result_digest"]

    # Model a restart after an incomplete remote commit: submitted meta
    # survived locally, while result.md needs to be reconstructed from the
    # caller's identical retry payload.
    result_path.unlink()
    retried = _response_json(await taskflow(action="submit_task", payload=request))

    assert retried["ok"] is True
    assert retried["reused"] is True
    assert retried["task"]["submission_id"] == submission_id
    assert retried["task"]["result_digest"] == result_digest
    assert "Result survives a retried remote commit." in result_path.read_text()
    assert sync.push_shared_path.call_args_list[-2].args == (
        "shared/tasks/st-repair/result.md",
    )
    assert sync.push_shared_path.call_args_list[-1].args == (
        "shared/tasks/st-repair/meta.json",
    )
    assert sync.push_shared_path.call_args_list[-1].kwargs == {}


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
    _mock_notify(monkeypatch)

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
    _mock_notify(monkeypatch)

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
@pytest.mark.parametrize("terminal_status", ["completed", "revision", "blocked", "cancelled"])
async def test_ack_task_rejects_terminal_task_without_reopening(
    tmp_path,
    monkeypatch,
    terminal_status,
):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@worker:domain")
    _mock_sync(monkeypatch)

    task_dir = workspace / "shared" / "tasks" / "st-terminal"
    task_dir.mkdir(parents=True)
    meta_path = task_dir / "meta.json"
    meta_path.write_text(
        json.dumps(
            {
                "task_id": "st-terminal",
                "project_id": "tp-01",
                "task_title": "Finished task",
                "assigned_to": "@worker:domain",
                "room_id": "room:!team-room:domain",
                "status": terminal_status,
                "depends_on": [],
                "submission_id": "submission-original",
                "submitted_at": "2026-08-13T01:00:00Z",
                "continuation": {
                    "status": "resolved",
                    "delivery_id": "delivery-original",
                },
            },
        ),
    )
    (task_dir / "spec.md").write_text("# Finished task\n")
    original_meta = meta_path.read_bytes()

    response = await taskflow(
        action="ack_task",
        payload={"taskId": "st-terminal"},
    )
    payload = _response_json(response)

    assert payload["ok"] is False
    assert f"ack_task cannot update terminal task: {terminal_status}" in payload["error"]
    assert meta_path.read_bytes() == original_meta


@pytest.mark.asyncio
async def test_submit_task_calls_sync_and_stat(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@worker:domain")
    mock = _mock_sync(monkeypatch)

    task_dir = workspace / "shared" / "tasks" / "st-01"
    task_dir.mkdir(parents=True)
    deliverable_path = task_dir / "workspace" / "output.md"
    deliverable_path.parent.mkdir(parents=True)
    deliverable_path.write_text("deliverable")
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
            "deliverables": ["shared/tasks/st-01/workspace/output.md"],
        },
    )
    payload = _response_json(response)

    assert payload["ok"] is True
    assert payload["task"]["status"] == "submitted"
    assert payload["synced"] is True
    assert payload["verified"] is True
    assert mock.push_shared_path.call_args_list == [
        (("shared/tasks/st-01/result.md",), {}),
        (("shared/tasks/st-01/workspace/output.md",), {}),
        (("shared/tasks/st-01/meta.json",), {}),
    ]
    assert mock.stat_shared_path.call_args_list == [
        (("shared/tasks/st-01/result.md",), {}),
        (("shared/tasks/st-01/workspace/output.md",), {}),
        (("shared/tasks/st-01/meta.json",), {}),
    ]


@pytest.mark.asyncio
async def test_submit_task_does_not_publish_meta_when_deliverable_sync_fails(
    tmp_path,
    monkeypatch,
):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@worker:domain")
    sync = _mock_sync(monkeypatch)
    sync.push_shared_path.side_effect = [None, RuntimeError("deliverable unavailable")]
    task_dir = workspace / "shared" / "tasks" / "st-publish-last"
    (task_dir / "workspace").mkdir(parents=True)
    (task_dir / "workspace" / "output.md").write_text("output")
    (task_dir / "meta.json").write_text(json.dumps({
        "task_id": "st-publish-last",
        "project_id": "tp-01",
        "task_title": "Publish last",
        "assigned_to": "@worker:domain",
        "room_id": "room:!team:domain",
        "status": "in_progress",
    }))

    response = _response_json(await taskflow(
        action="submit_task",
        payload={
            "taskId": "st-publish-last",
            "status": "SUCCESS",
            "summary": "Done.",
            "deliverables": ["shared/tasks/st-publish-last/workspace/output.md"],
        },
    ))

    assert response["ok"] is False
    assert response["retryable"] is True
    assert [call.args[0] for call in sync.push_shared_path.call_args_list] == [
        "shared/tasks/st-publish-last/result.md",
        "shared/tasks/st-publish-last/workspace/output.md",
    ]


@pytest.mark.asyncio
async def test_submit_task_publish_last_deduplicates_result_deliverable(tmp_path, monkeypatch):
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces/default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@worker:domain")
    sync = _mock_sync(monkeypatch)
    task_dir = workspace / "shared/tasks/st-result"
    task_dir.mkdir(parents=True)
    (task_dir / "meta.json").write_text(json.dumps({
        "task_id": "st-result",
        "project_id": "tp-01",
        "task_title": "Deduplicate result",
        "assigned_to": "@worker:domain",
        "room_id": "room:!team:domain",
        "status": "in_progress",
    }))

    response = _response_json(await taskflow(
        action="submit_task",
        payload={
            "taskId": "st-result",
            "status": "SUCCESS",
            "summary": "Done.",
            "deliverables": ["shared/tasks/st-result/result.md"],
        },
    ))

    assert response["ok"] is True
    assert [call.args[0] for call in sync.push_shared_path.call_args_list] == [
        "shared/tasks/st-result/result.md",
        "shared/tasks/st-result/meta.json",
    ]


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
    _mock_notify(monkeypatch)

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
    # Task dir pushed at least twice: once before notification (prepared
    # files visible to Worker) and once after commit (assigned + event_id).
    assert mock.push_shared_path.call_count >= 2
    assert mock.push_shared_path.call_args_list[0].args == ("shared/tasks/tp-push-01/",)


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
async def test_delegate_task_notification_failure_leaves_task_prepared(
    tmp_path, monkeypatch,
):
    """When notification fails, delegate_task keeps the task prepared.

    The node is claimed (plan delegated, meta.json written with status
    ``prepared``) so files are visible to Workers, but no event_id is
    recorded — a retry re-sends with a stable txn_id and only then
    marks the task assigned.
    """
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@lead:domain")
    _mock_sync(monkeypatch)

    async def failing_notify(**kwargs):
        return {"sent": False, "error": "worker not in room"}

    monkeypatch.setattr(taskflow_tool, "_notify_task_assignment", failing_notify)

    await projectflow(
        action="create_project",
        payload={"projectId": "tp-01", "title": "Test"},
    )
    await projectflow(
        action="plan_dag",
        payload={
            "projectId": "tp-01",
            "tasks": [
                {
                    "taskId": "st-01",
                    "title": "Do stuff",
                    "assignedTo": "@worker-a:domain",
                    "dependsOn": [],
                },
            ],
        },
    )

    response = await taskflow(
        action="delegate_task",
        payload={
            "projectId": "tp-01",
            "taskId": "st-01",
            "roomId": "room:!team-room:domain",
            "spec": "Do the stuff.",
        },
    )
    payload = _response_json(response)
    assert payload["ok"] is False
    assert "not in room" in payload["error"]
    assert payload["retryable"] is True
    # Node is claimed: plan marked delegated ([~] marker), task files
    # written with status=prepared, but NO event_id yet (notification
    # never sent).
    plan = (workspace / "shared" / "projects" / "tp-01" / "plan.md").read_text()
    assert "[~]" in plan
    meta = json.loads((workspace / "shared" / "tasks" / "st-01" / "meta.json").read_text())
    assert meta["status"] == "prepared"
    assert meta.get("event_id") is None


@pytest.mark.asyncio
async def test_delegate_task_retry_after_notification_failure_sends_txn_id(
    tmp_path, monkeypatch,
):
    """Retry of a prepared task re-sends with the stable txn_id and commits.

    The first attempt fails at the notification boundary. The retry
    detects the prepared meta, skips re-claiming, sends with the same
    ``delegate-{task_id}`` txn_id (idempotent), records the event_id,
    and marks the task assigned.
    """
    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@lead:domain")
    mock = _mock_sync(monkeypatch)

    sent: dict[str, Any] = {}

    async def flaky_notify(**kwargs):
        sent["txn_id"] = kwargs.get("txn_id")
        if not sent.get("ok"):
            return {"sent": False, "error": "worker not in room"}
        return {
            "sent": True,
            "eventId": "$retry-event",
            "roomId": kwargs.get("room_id", ""),
            "assignee": "@worker-a:domain",
        }

    monkeypatch.setattr(taskflow_tool, "_notify_task_assignment", flaky_notify)

    await projectflow(
        action="create_project",
        payload={"projectId": "tp-01", "title": "Test"},
    )
    await projectflow(
        action="plan_dag",
        payload={
            "projectId": "tp-01",
            "tasks": [
                {
                    "taskId": "st-01",
                    "title": "Do stuff",
                    "assignedTo": "@worker-a:domain",
                    "dependsOn": [],
                },
            ],
        },
    )

    payload_dict = {
        "projectId": "tp-01",
        "taskId": "st-01",
        "roomId": "room:!team-room:domain",
        "spec": "Do the stuff.",
    }

    first = _response_json(
        await taskflow(action="delegate_task", payload=payload_dict),
    )
    assert first["ok"] is False
    assert first["retryable"] is True

    sent["ok"] = True
    retry = _response_json(
        await taskflow(action="delegate_task", payload=payload_dict),
    )
    assert retry["ok"] is True
    assert retry["notification"]["eventId"] == "$retry-event"
    assert retry["task"]["status"] == "assigned"
    assert retry["task"]["event_id"] == "$retry-event"
    # Stable txn_id was passed to the send boundary on both attempts.
    assert sent.get("txn_id") == "delegate-st-01"
    # The task dir was pushed at least twice (prepared + assigned).
    assert mock.push_shared_path.call_count >= 2


@pytest.mark.asyncio
async def test_delegate_task_notification_success_records_event_id(
    tmp_path, monkeypatch,
):
    """Successful notification includes eventId in the response."""
    working_dir = tmp_path / "worker" / ".copaw"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
    _set_actor(monkeypatch, "@lead:domain")
    _mock_sync(monkeypatch)
    _mock_notify(monkeypatch)

    await projectflow(
        action="create_project",
        payload={"projectId": "tp-01", "title": "Test"},
    )
    await projectflow(
        action="plan_dag",
        payload={
            "projectId": "tp-01",
            "tasks": [
                {
                    "taskId": "st-01",
                    "title": "Do stuff",
                    "assignedTo": "@worker-a:domain",
                    "dependsOn": [],
                },
            ],
        },
    )

    response = await taskflow(
        action="delegate_task",
        payload={
            "projectId": "tp-01",
            "taskId": "st-01",
            "roomId": "room:!team-room:domain",
            "spec": "Do the stuff.",
        },
    )
    payload = _response_json(response)
    assert payload["ok"] is True
    assert payload["notification"]["sent"] is True
    assert payload["notification"]["eventId"] == "$fake-event-id"
    assert payload["task"]["status"] == "assigned"


@pytest.mark.asyncio
async def test_validate_delegate_task_returns_task_without_writing(
    tmp_path, monkeypatch,
):
    """validate_delegate_task returns the DagTask without side effects."""
    from copaw_worker.task import validate_delegate_task

    working_dir = tmp_path / "worker" / ".copaw"
    workspace = working_dir / "workspaces" / "default"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))

    await projectflow(
        action="create_project",
        payload={"projectId": "tp-01", "title": "Test"},
    )
    await projectflow(
        action="plan_dag",
        payload={
            "projectId": "tp-01",
            "tasks": [
                {
                    "taskId": "st-01",
                    "title": "Do stuff",
                    "assignedTo": "@worker-a:domain",
                    "dependsOn": [],
                },
            ],
        },
    )

    store = FileSystemTaskStore(workspace)
    task = validate_delegate_task(
        store,
        project_id="tp-01",
        task_id="st-01",
        spec="Do the stuff.",
    )
    assert task.task_id == "st-01"
    assert task.assigned_to == "@worker-a:domain"
    # No state written
    assert not (workspace / "shared" / "tasks" / "st-01" / "meta.json").exists()
    plan = (workspace / "shared" / "projects" / "tp-01" / "plan.md").read_text()
    assert "delegated" not in plan


def test_task_meta_write_failure_preserves_previous_valid_state(tmp_path, monkeypatch):
    store = FileSystemTaskStore(tmp_path)
    task_dir = tmp_path / "shared" / "tasks" / "st-atomic"
    task_dir.mkdir(parents=True)
    meta_path = task_dir / "meta.json"
    original = TaskMeta(
        task_id="st-atomic",
        project_id="tp-atomic",
        task_title="Original task state",
        assigned_to="worker-a",
        status="in_progress",
    )
    meta_path.write_text(json.dumps(original.__dict__) + "\n", encoding="utf-8")
    original_bytes = meta_path.read_bytes()

    def fail_fsync(_fd):
        raise OSError("simulated disk flush failure")

    monkeypatch.setattr(os, "fsync", fail_fsync)

    with pytest.raises(OSError, match="simulated disk flush failure"):
        store.write_task_meta(
            TaskMeta(
                task_id="st-atomic",
                project_id="tp-atomic",
                task_title="Replacement task state",
                assigned_to="worker-a",
                status="submitted",
            ),
        )

    assert meta_path.read_bytes() == original_bytes
    assert store.read_task_meta("st-atomic") == original
    assert [path.name for path in task_dir.iterdir()] == ["meta.json"]


@pytest.mark.parametrize("failure_stage", ["fsync", "replace"])
def test_task_result_write_failure_preserves_previous_valid_result(
    tmp_path,
    monkeypatch,
    failure_stage,
):
    store = FileSystemTaskStore(tmp_path)
    task_id = "st-atomic-result"
    task_dir = tmp_path / "shared" / "tasks" / task_id
    result_path = task_dir / "result.md"
    original = TaskResult(
        status="SUCCESS",
        summary="Previously published result.",
        deliverables=["shared/tasks/st-atomic-result/workspace/output.md"],
    )
    replacement = TaskResult(
        status="REVISION_NEEDED",
        summary="This incomplete replacement must never become visible.",
    )
    store.write_task_result(task_id, original)
    original_bytes = result_path.read_bytes()

    if failure_stage == "fsync":
        def fail_fsync(_fd):
            raise OSError("simulated result flush failure")

        monkeypatch.setattr(os, "fsync", fail_fsync)
        expected_error = "simulated result flush failure"
    else:
        def fail_replace(_source, _destination):
            raise OSError("simulated result replace failure")

        monkeypatch.setattr(os, "replace", fail_replace)
        expected_error = "simulated result replace failure"

    with pytest.raises(OSError, match=expected_error):
        store.write_task_result(task_id, replacement)

    assert result_path.read_bytes() == original_bytes
    assert store.read_task_result(task_id) == original
    assert [path.name for path in task_dir.iterdir()] == ["result.md"]


def test_project_plan_replace_failure_preserves_previous_plan(tmp_path, monkeypatch):
    store = FileSystemTaskStore(tmp_path)
    project_dir = tmp_path / "shared" / "projects" / "tp-atomic"
    project_dir.mkdir(parents=True)
    plan_path = project_dir / "plan.md"
    original_plan = "# Original plan\n\n- [ ] st-01 Original task\n"
    plan_path.write_text(original_plan, encoding="utf-8")
    replace_paths = []

    def fail_replace(source, destination):
        replace_paths.append((Path(source), Path(destination)))
        raise OSError("simulated atomic replace failure")

    monkeypatch.setattr(os, "replace", fail_replace)

    with pytest.raises(OSError, match="simulated atomic replace failure"):
        store.write_project_plan(
            "tp-atomic",
            "# Replacement plan\n\n- [x] st-01 Original task\n",
        )

    assert len(replace_paths) == 1
    source, destination = replace_paths[0]
    assert source.parent == destination.parent == project_dir
    assert destination == plan_path
    assert store.read_project_plan("tp-atomic") == original_plan
    assert [path.name for path in project_dir.iterdir()] == ["plan.md"]
