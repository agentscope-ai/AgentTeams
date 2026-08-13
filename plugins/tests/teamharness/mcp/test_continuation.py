from __future__ import annotations

import hashlib
import json
from pathlib import Path
import subprocess
import sys
from typing import Any

import pytest


MCP_DIR = Path(__file__).resolve().parents[3] / "teamharness" / "mcp"
if str(MCP_DIR) not in sys.path:
    sys.path.insert(0, str(MCP_DIR))

import server  # noqa: E402


def _tool_payload(name: str, arguments: dict[str, Any]) -> dict[str, Any]:
    call_arguments = dict(arguments)
    if (
        name == "projectflow"
        and call_arguments.get("action") == "accept_task_result"
        and "role" not in call_arguments
    ):
        # Existing accept contract tests exercise a Leader-only operation.
        # Keep that caller identity explicit now that the public boundary
        # enforces it, while individual authorization tests can override it.
        call_arguments["role"] = "leader"
    response = server.call_tool(name, call_arguments)
    return json.loads(response["content"][0]["text"])


def _write_project_and_task(
    workspace: Path,
    *,
    task_status: str = "in_progress",
    project_id: str = "continuation-project",
    task_id: str = "continuation-project-01",
) -> tuple[str, str]:
    project = {
        "project_id": project_id,
        "title": "Continuation contract",
        "status": "active",
        "tasks": [
            {
                "task_id": task_id,
                "title": "Produce a result",
                "assigned_to": "@worker:example.test",
                "depends_on": [],
                "status": task_status,
            }
        ],
        "requester_report": {
            "pending": False,
            "sent_at": "2026-08-13T08:00:00Z",
        },
    }
    task = {
        "task_id": task_id,
        "project_id": project_id,
        "room_id": "!task:example.test",
        "status": task_status,
    }
    server._write_json(workspace / "shared" / "projects" / project_id / "meta.json", project)
    server._write_json(workspace / "shared" / "tasks" / task_id / "meta.json", task)
    return project_id, task_id


def _submit(workspace: Path, task_id: str, **overrides: Any) -> dict[str, Any]:
    payload: dict[str, Any] = {
        "taskId": task_id,
        "status": "SUCCESS",
        "summary": "The result is ready.",
        "deliverables": [f"shared/tasks/{task_id}/result.md"],
    }
    payload.update(overrides)
    return _tool_payload(
        "taskflow",
        {
            "role": "worker",
            "action": "submit_task",
            "workspaceDir": str(workspace),
            "payload": payload,
        },
    )


@pytest.fixture
def successful_side_effects(monkeypatch: pytest.MonkeyPatch) -> dict[str, list[Any]]:
    calls: dict[str, list[Any]] = {"publish": [], "sync": [], "project_sync": []}

    def publish(*args: Any, **kwargs: Any) -> list[dict[str, str]]:
        calls["publish"].append((args, kwargs))
        return [{"status": "published", "eventId": "$artifact"}]

    def sync(*args: Any, **kwargs: Any) -> bool:
        calls["sync"].append((args, kwargs))
        return True

    def project_sync(*args: Any, **kwargs: Any) -> bool:
        calls["project_sync"].append((args, kwargs))
        return True

    monkeypatch.setattr(server, "_publish_task_artifacts", publish)
    monkeypatch.setattr(server, "_publish_project_artifacts", publish)
    monkeypatch.setattr(server, "_sync_task", sync)
    monkeypatch.setattr(server, "_sync_project", project_sync)
    return calls


def test_first_submission_records_stable_continuation_identity(
    tmp_path: Path,
    successful_side_effects: dict[str, list[Any]],
) -> None:
    project_id, task_id = _write_project_and_task(tmp_path)

    submitted = _submit(tmp_path, task_id)

    assert submitted["ok"] is True
    assert submitted.get("reused") is not True
    task = submitted["task"]
    assert task["submission_id"]
    assert task["submitted_at"].endswith("Z")
    expected_delivery_id = hashlib.sha256(
        "\0".join(
            (project_id, task_id, task["submission_id"], "result-submitted:v1")
        ).encode()
    ).hexdigest()
    assert task["continuation"] == {
        "status": "pending",
        "delivery_id": expected_delivery_id,
    }
    persisted = json.loads(
        (tmp_path / "shared" / "tasks" / task_id / "meta.json").read_text(encoding="utf-8")
    )
    assert persisted == task
    assert len(successful_side_effects["publish"]) == 1


def test_first_submission_uses_cross_runtime_canonical_result_digest(
    tmp_path: Path,
    successful_side_effects: dict[str, list[Any]],
) -> None:
    _project_id, task_id = _write_project_and_task(
        tmp_path,
        project_id="tp-01",
        task_id="st-digest",
    )

    submitted = _submit(
        tmp_path,
        task_id,
        status="SUCCESS",
        summary="  完成\n  API\t设计  ",
        deliverables=[
            "shared/tasks/st-digest/workspace/b.md",
            "shared/tasks/st-digest/workspace/a.md",
        ],
        # Runtime prose is accepted but does not participate in the shared
        # structured-result identity.
        notes=["这段文字不应改变摘要。"],
    )

    assert submitted["ok"] is True
    assert submitted["task"]["result_digest"] == (
        "cb1daffd3cf60982383e60cf0a09a719abb2a2bf378471a494cebad0bf1fbec7"
    )
    persisted = json.loads(
        (tmp_path / "shared" / "tasks" / task_id / "meta.json").read_text(
            encoding="utf-8"
        )
    )
    assert persisted["result_digest"] == submitted["task"]["result_digest"]


def test_identical_submission_retry_reuses_first_write_without_republishing(
    tmp_path: Path,
    successful_side_effects: dict[str, list[Any]],
) -> None:
    _project_id, task_id = _write_project_and_task(tmp_path)
    first = _submit(tmp_path, task_id)
    meta_path = tmp_path / "shared" / "tasks" / task_id / "meta.json"
    first_bytes = meta_path.read_bytes()

    retried = _submit(tmp_path, task_id)

    assert retried["ok"] is True
    assert retried["reused"] is True
    assert retried["task"]["submission_id"] == first["task"]["submission_id"]
    assert retried["task"]["submitted_at"] == first["task"]["submitted_at"]
    assert retried["publishedArtifacts"] == []
    assert meta_path.read_bytes() == first_bytes
    assert len(successful_side_effects["publish"]) == 1


def test_submission_retry_repairs_project_node_after_partial_write(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    successful_side_effects: dict[str, list[Any]],
) -> None:
    project_id, task_id = _write_project_and_task(tmp_path)
    real_update = server._update_project_task
    update_attempts = 0

    def fail_first_project_update(*args: Any, **kwargs: Any) -> None:
        nonlocal update_attempts
        update_attempts += 1
        if update_attempts == 1:
            raise OSError("forced project update failure")
        real_update(*args, **kwargs)

    monkeypatch.setattr(server, "_update_project_task", fail_first_project_update)

    first = _submit(tmp_path, task_id)
    persisted_task = json.loads(
        (tmp_path / "shared" / "tasks" / task_id / "meta.json").read_text(encoding="utf-8")
    )
    persisted_project = json.loads(
        (tmp_path / "shared" / "projects" / project_id / "meta.json").read_text(encoding="utf-8")
    )

    assert first["ok"] is False
    assert first["retryable"] is True
    assert first["statePersisted"] is True
    assert persisted_task["status"] == "submitted"
    assert persisted_project["tasks"][0]["status"] == "in_progress"

    retried = _submit(tmp_path, task_id)
    repaired_project = json.loads(
        (tmp_path / "shared" / "projects" / project_id / "meta.json").read_text(encoding="utf-8")
    )

    assert retried["ok"] is True
    assert retried["reused"] is True
    assert retried["task"]["submission_id"] == persisted_task["submission_id"]
    assert repaired_project["tasks"][0]["status"] == "submitted"
    assert len(successful_side_effects["publish"]) == 0


