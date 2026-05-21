"""CoPaw-native projectflow tool for HiClaw project/DAG execution."""

from __future__ import annotations

import asyncio
from dataclasses import asdict
import json
import os
from pathlib import Path
from typing import Any
import urllib.error
import urllib.request

from agentscope.message import TextBlock
from agentscope.tool import ToolResponse

from copaw_worker.task import (
    FileSystemTaskStore,
    TaskflowError,
    canonical_worker_id,
    complete_project,
    create_project,
    pause_project,
    parse_dag_tasks,
    parse_loop_plan,
    parse_loop_tasks,
    parse_plan_type,
    plan_dag,
    plan_loop,
    ready_loop_nodes,
    ready_nodes,
    record_loop_iteration,
    resume_project,
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


def _coerce_optional_tasks(tasks: list[dict[str, Any]] | str | None) -> list[dict[str, Any]]:
    if tasks is None:
        return []
    return _coerce_tasks(tasks)


def _required_int(payload: dict[str, Any], key: str) -> int:
    value = payload.get(key)
    if isinstance(value, bool) or not isinstance(value, int):
        raise TaskflowError(f"payload.{key} must be an integer")
    return value


def _optional_int(payload: dict[str, Any], key: str, default: int) -> int:
    value = payload.get(key)
    if value is None:
        return default
    if isinstance(value, bool) or not isinstance(value, int):
        raise TaskflowError(f"payload.{key} must be an integer")
    return value


def _list_project_ids(store: FileSystemTaskStore) -> list[str]:
    projects_dir = store.shared_dir / "projects"
    if not projects_dir.exists():
        return []
    return sorted(
        path.name
        for path in projects_dir.iterdir()
        if path.is_dir() and (path / "meta.json").exists()
    )


def _task_payload(task: Any) -> dict[str, Any]:
    return {
        "taskId": task.task_id,
        "title": task.title,
        "assignedTo": task.assigned_to,
        "dependsOn": task.depends_on,
        "planStatus": task.status,
    }


async def _fetch_worker_runtime_status(
    worker_name: str,
    *,
    timeout_seconds: int,
) -> dict[str, Any]:
    safe_worker = canonical_worker_id(worker_name)
    if not safe_worker:
        return {
            "runtimeStatus": "unknown",
            "runtimeStatusSource": "unconfigured",
            "error": "worker name is empty",
        }

    url = f"http://hiclaw-worker-{safe_worker}:8088/api/chats"

    def _fetch() -> dict[str, Any]:
        request = urllib.request.Request(url, headers={"X-Agent-Id": "default"})
        with urllib.request.urlopen(request, timeout=timeout_seconds) as response:
            data = json.loads(response.read().decode("utf-8"))
        if not isinstance(data, list):
            raise TaskflowError("worker /api/chats response must be a list")
        running = any(
            isinstance(item, dict) and item.get("status") == "running"
            for item in data
        )
        running_count = sum(
            1
            for item in data
            if isinstance(item, dict) and item.get("status") == "running"
        )
        return {
            "runtimeStatus": "running" if running else "idle",
            "runtimeStatusSource": url,
            "runningSessionCount": running_count,
            "sessionCount": len(data),
        }

    try:
        return await asyncio.to_thread(_fetch)
    except (
        OSError,
        TimeoutError,
        urllib.error.URLError,
        json.JSONDecodeError,
        TaskflowError,
    ) as exc:
        return {
            "runtimeStatus": "unknown",
            "runtimeStatusSource": url,
            "error": str(exc),
        }


async def _check_active_tasks(
    store: FileSystemTaskStore,
    *,
    project_id: str | None = None,
    timeout_seconds: int = 3,
) -> dict[str, Any]:
    project_ids = [project_id] if project_id else _list_project_ids(store)
    issues: list[dict[str, Any]] = []
    checked_projects = 0

    for current_project_id in project_ids:
        if not current_project_id:
            continue
        meta = store.read_project_meta(current_project_id)
        if meta.status != "active":
            continue
        checked_projects += 1

        plan = store.read_project_plan(current_project_id)
        plan_type = parse_plan_type(plan)
        tasks = parse_loop_tasks(plan) if plan_type == "loop" else parse_dag_tasks(plan)
        project_issue_start = len(issues)
        for task in tasks:
            if task.status != "delegated":
                continue

            base = {
                "projectId": meta.project_id,
                "projectTitle": meta.title,
                "projectStatus": meta.status,
                "planType": plan_type,
                "taskId": task.task_id,
                "taskTitle": task.title,
                "assignedTo": task.assigned_to,
                "planStatus": task.status,
                "taskPath": f"shared/tasks/{task.task_id}/",
            }

            try:
                task_meta = store.read_task_meta(task.task_id)
                task_status = task_meta.status
            except TaskflowError:
                issues.append(
                    {
                        **base,
                        "taskStatus": "missing",
                        "issueType": "missing_task_meta",
                        "recommendation": "leader_repair_or_delegate_again",
                    }
                )
                continue

            try:
                result = store.read_task_result(task.task_id)
                issues.append(
                    {
                        **base,
                        "taskStatus": task_status,
                        "resultStatus": result.status,
                        "issueType": "task_result_pending_check",
                        "recommendation": "leader_check_submitted_result",
                    }
                )
                continue
            except TaskflowError as exc:
                task_dir = store.shared_dir / "tasks" / task.task_id
                if (task_dir / "result.md").exists():
                    issues.append(
                        {
                            **base,
                            "taskStatus": task_status,
                            "issueType": "invalid_task_result",
                            "error": str(exc),
                            "recommendation": "ask_worker_fix_result_protocol",
                        }
                    )
                    continue

            runtime = await _fetch_worker_runtime_status(
                task.assigned_to,
                timeout_seconds=timeout_seconds,
            )
            runtime_status = runtime.get("runtimeStatus")
            if runtime_status == "running":
                continue
            if runtime_status == "idle":
                issues.append(
                    {
                        **base,
                        "taskStatus": task_status,
                        **runtime,
                        "issueType": "task_not_running",
                        "recommendation": "ask_worker_continue_task",
                    }
                )
            else:
                issues.append(
                    {
                        **base,
                        "taskStatus": task_status,
                        **runtime,
                        "issueType": "worker_runtime_unknown",
                        "recommendation": "inspect_worker_runtime",
                    }
                )

        if len(issues) > project_issue_start:
            continue

        ready = (
            ready_loop_nodes(store, project_id=current_project_id)
            if plan_type == "loop"
            else ready_nodes(store, project_id=current_project_id)
        )
        if ready:
            issues.append(
                {
                    "projectId": meta.project_id,
                    "projectTitle": meta.title,
                    "projectStatus": meta.status,
                    "planType": plan_type,
                    "issueType": "ready_tasks_pending",
                    "readyTasks": [_task_payload(task) for task in ready],
                    "recommendation": "normal_leader_schedule_ready_tasks",
                }
            )
            continue

        if not tasks or not all(task.status == "completed" for task in tasks):
            continue

        if plan_type == "loop":
            loop = parse_loop_plan(plan)
            if loop is not None and loop.status == "running":
                issues.append(
                    {
                        "projectId": meta.project_id,
                        "projectTitle": meta.title,
                        "projectStatus": meta.status,
                        "planType": plan_type,
                        "issueType": "loop_iteration_decision_pending",
                        "iteration": loop.current_iteration,
                        "maxIterations": loop.max_iterations,
                        "recommendation": "normal_leader_record_loop_iteration",
                    }
                )
            continue

        issues.append(
            {
                "projectId": meta.project_id,
                "projectTitle": meta.title,
                "projectStatus": meta.status,
                "planType": plan_type,
                "issueType": "project_completion_pending",
                "recommendation": "normal_leader_aggregate_and_complete_project",
            }
        )

    return {
        "checkedProjects": checked_projects,
        "issues": issues,
    }


async def projectflow(
    action: str,
    payload: dict[str, Any] | str | None = None,
    dryRun: bool = False,
) -> ToolResponse:
    """Manage HiClaw project execution plans with action-specific payload fields."""
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

        if action == "plan_dag":
            project_id = _required_str(payload_data, "projectId")
            tasks_payload = _coerce_tasks(payload_data.get("tasks"))
            if dryRun:
                return _ok(dryRun=True, action=action, projectId=project_id, tasks=tasks_payload)
            graph = plan_dag(store, project_id=project_id, tasks=tasks_payload)
            ready = ready_nodes(store, project_id=project_id)
            return _ok(
                action=action,
                tasks=[asdict(task) for task in graph],
                readyNodes=[asdict(task) for task in ready],
            )

        if action == "plan_loop":
            project_id = _required_str(payload_data, "projectId")
            goal = _required_str(payload_data, "goal")
            stop_condition = _required_str(payload_data, "stopCondition")
            iteration_template = _required_str(payload_data, "iterationTemplate")
            max_iterations = _required_int(payload_data, "maxIterations")
            current_iteration = _optional_int(payload_data, "currentIteration", 0)
            status = str(payload_data.get("status") or "running")
            tasks_payload = _coerce_optional_tasks(payload_data.get("tasks"))
            if dryRun:
                return _ok(
                    dryRun=True,
                    action=action,
                    projectId=project_id,
                    goal=goal,
                    stopCondition=stop_condition,
                    iterationTemplate=iteration_template,
                    maxIterations=max_iterations,
                    currentIteration=current_iteration,
                    status=status,
                    tasks=tasks_payload,
                )
            loop = plan_loop(
                store,
                project_id=project_id,
                goal=goal,
                stop_condition=stop_condition,
                iteration_template=iteration_template,
                max_iterations=max_iterations,
                current_iteration=current_iteration,
                status=status,
                tasks=tasks_payload,
            )
            ready = ready_loop_nodes(store, project_id=project_id)
            return _ok(
                action=action,
                loop=asdict(loop),
                readyNodes=[asdict(task) for task in ready],
            )

        if action == "ready_nodes":
            project_id = _required_str(payload_data, "projectId")
            ready = ready_nodes(store, project_id=project_id)
            return _ok(action=action, readyNodes=[asdict(task) for task in ready])

        if action == "ready_loop_nodes":
            project_id = _required_str(payload_data, "projectId")
            ready = ready_loop_nodes(store, project_id=project_id)
            return _ok(action=action, readyNodes=[asdict(task) for task in ready])

        if action == "check_active_tasks":
            project_id = _optional_str(payload_data, "projectId")
            timeout_seconds = _optional_int(payload_data, "timeoutSeconds", 3)
            if timeout_seconds < 1:
                raise TaskflowError("payload.timeoutSeconds must be greater than zero")
            if dryRun:
                return _ok(
                    dryRun=True,
                    action=action,
                    projectId=project_id,
                    timeoutSeconds=timeout_seconds,
                )
            result = await _check_active_tasks(
                store,
                project_id=project_id,
                timeout_seconds=timeout_seconds,
            )
            return _ok(action=action, **result)

        if action == "record_loop_iteration":
            project_id = _required_str(payload_data, "projectId")
            iteration = _required_int(payload_data, "iteration")
            decision = _required_str(payload_data, "decision")
            summary = _required_str(payload_data, "summary")
            next_action = _optional_str(payload_data, "nextAction")
            if dryRun:
                return _ok(
                    dryRun=True,
                    action=action,
                    projectId=project_id,
                    iteration=iteration,
                    decision=decision,
                    summary=summary,
                    nextAction=next_action,
                )
            loop = record_loop_iteration(
                store,
                project_id=project_id,
                iteration=iteration,
                decision=decision,
                summary=summary,
                next_action=next_action,
            )
            return _ok(action=action, loop=asdict(loop))

        if action == "pause_project":
            project_id = _required_str(payload_data, "projectId")
            if dryRun:
                return _ok(
                    dryRun=True,
                    action=action,
                    projectId=project_id,
                )
            meta = pause_project(store, project_id=project_id)
            return _ok(action=action, project=asdict(meta))

        if action == "resume_project":
            project_id = _required_str(payload_data, "projectId")
            if dryRun:
                return _ok(
                    dryRun=True,
                    action=action,
                    projectId=project_id,
                )
            meta = resume_project(store, project_id=project_id)
            return _ok(action=action, project=asdict(meta))

        if action == "complete_project":
            project_id = _required_str(payload_data, "projectId")
            if dryRun:
                return _ok(
                    dryRun=True,
                    action=action,
                    projectId=project_id,
                )
            meta = complete_project(store, project_id=project_id)
            return _ok(action=action, project=asdict(meta))

        raise TaskflowError(
            "action must be one of: create_project, plan_dag, ready_nodes, "
            "plan_loop, ready_loop_nodes, record_loop_iteration, "
            "check_active_tasks, pause_project, resume_project, complete_project",
        )
    except TaskflowError as exc:
        return _error(
            str(exc),
            action=action,
            projectId=payload_data.get("projectId"),
        )
    except Exception as exc:  # pragma: no cover - defensive tool boundary
        return _error(
            f"projectflow failed: {exc}",
            action=action,
            projectId=payload_data.get("projectId"),
        )
