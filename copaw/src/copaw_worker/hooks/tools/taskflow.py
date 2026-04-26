"""CoPaw-native taskflow tool for HiClaw project/task state."""

from __future__ import annotations

from dataclasses import asdict
import json
import os
from pathlib import Path
from typing import Any

from agentscope.message import TextBlock
from agentscope.tool import ToolResponse

from copaw_worker.task import (
    FileSystemTaskStore,
    RESULT_STATUSES,
    TaskResult,
    TaskflowError,
    ack_task,
    add_tasks,
    assign_task,
    complete_task,
    create_project,
    ready_tasks,
    submit_task,
    validate_task_result,
)


def _response(payload: dict[str, Any]) -> ToolResponse:
    return ToolResponse(
        content=[
            TextBlock(
                type="text",
                text=json.dumps(payload, ensure_ascii=False),
            ),
        ],
    )


def _ok(**payload: Any) -> ToolResponse:
    return _response({"ok": True, **payload})


def _error(message: str, **payload: Any) -> ToolResponse:
    return _response({"ok": False, "error": message, **payload})


def _workspace_dir() -> Path:
    configured = os.getenv("COPAW_WORKING_DIR")
    if configured:
        return Path(configured) / "workspaces" / "default"

    cwd = Path.cwd()
    if cwd.name == "default" and cwd.parent.name == "workspaces":
        return cwd
    if cwd.name == ".copaw":
        return cwd / "workspaces" / "default"
    return cwd


def _store() -> FileSystemTaskStore:
    return FileSystemTaskStore(_workspace_dir())


def _asdict_list(items: list[Any]) -> list[dict[str, Any]]:
    return [asdict(item) for item in items]


def _coerce_payload(payload: dict[str, Any] | str | None) -> dict[str, Any]:
    if isinstance(payload, str):
        try:
            payload = json.loads(payload)
        except json.JSONDecodeError as exc:
            raise TaskflowError(f"payload must be a JSON object: {exc.msg}") from exc
    if payload is None:
        return {}
    if not isinstance(payload, dict):
        raise TaskflowError("payload must be an object")
    return payload


def _required_str(payload: dict[str, Any], key: str) -> str:
    value = payload.get(key)
    if not isinstance(value, str) or not value.strip():
        raise TaskflowError(f"payload.{key} is required")
    return value.strip()


def _optional_str(payload: dict[str, Any], key: str) -> str | None:
    value = payload.get(key)
    if value is None:
        return None
    if not isinstance(value, str):
        raise TaskflowError(f"payload.{key} must be a string")
    return value


def _coerce_tasks(tasks: list[dict[str, Any]] | str | None) -> list[dict[str, Any]]:
    if isinstance(tasks, str):
        try:
            tasks = json.loads(tasks)
        except json.JSONDecodeError as exc:
            raise TaskflowError(f"tasks must be a JSON array: {exc.msg}") from exc
    if not isinstance(tasks, list) or not tasks:
        raise TaskflowError("tasks must be a non-empty list")
    if not all(isinstance(task, dict) for task in tasks):
        raise TaskflowError("tasks must be a list of objects")
    return tasks


def _coerce_str_list(payload: dict[str, Any], key: str) -> list[str]:
    value = payload.get(key)
    if value is None:
        return []
    if isinstance(value, str):
        try:
            value = json.loads(value)
        except json.JSONDecodeError as exc:
            raise TaskflowError(f"payload.{key} must be a JSON array: {exc.msg}") from exc
    if not isinstance(value, list):
        raise TaskflowError(f"payload.{key} must be a list")
    normalized = [str(item).strip() for item in value if str(item).strip()]
    return normalized


def _task_result_from_payload(payload: dict[str, Any]) -> TaskResult | None:
    result_keys = {"status", "summary", "deliverables", "notes"}
    if not any(key in payload for key in result_keys):
        return None

    status = _required_str(payload, "status")
    if status not in RESULT_STATUSES:
        raise TaskflowError(f"invalid result status: {status}")
    return TaskResult(
        status=status,
        summary=_required_str(payload, "summary"),
        deliverables=_coerce_str_list(payload, "deliverables"),
        notes=_coerce_str_list(payload, "notes"),
    )