def test_conflicting_submission_retry_preserves_original_meta(
    tmp_path: Path,
    successful_side_effects: dict[str, list[Any]],
) -> None:
    _project_id, task_id = _write_project_and_task(tmp_path)
    _submit(tmp_path, task_id)
    meta_path = tmp_path / "shared" / "tasks" / task_id / "meta.json"
    first_bytes = meta_path.read_bytes()

    conflict = _submit(tmp_path, task_id, summary="A different result.")

    assert conflict["ok"] is False
    assert "conflicts with existing submission" in conflict["error"]
    assert meta_path.read_bytes() == first_bytes
    assert len(successful_side_effects["publish"]) == 1


def test_legacy_submitted_task_rejects_a_different_result_without_mutation(
    tmp_path: Path,
    successful_side_effects: dict[str, list[Any]],
) -> None:
    project_id, task_id = _write_project_and_task(tmp_path, task_status="submitted")
    task_path = tmp_path / "shared" / "tasks" / task_id / "meta.json"
    legacy_task = json.loads(task_path.read_text(encoding="utf-8"))
    legacy_task.update(
        {
            "result_status": "SUCCESS",
            "summary": "The legacy result is ready.",
            "deliverables": [f"shared/tasks/{task_id}/result.md"],
            "submitted_at": "2026-08-12T08:00:00Z",
        }
    )
    server._write_json(task_path, legacy_task)
    project_path = tmp_path / "shared" / "projects" / project_id / "meta.json"
    before_project = project_path.read_bytes()
    before_task = task_path.read_bytes()

    conflict = _submit(tmp_path, task_id, summary="A replacement result.")

    assert conflict["ok"] is False
    assert "conflicts with existing submission" in conflict["error"]
    assert project_path.read_bytes() == before_project
    assert task_path.read_bytes() == before_task
    assert successful_side_effects["publish"] == []
    assert successful_side_effects["sync"] == []


def test_legacy_submitted_task_rejects_an_identical_retry_without_creating_identity(
    tmp_path: Path,
    successful_side_effects: dict[str, list[Any]],
) -> None:
    project_id, task_id = _write_project_and_task(tmp_path, task_status="submitted")
    task_path = tmp_path / "shared" / "tasks" / task_id / "meta.json"
    legacy_task = json.loads(task_path.read_text(encoding="utf-8"))
    legacy_task.update(
        {
            "result_status": "SUCCESS",
            "summary": "The result is ready.",
            "deliverables": [f"shared/tasks/{task_id}/result.md"],
            "submitted_at": "2026-08-12T08:00:00Z",
        }
    )
    server._write_json(task_path, legacy_task)
    project_path = tmp_path / "shared" / "projects" / project_id / "meta.json"
    before_project = project_path.read_bytes()
    before_task = task_path.read_bytes()

    retry = _submit(tmp_path, task_id)

    assert retry["ok"] is False
    assert "no submission identity" in retry["error"]
    assert project_path.read_bytes() == before_project
    assert task_path.read_bytes() == before_task
    assert successful_side_effects["publish"] == []
    assert successful_side_effects["sync"] == []


def test_submitted_retry_backfills_digest_from_persisted_meta(
    tmp_path: Path,
    successful_side_effects: dict[str, list[Any]],
) -> None:
    _project_id, task_id = _write_project_and_task(
        tmp_path,
        task_status="submitted",
    )
    task_path = tmp_path / "shared" / "tasks" / task_id / "meta.json"
    task = json.loads(task_path.read_text(encoding="utf-8"))
    task.update(
        {
            "resultStatus": "SUCCESS",
            "summary": "The result is ready.",
            "deliverables": [f"shared/tasks/{task_id}/result.md"],
            "submissionId": "legacy-stable-submission",
            "submittedAt": "2026-08-12T08:00:00Z",
        }
    )
    server._write_json(task_path, task)

    retried = _submit(tmp_path, task_id)

    assert retried["ok"] is True
    assert retried["reused"] is True
    assert retried["task"]["submission_id"] == "legacy-stable-submission"
    assert retried["task"]["result_digest"] == (
        "69ebfbd366d793c24c654496546619130c8d62e336b905b217a9e8d099f35496"
    )
    persisted = json.loads(task_path.read_text(encoding="utf-8"))
    assert persisted["result_digest"] == retried["task"]["result_digest"]
    assert "resultDigest" not in persisted
    assert "submissionId" not in persisted


def test_accept_validates_submission_id_and_resolves_task_fence(
    tmp_path: Path,
    successful_side_effects: dict[str, list[Any]],
) -> None:
    project_id, task_id = _write_project_and_task(tmp_path)
    submitted = _submit(tmp_path, task_id)
    submission_id = submitted["task"]["submission_id"]
    project_path = tmp_path / "shared" / "projects" / project_id / "meta.json"
    before_wrong_id = project_path.read_bytes()

    wrong_id = _tool_payload(
        "projectflow",
        {
            "action": "accept_task_result",
            "workspaceDir": str(tmp_path),
            "payload": {
                "projectId": project_id,
                "taskId": task_id,
                "submissionId": "a-different-submission",
                "resultStatus": "SUCCESS",
                "summary": "The result is ready.",
            },
        },
    )
    assert wrong_id["ok"] is False
    assert "submissionId does not match" in wrong_id["error"]
    assert project_path.read_bytes() == before_wrong_id

    accepted = _tool_payload(
        "projectflow",
        {
            "action": "accept_task_result",
            "workspaceDir": str(tmp_path),
            "payload": {
                "projectId": project_id,
                "taskId": task_id,
                "submissionId": submission_id,
                "resultStatus": "SUCCESS",
                "summary": "The result is ready.",
            },
        },
    )

    assert accepted["ok"] is True
    assert accepted["submissionId"] == submission_id
    assert accepted["task"]["status"] == "completed"
    assert accepted["task"]["continuation"]["status"] == "resolved"
    task = json.loads(
        (tmp_path / "shared" / "tasks" / task_id / "meta.json").read_text(encoding="utf-8")
    )
    assert task["status"] == "completed"
    assert task["continuation"]["status"] == "resolved"
    assert task["continuation"]["delivery_id"] == submitted["task"]["continuation"]["delivery_id"]
    assert task["submission_id"] == submission_id
    assert task["continuation"]["resolution"] == "completed"
    assert task["continuation"]["resolved_at"].endswith("Z")


@pytest.mark.parametrize(
    ("operation", "requested_submission_id"),
    [
        ("accept", None),
        ("accept", "a-stale-submission"),
        ("cancel", None),
        ("cancel", "a-stale-submission"),
    ],
)
def test_terminal_decision_requires_the_current_submission_id_before_side_effects(
    tmp_path: Path,
    successful_side_effects: dict[str, list[Any]],
    operation: str,
    requested_submission_id: str | None,
) -> None:
    project_id, task_id = _write_project_and_task(tmp_path)
    submitted = _submit(tmp_path, task_id)
    task_path = tmp_path / "shared" / "tasks" / task_id / "meta.json"
    project_path = tmp_path / "shared" / "projects" / project_id / "meta.json"
    before_task = task_path.read_bytes()
    before_project = project_path.read_bytes()
    before_side_effect_counts = {
        name: len(calls) for name, calls in successful_side_effects.items()
    }

    if operation == "accept":
        payload: dict[str, Any] = {
            "projectId": project_id,
            "taskId": task_id,
            "resultStatus": "SUCCESS",
            "summary": "This decision must be fenced.",
        }
        tool = "projectflow"
        arguments: dict[str, Any] = {
            "action": "accept_task_result",
            "workspaceDir": str(tmp_path),
            "payload": payload,
        }
    else:
        payload = {
            "taskId": task_id,
            "reason": "This decision must be fenced.",
        }
        tool = "taskflow"
        arguments = {
            "role": "leader",
            "action": "cancel_task",
            "workspaceDir": str(tmp_path),
            "payload": payload,
        }
    if requested_submission_id is not None:
        payload["submissionId"] = requested_submission_id

    rejected = _tool_payload(tool, arguments)

    assert rejected["ok"] is False
    if requested_submission_id is None:
        assert "submissionId is required" in rejected["error"]
    else:
        assert "submissionId does not match" in rejected["error"]
    assert task_path.read_bytes() == before_task
    assert project_path.read_bytes() == before_project
    assert {
        name: len(calls) for name, calls in successful_side_effects.items()
    } == before_side_effect_counts
    assert submitted["task"]["submission_id"]


