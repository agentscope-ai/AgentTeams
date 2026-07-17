"""Bridge TeamHarness MCP helpers to agentteams_protocol shared domain."""

from __future__ import annotations

from typing import Any

from agentteams_protocol.task import (
    DagTask,
    TaskResult,
    TaskflowError,
    canonical_worker_id,
    parse_task_result,
    validate_dag,
    validate_task_result,
)


def safe_id(value: Any, field: str) -> str:
    """Validate id using protocol rules; raise ValueError for MCP callers."""
    text = str(value or "").strip()
    if not text:
        raise ValueError(f"{field} is required")
    try:
        from agentteams_protocol.task import safe_id as protocol_safe_id

        return protocol_safe_id(text, field)
    except TaskflowError as exc:
        raise ValueError(str(exc)) from exc


def validate_task_graph(tasks: list[dict[str, Any]]) -> None:
    """Validate DAG shape using shared protocol (MCP task dicts)."""
    dag_tasks = [
        DagTask(
            task_id=str(task.get("task_id") or ""),
            title=str(task.get("title") or task.get("task_id") or ""),
            assigned_to=str(task.get("assigned_to") or ""),
            depends_on=[str(dep) for dep in (task.get("depends_on") or [])],
            status="pending",
        )
        for task in tasks
    ]
    try:
        validate_dag(dag_tasks)
    except TaskflowError as exc:
        raise ValueError(str(exc)) from exc


def validate_deliverables(task_id: str, deliverables: list[Any]) -> list[str]:
    """Validate deliverable paths via protocol result rules."""
    normalized = [str(item).strip() for item in deliverables if str(item).strip()]
    try:
        safe_task_id = safe_id(task_id, "taskId")
    except ValueError:
        raise
    result = TaskResult(status="SUCCESS", summary="validation", deliverables=normalized)
    try:
        validate_task_result(safe_task_id, result)
    except TaskflowError as exc:
        raise ValueError(str(exc)) from exc
    return normalized


def parse_result_markdown(text: str) -> TaskResult:
    try:
        return parse_task_result(text)
    except TaskflowError as exc:
        raise ValueError(str(exc)) from exc


def normalize_worker_id(value: str | None) -> str:
    return canonical_worker_id(value)


def protocol_core_enabled() -> bool:
    import os

    return os.getenv("AGENTTEAMS_REFACTOR_PROTOCOL_CORE", "").strip() == "1"


def dual_run_validate_dag(tasks: list[dict[str, Any]]) -> None:
    """When AGENTTEAMS_REFACTOR_PROTOCOL_CORE=1, re-validate DAG via shared protocol."""
    if not protocol_core_enabled():
        return
    validate_task_graph(tasks)