async def taskflow(
    action: str,
    payload: dict[str, Any] | str | None = None,
    dryRun: bool = False,
) -> ToolResponse:
    """Manage HiClaw task state with action-specific payload fields."""
    payload_data: dict[str, Any] = {}
    try:
        store = _store()
        payload_data = _coerce_payload(payload)

        if action == "create_project":
            project_id = _required_str(payload_data, "projectId")
            title = _required_str(payload_data, "title")
            if dryRun:
                return _ok(
                    dryRun=True,
                    action=action,
                    projectId=project_id,
                    title=title,
                )
            meta = create_project(
                store,
                project_id=project_id,
                title=title,
                source=_optional_str(payload_data, "source"),
                requester=_optional_str(payload_data, "requester"),
                parent_task_id=_optional_str(payload_data, "parentTaskId"),
            )
            return _ok(action=action, project=asdict(meta))

        if action == "add_tasks":
            project_id = _required_str(payload_data, "projectId")
            tasks_payload = _coerce_tasks(payload_data.get("tasks"))
            if dryRun:
                return _ok(dryRun=True, action=action, projectId=project_id, tasks=tasks_payload)
            graph = add_tasks(store, project_id=project_id, tasks=tasks_payload)
            ready = ready_tasks(store, project_id=project_id)
            return _ok(
                action=action,
                tasks=_asdict_list(graph),
                readyTasks=_asdict_list(ready),
            )

        if action == "ready_tasks":
            project_id = _required_str(payload_data, "projectId")
            ready = ready_tasks(store, project_id=project_id)
            return _ok(action=action, readyTasks=_asdict_list(ready))

        if action == "assign_task":
            project_id = _required_str(payload_data, "projectId")
            task_id = _required_str(payload_data, "taskId")
            spec = _required_str(payload_data, "spec")
            if dryRun:
                return _ok(
                    dryRun=True,
                    action=action,
                    projectId=project_id,
                    taskId=task_id,
                )
            meta = assign_task(
                store,
                project_id=project_id,
                task_id=task_id,
                spec=spec,
                room_id=_optional_str(payload_data, "roomId"),
            )
            return _ok(action=action, task=asdict(meta))

        if action == "complete_task":
            project_id = _required_str(payload_data, "projectId")
            task_id = _required_str(payload_data, "taskId")
            if dryRun:
                return _ok(
                    dryRun=True,
                    action=action,
                    projectId=project_id,
                    taskId=task_id,
                )
            result = complete_task(store, project_id=project_id, task_id=task_id)
            ready = ready_tasks(store, project_id=project_id)
            return _ok(
                action=action,
                result=asdict(result),
                readyTasks=_asdict_list(ready),
            )

        if action == "ack_task":
            task_id = _required_str(payload_data, "taskId")
            if dryRun:
                return _ok(dryRun=True, action=action, taskId=task_id)
            meta = ack_task(store, task_id=task_id)
            return _ok(action=action, task=asdict(meta))

        if action == "submit_task":
            task_id = _required_str(payload_data, "taskId")
            result = _task_result_from_payload(payload_data)
            if result is not None:
                validate_task_result(task_id, result)
            if dryRun:
                dry_run_payload: dict[str, Any] = {
                    "dryRun": True,
                    "action": action,
                    "taskId": task_id,
                }
                if result is not None:
                    dry_run_payload["result"] = asdict(result)
                return _ok(**dry_run_payload)
            meta = submit_task(store, task_id=task_id, result=result)
            response_payload: dict[str, Any] = {"action": action, "task": asdict(meta)}
            if result is not None:
                response_payload["result"] = asdict(result)
            return _ok(**response_payload)

        raise TaskflowError(
            "action must be one of: create_project, add_tasks, ready_tasks, "
            "assign_task, complete_task, ack_task, submit_task",
        )
    except TaskflowError as exc:
        return _error(
            str(exc),
            action=action,
            projectId=payload_data.get("projectId"),
            taskId=payload_data.get("taskId"),
        )
    except Exception as exc:  # pragma: no cover - defensive runtime boundary
        return _error(
            f"taskflow failed: {exc}",
            action=action,
            projectId=payload_data.get("projectId"),
            taskId=payload_data.get("taskId"),
        )