def test_repeated_accept_same_decision_is_noop_and_conflicting_decision_is_rejected(
    tmp_path: Path,
    successful_side_effects: dict[str, list[Any]],
) -> None:
    project_id, task_id = _write_project_and_task(tmp_path)
    submitted = _submit(tmp_path, task_id)
    submission_id = submitted["task"]["submission_id"]
    accept_payload = {
        "projectId": project_id,
        "taskId": task_id,
        "submissionId": submission_id,
        "resultStatus": "SUCCESS",
        "summary": "The result is ready.",
    }
    first = _tool_payload(
        "projectflow",
        {
            "action": "accept_task_result",
            "workspaceDir": str(tmp_path),
            "payload": accept_payload,
        },
    )
    assert first["ok"] is True
    marked = _tool_payload(
        "projectflow",
        {
            "action": "mark_requester_report_sent",
            "workspaceDir": str(tmp_path),
            "payload": {"projectId": project_id, "sentAt": "2026-08-13T09:00:00Z"},
        },
    )
    assert marked["ok"] is True
    project_path = tmp_path / "shared" / "projects" / project_id / "meta.json"
    task_path = tmp_path / "shared" / "tasks" / task_id / "meta.json"
    before_retry_project = project_path.read_bytes()
    before_retry_task = task_path.read_bytes()

    retried = _tool_payload(
        "projectflow",
        {
            "action": "accept_task_result",
            "workspaceDir": str(tmp_path),
            "payload": {**accept_payload, "summary": "A retry must not replace the first report."},
        },
    )
    assert retried["ok"] is True
    assert retried["reused"] is True
    assert retried["publishedArtifacts"] == []
    assert retried["project"]["requester_report"] == {
        "pending": False,
        "reason": "task_result_accepted",
        "report_path": f"shared/projects/{project_id}/result.md",
        "result_status": "SUCCESS",
        "sent_at": "2026-08-13T09:00:00Z",
        "summary": "The result is ready.",
        "task_id": task_id,
    }
    assert project_path.read_bytes() == before_retry_project
    assert task_path.read_bytes() == before_retry_task
    assert len(successful_side_effects["sync"]) == 3  # submit, first accept, retry repair

    conflict = _tool_payload(
        "projectflow",
        {
            "action": "accept_task_result",
            "workspaceDir": str(tmp_path),
            "payload": {
                **accept_payload,
                "accepted": False,
            },
        },
    )
    assert conflict["ok"] is False
    assert "already decided as completed" in conflict["error"]
    assert project_path.read_bytes() == before_retry_project
    assert task_path.read_bytes() == before_retry_task


def test_cancel_resolves_pending_continuation_without_replacing_identity(
    tmp_path: Path,
    successful_side_effects: dict[str, list[Any]],
) -> None:
    _project_id, task_id = _write_project_and_task(tmp_path)
    submitted = _submit(tmp_path, task_id)
    continuation = submitted["task"]["continuation"]

    cancelled = _tool_payload(
        "taskflow",
        {
            "role": "leader",
            "action": "cancel_task",
            "workspaceDir": str(tmp_path),
            "payload": {
                "taskId": task_id,
                "submissionId": submitted["task"]["submission_id"],
                "reason": "superseded",
            },
        },
    )

    assert cancelled["ok"] is True
    assert cancelled["task"]["continuation"]["status"] == "resolved"
    assert cancelled["task"]["continuation"]["delivery_id"] == continuation["delivery_id"]
    assert cancelled["task"]["submission_id"] == submitted["task"]["submission_id"]
    assert cancelled["task"]["continuation"]["resolution"] == "cancelled"


@pytest.mark.parametrize("action", ["ack_task", "submit_task"])
def test_late_worker_updates_cannot_revive_a_terminal_task(
    tmp_path: Path,
    successful_side_effects: dict[str, list[Any]],
    action: str,
) -> None:
    project_id, task_id = _write_project_and_task(tmp_path, task_status="completed")
    project_path = tmp_path / "shared" / "projects" / project_id / "meta.json"
    task_path = tmp_path / "shared" / "tasks" / task_id / "meta.json"
    before_project = project_path.read_bytes()
    before_task = task_path.read_bytes()
    payload: dict[str, Any] = {"taskId": task_id}
    if action == "submit_task":
        payload.update(
            {
                "status": "SUCCESS",
                "summary": "This result arrived after the task was completed.",
                "deliverables": [f"shared/tasks/{task_id}/result.md"],
            }
        )

    late_update = _tool_payload(
        "taskflow",
        {
            "role": "worker",
            "action": action,
            "workspaceDir": str(tmp_path),
            "payload": payload,
        },
    )

    assert late_update["ok"] is False
    assert f"{action} cannot update terminal task: completed" in late_update["error"]
    assert project_path.read_bytes() == before_project
    assert task_path.read_bytes() == before_task
    assert successful_side_effects["publish"] == []
    assert successful_side_effects["sync"] == []


def test_late_submit_cannot_revive_a_project_terminal_fence_when_task_meta_is_stale(
    tmp_path: Path,
    successful_side_effects: dict[str, list[Any]],
) -> None:
    project_id, task_id = _write_project_and_task(tmp_path, task_status="completed")
    task_path = tmp_path / "shared" / "tasks" / task_id / "meta.json"
    stale_task = json.loads(task_path.read_text(encoding="utf-8"))
    stale_task["status"] = "in_progress"
    server._write_json(task_path, stale_task)
    project_path = tmp_path / "shared" / "projects" / project_id / "meta.json"
    before_project = project_path.read_bytes()
    before_task = task_path.read_bytes()
    late_submit = _submit(tmp_path, task_id)

    assert late_submit["ok"] is False
    assert "submit_task cannot update terminal task: completed" in late_submit["error"]
    assert project_path.read_bytes() == before_project
    assert task_path.read_bytes() == before_task
    assert successful_side_effects["publish"] == []
    assert successful_side_effects["sync"] == []


def test_repeated_accept_repairs_a_missing_task_fence_without_reopening_report(
    tmp_path: Path,
    successful_side_effects: dict[str, list[Any]],
) -> None:
    project_id, task_id = _write_project_and_task(tmp_path)
    submitted = _submit(tmp_path, task_id)
    submission_id = submitted["task"]["submission_id"]
    project_path = tmp_path / "shared" / "projects" / project_id / "meta.json"
    project = json.loads(project_path.read_text(encoding="utf-8"))
    project["tasks"][0]["status"] = "completed"
    project["requester_report"] = {
        "pending": False,
        "sent_at": "2026-08-13T09:00:00Z",
        "task_id": task_id,
        "reason": "task_result_accepted",
    }
    server._write_json(project_path, project)
    task_path = tmp_path / "shared" / "tasks" / task_id / "meta.json"
    stale_task = json.loads(task_path.read_text(encoding="utf-8"))
    assert stale_task["status"] == "submitted"
    assert stale_task["continuation"]["status"] == "pending"

    repaired = _tool_payload(
        "projectflow",
        {
            "action": "accept_task_result",
            "workspaceDir": str(tmp_path),
            "payload": {
                "projectId": project_id,
                "taskId": task_id,
                "submissionId": submission_id,
                "resultStatus": "SUCCESS",
                "summary": "This retry repairs only the task fence.",
            },
        },
    )

    assert repaired["ok"] is True
    assert repaired["reused"] is True
    assert repaired["repairedTaskFence"] is True
    assert repaired["synced"] is True
    repaired_task = json.loads(task_path.read_text(encoding="utf-8"))
    assert repaired_task["status"] == "completed"
    assert repaired_task["continuation"]["status"] == "resolved"
    assert repaired["project"]["requester_report"] == project["requester_report"]
    assert repaired["publishedArtifacts"] == []


