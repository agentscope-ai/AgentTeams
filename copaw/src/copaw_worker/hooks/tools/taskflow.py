"""CoPaw-native taskflow tool for HiClaw task state."""

from __future__ import annotations

from dataclasses import asdict
import json
import os
from pathlib import Path
from typing import Any

from agentscope.message import TextBlock
from agentscope.tool import ToolResponse

from copaw_worker.hooks.tools.filesync import create_sync
from copaw_worker.task import (
    FileSystemTaskStore,
    RESULT_STATUSES,
    TaskMeta,
    TaskResult,
    TaskflowError,
    ack_task,
    canonical_worker_id,
    check_task,
    delegate_task,
    is_effective_result,
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


def _current_actor() -> str | None:
    configured = os.getenv("HICLAW_MATRIX_USER_ID") or os.getenv("COPAW_MATRIX_USER_ID")
    if configured:
        return configured.strip()

    try:
        from copaw.config.config import load_agent_config

        agent_config = load_agent_config("default")
        channels = _read_config_value(agent_config, "channels") or {}
        matrix_cfg = _read_config_value(channels, "matrix") or {}
        user_id = _read_config_value(matrix_cfg, "user_id", "userId")
        return str(user_id).strip() if user_id else None
    except Exception:
        return None


def _read_config_value(obj: Any, *names: str) -> Any:
    for name in names:
        if isinstance(obj, dict) and name in obj:
            return obj.get(name)
        if hasattr(obj, name):
            return getattr(obj, name)
    return None


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


def _require_ack_preconditions(meta: TaskMeta, actor: str | None) -> None:
    current = canonical_worker_id(actor)
    if not current:
        raise TaskflowError("current worker identity is required")
    assigned = canonical_worker_id(meta.assigned_to)
    if current != assigned:
        raise TaskflowError(
            f"task {meta.task_id} is assigned to {meta.assigned_to}, not {current}",
        )
    if not (meta.room_id or "").strip():
        raise TaskflowError(f"task {meta.task_id} is missing room_id")


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

        if action == "delegate_task":
            project_id = _required_str(payload_data, "projectId")
            task_id = _required_str(payload_data, "taskId")
            room_id = _required_str(payload_data, "roomId")
            spec = _required_str(payload_data, "spec")
            if dryRun:
                return _ok(
                    dryRun=True,
                    action=action,
                    projectId=project_id,
                    taskId=task_id,
                )
            meta = delegate_task(
                store,
                project_id=project_id,
                task_id=task_id,
                spec=spec,
                room_id=room_id,
            )
            task_path = f"shared/tasks/{task_id}/"
            sync = create_sync()
            sync.push_shared_path(task_path)
            return _ok(action=action, task=asdict(meta), synced=True)

        if action == "check_task":
            task_id = _required_str(payload_data, "taskId")
            if dryRun:
                return _ok(dryRun=True, action=action, taskId=task_id)
            task_path = f"shared/tasks/{task_id}/"
            sync = create_sync()
            sync.pull_shared_path(task_path)
            meta = store.read_task_meta(task_id)
            result = check_task(store, task_id=task_id)
            return _ok(
                action=action,
                task=asdict(meta),
                result=asdict(result),
                effective=is_effective_result(result),
            )

        if action == "ack_task":
            task_id = _required_str(payload_data, "taskId")
            if dryRun:
                return _ok(dryRun=True, action=action, taskId=task_id)
            task_path = f"shared/tasks/{task_id}/"
            sync = create_sync()
            sync.pull_shared_path(task_path)
            actor = _current_actor()
            _require_ack_preconditions(store.read_task_meta(task_id), actor)
            spec = store.read_task_spec(task_id)
            meta = ack_task(store, task_id=task_id, actor=actor)
            sync.push_shared_path(task_path, exclude=["spec.md", "base/"])
            return _ok(action=action, task=asdict(meta), spec=spec)

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
            meta = submit_task(store, task_id=task_id, result=result, actor=_current_actor())
            task_path = f"shared/tasks/{task_id}/"
            result_path = f"shared/tasks/{task_id}/result.md"
            sync = create_sync()
            sync.push_shared_path(task_path, exclude=["spec.md", "base/"])
            sync.stat_shared_path(result_path)
            response_payload: dict[str, Any] = {
                "action": action,
                "task": asdict(meta),
                "synced": True,
                "verified": True,
            }
            if result is not None:
                response_payload["result"] = asdict(result)
            return _ok(**response_payload)

        raise TaskflowError(
            "action must be one of: delegate_task, check_task, ack_task, submit_task",
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