def test_accept_retry_repairs_plan_after_meta_write_succeeds(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    successful_side_effects: dict[str, list[Any]],
) -> None:
    project_id, task_id = _write_project_and_task(tmp_path)
    submitted = _submit(tmp_path, task_id)
    project_dir = tmp_path / "shared" / "projects" / project_id
    plan_path = project_dir / "plan.md"
    server._write_project_plan(project_dir, json.loads((project_dir / "meta.json").read_text(encoding="utf-8")))
    before_plan = plan_path.read_bytes()
    real_write_plan = server._write_project_plan
    plan_attempts = 0

    def fail_first_plan_write(*args: Any, **kwargs: Any) -> None:
        nonlocal plan_attempts
        plan_attempts += 1
        if plan_attempts == 1:
            raise OSError("forced plan write failure")
        real_write_plan(*args, **kwargs)

    monkeypatch.setattr(server, "_write_project_plan", fail_first_plan_write)
    arguments = {
        "action": "accept_task_result",
        "workspaceDir": str(tmp_path),
        "payload": {
            "projectId": project_id,
            "taskId": task_id,
            "submissionId": submitted["task"]["submission_id"],
            "resultStatus": "SUCCESS",
            "summary": "Accepted once.",
        },
    }

    first = _tool_payload("projectflow", arguments)
    committed_project = json.loads((project_dir / "meta.json").read_text(encoding="utf-8"))

    assert first["ok"] is False
    assert first["retryable"] is True
    assert first["statePersisted"] is True
    assert committed_project["tasks"][0]["status"] == "completed"
    assert plan_path.read_bytes() == before_plan

    retried = _tool_payload("projectflow", arguments)
    repaired_plan = plan_path.read_text(encoding="utf-8")

    assert retried["ok"] is True
    assert retried["reused"] is True
    assert "status: completed" in repaired_plan
    assert plan_attempts == 2


def test_legacy_accept_without_task_meta_remains_supported(
    tmp_path: Path,
    successful_side_effects: dict[str, list[Any]],
) -> None:
    project_id = "legacy-project"
    task_id = "legacy-project-01"
    project = {
        "project_id": project_id,
        "title": "Legacy plan-only project",
        "status": "active",
        "tasks": [{"task_id": task_id, "status": "planned"}],
    }
    server._write_json(tmp_path / "shared" / "projects" / project_id / "meta.json", project)

    accepted = _tool_payload(
        "projectflow",
        {
            "action": "accept_task_result",
            "workspaceDir": str(tmp_path),
            "payload": {
                "projectId": project_id,
                "taskId": task_id,
                "resultStatus": "SUCCESS",
                "summary": "Accepted through the legacy plan-only path.",
            },
        },
    )

    assert accepted["ok"] is True
    assert accepted["submissionId"] is None
    assert accepted["nodeStatus"] == "completed"
    assert accepted["task"] is None
    assert "synced" not in accepted
    assert not (tmp_path / "shared" / "tasks" / task_id / "meta.json").exists()
    assert successful_side_effects["sync"] == []


def test_accept_retry_repairs_failed_task_meta_sync_without_reopening_report(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    successful_side_effects: dict[str, list[Any]],
) -> None:
    project_id, task_id = _write_project_and_task(tmp_path)
    submitted = _submit(tmp_path, task_id)
    submission_id = submitted["task"]["submission_id"]
    sync_outcomes = iter((False, True))
    monkeypatch.setattr(server, "_sync_task", lambda *_args, **_kwargs: next(sync_outcomes))
    payload = {
        "projectId": project_id,
        "taskId": task_id,
        "submissionId": submission_id,
        "resultStatus": "SUCCESS",
        "summary": "Accepted once.",
        "publishArtifacts": True,
    }

    first = _tool_payload(
        "projectflow",
        {
            "action": "accept_task_result",
            "workspaceDir": str(tmp_path),
            "payload": payload,
        },
    )
    assert first["ok"] is False
    assert first["retryable"] is True
    assert first["statePersisted"] is True
    assert first["synced"] is False
    assert "notificationNeeded" not in first
    assert len(successful_side_effects["publish"]) == 1  # submit only
    marked = _tool_payload(
        "projectflow",
        {
            "action": "mark_requester_report_sent",
            "workspaceDir": str(tmp_path),
            "payload": {"projectId": project_id, "sentAt": "2026-08-13T09:00:00Z"},
        },
    )
    report_after_send = marked["project"]["requester_report"]

    retried = _tool_payload(
        "projectflow",
        {
            "action": "accept_task_result",
            "workspaceDir": str(tmp_path),
            "payload": payload,
        },
    )

    assert retried["ok"] is True
    assert retried["reused"] is True
    assert retried["repairedTaskFence"] is False
    assert retried["synced"] is True
    assert retried["project"]["requester_report"] == report_after_send
    assert retried["publishedArtifacts"] == []
    assert len(successful_side_effects["publish"]) == 1


def test_cancel_retry_repairs_failed_sync_and_keeps_the_original_decision(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    successful_side_effects: dict[str, list[Any]],
) -> None:
    project_id, task_id = _write_project_and_task(tmp_path)
    submitted = _submit(tmp_path, task_id)
    sync_outcomes = iter((False, True))
    monkeypatch.setattr(server, "_sync_task", lambda *_args, **_kwargs: next(sync_outcomes))
    cancel_arguments = {
        "role": "leader",
        "action": "cancel_task",
        "workspaceDir": str(tmp_path),
        "payload": {
            "taskId": task_id,
            "submissionId": submitted["task"]["submission_id"],
            "reason": "superseded",
            "replacementTaskId": "continuation-project-02",
        },
    }

    first = _tool_payload("taskflow", cancel_arguments)
    assert first["ok"] is False
    assert first["retryable"] is True
    assert first["statePersisted"] is True
    assert first["synced"] is False
    assert "notificationNeeded" not in first

    retried = _tool_payload("taskflow", cancel_arguments)
    assert retried["ok"] is True
    assert retried["reused"] is True
    assert retried["synced"] is True
    assert retried["task"]["submission_id"] == submitted["task"]["submission_id"]
    assert retried["task"]["continuation"]["status"] == "resolved"
    assert retried["task"]["continuation"]["resolution"] == "cancelled"
    assert retried["project"]["tasks"][0]["status"] == "cancelled"

    conflict = _tool_payload(
        "taskflow",
        {
            **cancel_arguments,
            "payload": {
                "taskId": task_id,
                "submissionId": submitted["task"]["submission_id"],
                "reason": "a different cancellation",
            },
        },
    )
    assert conflict["ok"] is False
    assert "conflicts with existing cancellation" in conflict["error"]


def test_cancel_retry_repairs_project_node_after_partial_write(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    successful_side_effects: dict[str, list[Any]],
) -> None:
    project_id, task_id = _write_project_and_task(tmp_path)
    real_update = server._update_project_task
    update_attempts = 0

    def fail_first_project_update(*args: Any, **kwargs: Any) -> None:
        nonlocal update_attempts
        update_attempts += 1
        if update_attempts == 1:
            raise OSError("forced project update failure")
        real_update(*args, **kwargs)

    monkeypatch.setattr(server, "_update_project_task", fail_first_project_update)
    arguments = {
        "role": "leader",
        "action": "cancel_task",
        "workspaceDir": str(tmp_path),
        "payload": {"taskId": task_id, "reason": "superseded"},
    }

    first = _tool_payload("taskflow", arguments)
    persisted_task = json.loads(
        (tmp_path / "shared" / "tasks" / task_id / "meta.json").read_text(encoding="utf-8")
    )
    persisted_project = json.loads(
        (tmp_path / "shared" / "projects" / project_id / "meta.json").read_text(encoding="utf-8")
    )

    assert first["ok"] is False
    assert first["retryable"] is True
    assert first["statePersisted"] is True
    assert persisted_task["status"] == "cancelled"
    assert persisted_project["tasks"][0]["status"] == "in_progress"

    retried = _tool_payload("taskflow", arguments)
    repaired_project = json.loads(
        (tmp_path / "shared" / "projects" / project_id / "meta.json").read_text(encoding="utf-8")
    )

    assert retried["ok"] is True
    assert retried["reused"] is True
    assert repaired_project["tasks"][0]["status"] == "cancelled"
    assert successful_side_effects["publish"] == []


def test_legacy_cancelled_task_still_rejects_a_second_cancel(
    tmp_path: Path,
    successful_side_effects: dict[str, list[Any]],
) -> None:
    _project_id, task_id = _write_project_and_task(tmp_path, task_status="cancelled")
    task_path = tmp_path / "shared" / "tasks" / task_id / "meta.json"
    task = json.loads(task_path.read_text(encoding="utf-8"))
    task["cancel_reason"] = "legacy cancel"
    server._write_json(task_path, task)

    retried = _tool_payload(
        "taskflow",
        {
            "role": "leader",
            "action": "cancel_task",
            "workspaceDir": str(tmp_path),
            "payload": {"taskId": task_id, "reason": "legacy cancel"},
        },
    )

    assert retried["ok"] is False
    assert "cannot cancel terminal task: cancelled" in retried["error"]
    assert successful_side_effects["sync"] == []


def test_submit_sync_failure_is_retryable_and_reuses_persisted_submission(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    successful_side_effects: dict[str, list[Any]],
) -> None:
    _project_id, task_id = _write_project_and_task(tmp_path)
    outcomes = iter((False, True))
    monkeypatch.setattr(server, "_sync_task", lambda *_args, **_kwargs: next(outcomes))

    first = _submit(tmp_path, task_id)
    assert first["ok"] is False
    assert first["retryable"] is True
    assert first["statePersisted"] is True
    assert first["synced"] is False
    assert "notificationNeeded" not in first
    submission_id = first["task"]["submission_id"]

    retried = _submit(tmp_path, task_id)
    assert retried["ok"] is True
    assert retried["reused"] is True
    assert retried["synced"] is True
    assert retried["task"]["submission_id"] == submission_id
    assert len(successful_side_effects["publish"]) == 1


@pytest.mark.parametrize(
    "error",
    [
        OSError("mc is unavailable"),
        subprocess.TimeoutExpired(cmd=["mc", "mirror"], timeout=120),
    ],
    ids=("os-error", "timeout"),
)
def test_submit_filesync_process_failure_is_a_retryable_public_result(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    error: Exception,
) -> None:
    _project_id, task_id = _write_project_and_task(tmp_path)
    monkeypatch.setattr(server, "_publish_task_artifacts", lambda *_args, **_kwargs: [])

    def fail_filesync(*_args: Any, **_kwargs: Any) -> Any:
        raise error

    monkeypatch.setattr(server.subprocess, "run", fail_filesync)

    result = _submit(tmp_path, task_id)

    assert result["ok"] is False
    assert result["retryable"] is True
    assert result["statePersisted"] is True
    assert result["synced"] is False
    assert "shared-storage sync failed" in result["error"]
    persisted = json.loads(
        (tmp_path / "shared" / "tasks" / task_id / "meta.json").read_text(encoding="utf-8")
    )
    assert persisted["status"] == "submitted"
    assert persisted["submission_id"]


def test_accept_sync_failure_is_retryable_without_reopening_side_effects(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    successful_side_effects: dict[str, list[Any]],
) -> None:
    project_id, task_id = _write_project_and_task(tmp_path)
    submitted = _submit(tmp_path, task_id)
    submission_id = submitted["task"]["submission_id"]
    outcomes = iter((False, True))
    monkeypatch.setattr(server, "_sync_task", lambda *_args, **_kwargs: next(outcomes))
    arguments = {
        "action": "accept_task_result",
        "workspaceDir": str(tmp_path),
        "payload": {
            "projectId": project_id,
            "taskId": task_id,
            "submissionId": submission_id,
            "resultStatus": "SUCCESS",
            "summary": "Accepted once.",
        },
    }

    first = _tool_payload("projectflow", arguments)
    assert first["ok"] is False
    assert first["retryable"] is True
    assert first["statePersisted"] is True
    assert first["synced"] is False
    assert "notificationNeeded" not in first
    assert first["publishedArtifacts"] == []

    retried = _tool_payload("projectflow", arguments)
    assert retried["ok"] is True
    assert retried["reused"] is True
    assert retried["synced"] is True
    assert retried["publishedArtifacts"] == []


def test_cancel_sync_failure_is_retryable_and_repairs_without_changing_reason(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    successful_side_effects: dict[str, list[Any]],
) -> None:
    _project_id, task_id = _write_project_and_task(tmp_path)
    submitted = _submit(tmp_path, task_id)
    outcomes = iter((False, True))
    monkeypatch.setattr(server, "_sync_task", lambda *_args, **_kwargs: next(outcomes))
    arguments = {
        "role": "leader",
        "action": "cancel_task",
        "workspaceDir": str(tmp_path),
        "payload": {
            "taskId": task_id,
            "submissionId": submitted["task"]["submission_id"],
            "reason": "superseded",
        },
    }

    first = _tool_payload("taskflow", arguments)
    assert first["ok"] is False
    assert first["retryable"] is True
    assert first["statePersisted"] is True
    assert first["synced"] is False
    assert "notificationNeeded" not in first

    retried = _tool_payload("taskflow", arguments)
    assert retried["ok"] is True
    assert retried["reused"] is True
    assert retried["synced"] is True
    assert retried["task"]["cancel_reason"] == "superseded"


@pytest.mark.parametrize(
    ("result_status", "expected_node_status"),
    [
        ("SUCCESS", "completed"),
        ("SUCCESS_WITH_NOTES", "completed"),
        ("REVISION_NEEDED", "revision"),
        ("BLOCKED", "blocked"),
        ("INTERRUPTED", "blocked"),
    ],
)
def test_supported_result_status_round_trips_through_submit_check_and_accept(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    successful_side_effects: dict[str, list[Any]],
    result_status: str,
    expected_node_status: str,
) -> None:
    project_id, task_id = _write_project_and_task(tmp_path)
    monkeypatch.setattr(server, "_pull_task", lambda *_args, **_kwargs: False)

    submitted = _submit(tmp_path, task_id, status=result_status)
    checked = _tool_payload(
        "taskflow",
        {
            "role": "leader",
            "action": "check_task",
            "workspaceDir": str(tmp_path),
            "payload": {"taskId": task_id},
        },
    )
    accepted = _tool_payload(
        "projectflow",
        {
            "action": "accept_task_result",
            "workspaceDir": str(tmp_path),
            "payload": {
                "projectId": project_id,
                "taskId": task_id,
                "submissionId": submitted["task"]["submission_id"],
                "resultStatus": result_status,
                "summary": "Review the submitted result.",
            },
        },
    )

    assert submitted["ok"] is True
    assert submitted["task"]["result_status"] == result_status
    assert checked["ok"] is True
    assert checked["effective"] is True
    assert checked["validationErrors"] == []
    assert checked["result"]["status"] == result_status
    assert accepted["ok"] is True
    assert accepted["nodeStatus"] == expected_node_status


@pytest.mark.parametrize("result_status", ["FAILED", "PARTIAL"])
def test_submit_rejects_unsupported_result_status_before_persisting(
    tmp_path: Path,
    successful_side_effects: dict[str, list[Any]],
    result_status: str,
) -> None:
    project_id, task_id = _write_project_and_task(tmp_path)
    project_path = tmp_path / "shared" / "projects" / project_id / "meta.json"
    task_path = tmp_path / "shared" / "tasks" / task_id / "meta.json"
    before_project = project_path.read_bytes()
    before_task = task_path.read_bytes()

    rejected = _submit(tmp_path, task_id, status=result_status)

    assert rejected["ok"] is False
    assert f"unsupported result status: {result_status}" in rejected["error"]
    assert project_path.read_bytes() == before_project
    assert task_path.read_bytes() == before_task
    assert successful_side_effects["publish"] == []
    assert successful_side_effects["sync"] == []


@pytest.mark.parametrize("task_status", ["assigned", "in_progress"])
def test_accept_requires_an_existing_task_meta_to_be_submitted(
    tmp_path: Path,
    successful_side_effects: dict[str, list[Any]],
    task_status: str,
) -> None:
    project_id, task_id = _write_project_and_task(tmp_path, task_status=task_status)
    project_path = tmp_path / "shared" / "projects" / project_id / "meta.json"
    task_path = tmp_path / "shared" / "tasks" / task_id / "meta.json"
    before_project = project_path.read_bytes()
    before_task = task_path.read_bytes()

    rejected = _tool_payload(
        "projectflow",
        {
            "action": "accept_task_result",
            "workspaceDir": str(tmp_path),
            "payload": {
                "projectId": project_id,
                "taskId": task_id,
                "resultStatus": "SUCCESS",
                "summary": "There is no submitted result to accept.",
            },
        },
    )

    assert rejected["ok"] is False
    assert f"requires submitted task state, got {task_status}" in rejected["error"]
    assert project_path.read_bytes() == before_project
    assert task_path.read_bytes() == before_task
    assert successful_side_effects["publish"] == []
    assert successful_side_effects["sync"] == []


@pytest.mark.parametrize(
    ("mutate_task", "expected_error"),
    [
        (lambda task: task.pop("submission_id"), "requires a submission identity"),
        (lambda task: task.__setitem__("result_status", "UNKNOWN"), "invalid result status: UNKNOWN"),
        (lambda task: task.__setitem__("summary", ""), "missing result summary"),
    ],
    ids=("missing-submission-id", "invalid-result-status", "missing-summary"),
)
def test_accept_rejects_an_invalid_persisted_submission_without_mutation(
    tmp_path: Path,
    successful_side_effects: dict[str, list[Any]],
    mutate_task: Any,
    expected_error: str,
) -> None:
    project_id, task_id = _write_project_and_task(tmp_path)
    submitted = _submit(tmp_path, task_id)
    project_path = tmp_path / "shared" / "projects" / project_id / "meta.json"
    task_path = tmp_path / "shared" / "tasks" / task_id / "meta.json"
    task = json.loads(task_path.read_text(encoding="utf-8"))
    mutate_task(task)
    server._write_json(task_path, task)
    before_project = project_path.read_bytes()
    before_task = task_path.read_bytes()

    rejected = _tool_payload(
        "projectflow",
        {
            "action": "accept_task_result",
            "workspaceDir": str(tmp_path),
            "payload": {
                "projectId": project_id,
                "taskId": task_id,
                "submissionId": submitted["task"]["submission_id"],
                "resultStatus": "SUCCESS",
                "summary": "Do not accept corrupt persisted state.",
            },
        },
    )

    assert rejected["ok"] is False
    assert expected_error in rejected["error"]
    assert project_path.read_bytes() == before_project
    assert task_path.read_bytes() == before_task
    assert len(successful_side_effects["publish"]) == 1  # submit only
    assert len(successful_side_effects["sync"]) == 1  # submit only


def test_accept_rejects_result_status_mismatch_without_mutation(
    tmp_path: Path,
    successful_side_effects: dict[str, list[Any]],
) -> None:
    project_id, task_id = _write_project_and_task(tmp_path)
    submitted = _submit(tmp_path, task_id, status="BLOCKED")
    project_path = tmp_path / "shared" / "projects" / project_id / "meta.json"
    task_path = tmp_path / "shared" / "tasks" / task_id / "meta.json"
    before_project = project_path.read_bytes()
    before_task = task_path.read_bytes()

    rejected = _tool_payload(
        "projectflow",
        {
            "action": "accept_task_result",
            "workspaceDir": str(tmp_path),
            "payload": {
                "projectId": project_id,
                "taskId": task_id,
                "submissionId": submitted["task"]["submission_id"],
                "resultStatus": "SUCCESS",
                "summary": "Do not reinterpret the submitted result.",
            },
        },
    )

    assert rejected["ok"] is False
    assert "resultStatus does not match the submitted task result" in rejected["error"]
    assert project_path.read_bytes() == before_project
    assert task_path.read_bytes() == before_task
    assert len(successful_side_effects["publish"]) == 1  # submit only
    assert len(successful_side_effects["sync"]) == 1  # submit only


@pytest.mark.parametrize(
    ("field", "replacement"),
    [
        ("summary", "A different but still valid summary."),
        ("deliverables", ["shared/tasks/continuation-project-01/other.txt"]),
    ],
)
def test_accept_rejects_a_valid_but_tampered_persisted_result_before_any_write(
    tmp_path: Path,
    successful_side_effects: dict[str, list[Any]],
    field: str,
    replacement: Any,
) -> None:
    project_id, task_id = _write_project_and_task(tmp_path)
    submitted = _submit(tmp_path, task_id)
    project_path = tmp_path / "shared" / "projects" / project_id / "meta.json"
    task_path = tmp_path / "shared" / "tasks" / task_id / "meta.json"
    task = json.loads(task_path.read_text(encoding="utf-8"))
    task[field] = replacement
    server._write_json(task_path, task)
    before_project = project_path.read_bytes()
    before_task = task_path.read_bytes()

    rejected = _tool_payload(
        "projectflow",
        {
            "action": "accept_task_result",
            "workspaceDir": str(tmp_path),
            "payload": {
                "projectId": project_id,
                "taskId": task_id,
                "submissionId": submitted["task"]["submission_id"],
                "resultStatus": "SUCCESS",
                "summary": "Do not accept a result whose content identity changed.",
            },
        },
    )

    assert rejected["ok"] is False
    assert "result digest does not match" in rejected["error"]
    assert project_path.read_bytes() == before_project
    assert task_path.read_bytes() == before_task
    assert len(successful_side_effects["publish"]) == 1  # submit only
    assert len(successful_side_effects["sync"]) == 1  # submit only


@pytest.mark.parametrize("action", ["accept", "cancel"])
def test_trusted_worker_role_cannot_forge_leader_state_transitions(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    successful_side_effects: dict[str, list[Any]],
    action: str,
) -> None:
    project_id, task_id = _write_project_and_task(tmp_path)
    submitted = _submit(tmp_path, task_id)
    project_path = tmp_path / "shared" / "projects" / project_id / "meta.json"
    task_path = tmp_path / "shared" / "tasks" / task_id / "meta.json"
    before_project = project_path.read_bytes()
    before_task = task_path.read_bytes()
    before_side_effect_counts = {name: len(calls) for name, calls in successful_side_effects.items()}
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "worker")

    if action == "accept":
        rejected = _tool_payload(
            "projectflow",
            {
                "role": "leader",
                "action": "accept_task_result",
                "workspaceDir": str(tmp_path),
                "payload": {
                    "role": "leader",
                    "projectId": project_id,
                    "taskId": task_id,
                    "submissionId": submitted["task"]["submission_id"],
                    "resultStatus": "SUCCESS",
                    "summary": "A worker cannot accept its own result.",
                },
            },
        )
    else:
        rejected = _tool_payload(
            "taskflow",
            {
                "role": "leader",
                "action": "cancel_task",
                "workspaceDir": str(tmp_path),
                "payload": {
                    "role": "leader",
                    "taskId": task_id,
                    "reason": "A forged cancellation.",
                },
            },
        )

    assert rejected["ok"] is False
    assert "requires leader role" in rejected["error"]
    assert project_path.read_bytes() == before_project
    assert task_path.read_bytes() == before_task
    assert {name: len(calls) for name, calls in successful_side_effects.items()} == before_side_effect_counts


def test_trusted_leader_role_can_accept_without_an_argument_role(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    successful_side_effects: dict[str, list[Any]],
) -> None:
    project_id, task_id = _write_project_and_task(tmp_path)
    submitted = _submit(tmp_path, task_id)
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "leader")

    accepted = _tool_payload(
        "projectflow",
        {
            "role": "worker",
            "action": "accept_task_result",
            "workspaceDir": str(tmp_path),
            "payload": {
                "projectId": project_id,
                "taskId": task_id,
                "submissionId": submitted["task"]["submission_id"],
                "resultStatus": "SUCCESS",
                "summary": "The trusted leader accepts the result.",
            },
        },
    )

    assert accepted["ok"] is True
    assert accepted["nodeStatus"] == "completed"


def test_accept_migrates_legacy_submitted_task_without_a_requested_identity(
    tmp_path: Path,
    successful_side_effects: dict[str, list[Any]],
) -> None:
    project_id, task_id = _write_project_and_task(tmp_path)
    _submit(tmp_path, task_id)
    task_path = tmp_path / "shared" / "tasks" / task_id / "meta.json"
    legacy = json.loads(task_path.read_text(encoding="utf-8"))
    legacy.pop("submission_id")
    legacy.pop("submitted_at")
    legacy.pop("result_digest")
    legacy.pop("continuation")
    server._write_json(task_path, legacy)

    accepted = _tool_payload(
        "projectflow",
        {
            "action": "accept_task_result",
            "workspaceDir": str(tmp_path),
            "payload": {
                "projectId": project_id,
                "taskId": task_id,
                "resultStatus": "SUCCESS",
                "summary": "Accept the legacy persisted result.",
            },
        },
    )

    assert accepted["ok"] is True
    assert accepted["submissionId"]
    migrated = accepted["task"]
    assert migrated["submission_id"] == accepted["submissionId"]
    assert migrated["submitted_at"].endswith("Z")
    assert migrated["result_digest"] == server._task_result_digest(server._submission_result(migrated))
    assert migrated["status"] == "completed"
    assert migrated["continuation"]["status"] == "resolved"
    assert migrated["continuation"]["resolution"] == "completed"
    assert migrated["continuation"]["resolved_at"].endswith("Z")


def test_legacy_accept_project_write_failure_reports_the_migrated_identity_as_persisted(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    successful_side_effects: dict[str, list[Any]],
) -> None:
    project_id, task_id = _write_project_and_task(tmp_path)
    _submit(tmp_path, task_id)
    task_path = tmp_path / "shared" / "tasks" / task_id / "meta.json"
    project_path = tmp_path / "shared" / "projects" / project_id / "meta.json"
    legacy = json.loads(task_path.read_text(encoding="utf-8"))
    for key in ("submission_id", "submitted_at", "result_digest", "continuation"):
        legacy.pop(key)
    server._write_json(task_path, legacy)
    real_write_json = server._write_json

    def fail_project_write(path: Path, data: dict[str, Any]) -> None:
        if path == project_path:
            raise OSError("project disk full")
        real_write_json(path, data)

    monkeypatch.setattr(server, "_write_json", fail_project_write)
    result = _tool_payload(
        "projectflow",
        {
            "action": "accept_task_result",
            "workspaceDir": str(tmp_path),
            "payload": {
                "projectId": project_id,
                "taskId": task_id,
                "resultStatus": "SUCCESS",
                "summary": "Retry after the project write is repaired.",
            },
        },
    )

    migrated = json.loads(task_path.read_text(encoding="utf-8"))
    assert result["ok"] is False
    assert result["retryable"] is True
    assert result["statePersisted"] is True
    assert migrated["submission_id"]
    assert migrated["continuation"]["status"] == "pending"
    assert json.loads(project_path.read_text(encoding="utf-8"))["tasks"][0]["status"] == "submitted"


@pytest.mark.parametrize("operation", ["submit", "cancel"])
def test_initial_task_state_write_failure_is_retryable_without_a_false_commit(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    successful_side_effects: dict[str, list[Any]],
    operation: str,
) -> None:
    _project_id, task_id = _write_project_and_task(tmp_path)
    task_path = tmp_path / "shared" / "tasks" / task_id / "meta.json"
    before = task_path.read_bytes()
    monkeypatch.setattr(server, "_write_task", lambda *_args, **_kwargs: (_ for _ in ()).throw(OSError("disk full")))

    if operation == "submit":
        result = _submit(tmp_path, task_id)
    else:
        result = _tool_payload(
            "taskflow",
            {
                "role": "leader",
                "action": "cancel_task",
                "workspaceDir": str(tmp_path),
                "payload": {"taskId": task_id, "reason": "stop"},
            },
        )

    assert result["ok"] is False
    assert result["retryable"] is True
    assert result["statePersisted"] is False
    assert result["synced"] is False
    assert task_path.read_bytes() == before


def test_accept_task_state_write_failure_reports_the_committed_project_decision(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    successful_side_effects: dict[str, list[Any]],
) -> None:
    project_id, task_id = _write_project_and_task(tmp_path)
    submitted = _submit(tmp_path, task_id)
    monkeypatch.setattr(server, "_write_task", lambda *_args, **_kwargs: (_ for _ in ()).throw(OSError("disk full")))

    result = _tool_payload(
        "projectflow",
        {
            "action": "accept_task_result",
            "workspaceDir": str(tmp_path),
            "payload": {
                "projectId": project_id,
                "taskId": task_id,
                "submissionId": submitted["task"]["submission_id"],
                "resultStatus": "SUCCESS",
                "summary": "The project decision commits first.",
            },
        },
    )

    persisted_project = json.loads(
        (tmp_path / "shared" / "projects" / project_id / "meta.json").read_text(encoding="utf-8")
    )
    persisted_task = json.loads(
        (tmp_path / "shared" / "tasks" / task_id / "meta.json").read_text(encoding="utf-8")
    )
    assert result["ok"] is False
    assert result["retryable"] is True
    assert result["statePersisted"] is True
    assert result["synced"] is False
    assert persisted_project["tasks"][0]["status"] == "completed"
    assert persisted_task["status"] == "submitted"


def test_submit_backfill_write_failure_reports_the_existing_submission_as_persisted(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    successful_side_effects: dict[str, list[Any]],
) -> None:
    _project_id, task_id = _write_project_and_task(tmp_path)
    submitted = _submit(tmp_path, task_id)
    task_path = tmp_path / "shared" / "tasks" / task_id / "meta.json"
    legacy = json.loads(task_path.read_text(encoding="utf-8"))
    legacy.pop("result_digest")
    server._write_json(task_path, legacy)
    monkeypatch.setattr(server, "_write_task", lambda *_args, **_kwargs: (_ for _ in ()).throw(OSError("disk full")))

    retried = _submit(tmp_path, task_id)

    assert retried["ok"] is False
    assert retried["retryable"] is True
    assert retried["statePersisted"] is True
    assert retried["synced"] is False
    assert retried["task"]["submission_id"] == submitted["task"]["submission_id"]
    persisted = json.loads(task_path.read_text(encoding="utf-8"))
    assert persisted["status"] == "submitted"
    assert persisted["submission_id"] == submitted["task"]["submission_id"]


@pytest.mark.parametrize("failure_point", ["fsync", "replace"])
def test_state_projection_atomic_write_preserves_the_previous_file_on_failure(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    failure_point: str,
) -> None:
    state_path = tmp_path / "shared" / "tasks" / "atomic-task" / "meta.json"
    server._write_json(state_path, {"task_id": "atomic-task", "status": "assigned"})
    before = state_path.read_bytes()

    if failure_point == "fsync":
        monkeypatch.setattr(server.os, "fsync", lambda _fd: (_ for _ in ()).throw(OSError("fsync failed")))
    else:
        def fail_replace(source: Path | str, target: Path | str) -> None:
            assert Path(source).parent == Path(target).parent
            raise OSError("replace failed")

        monkeypatch.setattr(server.os, "replace", fail_replace)

    with pytest.raises(OSError, match=failure_point):
        server._write_json(state_path, {"task_id": "atomic-task", "status": "submitted"})

    assert state_path.read_bytes() == before
    assert json.loads(state_path.read_text(encoding="utf-8"))["status"] == "assigned"
    assert list(state_path.parent.glob(".meta.json.*.tmp")) == []


@pytest.mark.parametrize("operation", ["submit", "accept", "cancel"])
def test_state_transition_commits_project_projection_to_shared_storage(
    tmp_path: Path,
    successful_side_effects: dict[str, list[Any]],
    operation: str,
) -> None:
    project_id, task_id = _write_project_and_task(tmp_path)

    if operation == "submit":
        result = _submit(tmp_path, task_id)
    else:
        submitted = _submit(tmp_path, task_id)
        if operation == "accept":
            result = _tool_payload(
                "projectflow",
                {
                    "action": "accept_task_result",
                    "workspaceDir": str(tmp_path),
                    "payload": {
                        "projectId": project_id,
                        "taskId": task_id,
                        "submissionId": submitted["task"]["submission_id"],
                        "resultStatus": "SUCCESS",
                        "summary": "Commit the project decision.",
                    },
                },
            )
        else:
            result = _tool_payload(
                "taskflow",
                {
                    "role": "leader",
                    "action": "cancel_task",
                    "workspaceDir": str(tmp_path),
                    "payload": {
                        "taskId": task_id,
                        "submissionId": submitted["task"]["submission_id"],
                        "reason": "cancelled by test",
                    },
                },
            )

    assert result["ok"] is True
    assert successful_side_effects["project_sync"]
    synced_project_ids = [args[1] for args, _kwargs in successful_side_effects["project_sync"]]
    assert project_id in synced_project_ids


def test_project_sync_failure_is_retryable_before_task_commit_is_reported(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    successful_side_effects: dict[str, list[Any]],
) -> None:
    project_id, task_id = _write_project_and_task(tmp_path)
    outcomes = iter((False, True))
    monkeypatch.setattr(server, "_sync_project", lambda *_args, **_kwargs: next(outcomes))

    first = _submit(tmp_path, task_id)
    second = _submit(tmp_path, task_id)

    assert first["ok"] is False
    assert first["retryable"] is True
    assert first["statePersisted"] is True
    assert first["synced"] is False
    assert second["ok"] is True
    assert second["reused"] is True
    assert second["task"]["submission_id"] == first["task"]["submission_id"]


def test_submit_pushes_and_verifies_result_payload_before_the_meta_commit_point(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    _project_id, task_id = _write_project_and_task(tmp_path)
    task_dir = tmp_path / "shared" / "tasks" / task_id
    (task_dir / "result.md").write_text("result", encoding="utf-8")
    (task_dir / "output.txt").write_text("output", encoding="utf-8")
    calls: list[tuple[str, str]] = []

    def filesync(arguments: dict[str, Any]) -> dict[str, Any]:
        calls.append((str(arguments["action"]), str(arguments["path"])))
        return {"ok": True}

    monkeypatch.setattr(server, "_filesync", filesync)
    monkeypatch.setattr(server, "_sync_project", lambda *_args, **_kwargs: True)
    monkeypatch.setattr(server, "_publish_task_artifacts", lambda *_args, **_kwargs: [])

    submitted = _submit(
        tmp_path,
        task_id,
        deliverables=[f"shared/tasks/{task_id}/output.txt"],
    )

    assert submitted["ok"] is True
    assert calls == [
        ("push", f"shared/tasks/{task_id}/result.md"),
        ("stat", f"shared/tasks/{task_id}/result.md"),
        ("push", f"shared/tasks/{task_id}/output.txt"),
        ("stat", f"shared/tasks/{task_id}/output.txt"),
        ("push", f"shared/tasks/{task_id}/meta.json"),
        ("stat", f"shared/tasks/{task_id}/meta.json"),
    ]


def test_submit_meta_commit_must_be_remotely_verified_before_success(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    _project_id, task_id = _write_project_and_task(tmp_path)
    task_dir = tmp_path / "shared" / "tasks" / task_id
    (task_dir / "result.md").write_text("result", encoding="utf-8")
    calls: list[tuple[str, str]] = []
    meta_stat_failed = False
    meta_path = f"shared/tasks/{task_id}/meta.json"

    def filesync(arguments: dict[str, Any]) -> dict[str, Any]:
        nonlocal meta_stat_failed
        call = (str(arguments["action"]), str(arguments["path"]))
        calls.append(call)
        if call == ("stat", meta_path) and not meta_stat_failed:
            meta_stat_failed = True
            return {"ok": False, "error": "remote meta commit is not visible"}
        return {"ok": True}

    monkeypatch.setattr(server, "_filesync", filesync)
    monkeypatch.setattr(server, "_sync_project", lambda *_args, **_kwargs: True)
    monkeypatch.setattr(server, "_publish_task_artifacts", lambda *_args, **_kwargs: [])

    first = _submit(tmp_path, task_id, deliverables=[])
    second = _submit(tmp_path, task_id, deliverables=[])

    assert first["ok"] is False
    assert first["statePersisted"] is True
    assert first["retryable"] is True
    assert first["synced"] is False
    assert second["ok"] is True
    assert second["reused"] is True
    assert second["task"]["submission_id"] == first["task"]["submission_id"]
    assert calls.count(("push", meta_path)) == 2
    assert calls.count(("stat", meta_path)) == 2
    assert calls[-2:] == [("push", meta_path), ("stat", meta_path)]


def test_submit_retry_repairs_interrupted_payload_publish_without_rotating_identity(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    _project_id, task_id = _write_project_and_task(tmp_path)
    task_dir = tmp_path / "shared" / "tasks" / task_id
    (task_dir / "result.md").write_text("result", encoding="utf-8")
    calls: list[tuple[str, str]] = []
    failed = False

    def filesync(arguments: dict[str, Any]) -> dict[str, Any]:
        nonlocal failed
        call = (str(arguments["action"]), str(arguments["path"]))
        calls.append(call)
        if call[0] == "stat" and not failed:
            failed = True
            return {"ok": False, "error": "remote stat unavailable"}
        return {"ok": True}

    monkeypatch.setattr(server, "_filesync", filesync)
    monkeypatch.setattr(server, "_sync_project", lambda *_args, **_kwargs: True)
    monkeypatch.setattr(server, "_publish_task_artifacts", lambda *_args, **_kwargs: [])

    first = _submit(tmp_path, task_id, deliverables=[])
    second = _submit(tmp_path, task_id, deliverables=[])

    assert first["ok"] is False
    assert first["statePersisted"] is True
    assert first["retryable"] is True
    assert second["ok"] is True
    assert second["reused"] is True
    assert second["task"]["submission_id"] == first["task"]["submission_id"]
    meta_push = ("push", f"shared/tasks/{task_id}/meta.json")
    assert calls.count(meta_push) == 1
    assert calls[-2:] == [
        meta_push,
        ("stat", f"shared/tasks/{task_id}/meta.json"),
    ]
