# project state normalization, task graph validation and plan writing (W1-a2: real module)
from __future__ import annotations

import time

from pathlib import Path

from typing import Any

# Cross-group imports are top-level absolutes (see lifecycle/transitions.py
# note): mcp/ on sys.path keeps one module identity in both load modes.
from mcp.common import refs
from mcp.common.io import _atomic_write_text, _project_timestamp, _read_json
from mcp.common.paths import _project_state_path, _task_state_path
from mcp.common.payload import _safe_id, _slugify

# Call-time lookups (common.refs): monkeypatch-sensitive names resolve
# against the bound mcp/server.py namespace at call time, so test
# monkeypatches of server attributes stay visible.
_sync_project = refs.proxy("_sync_project")
_write_json = refs.proxy("_write_json")


def _unique_project_id(arguments: dict[str, Any], base_id: str) -> str:
    project_id = _safe_id(base_id, "projectId")
    if not _project_state_path(arguments, project_id).exists():
        return project_id
    for index in range(1, 1000):
        candidate = _safe_id(f"{base_id}-{index:02d}", "projectId")
        if not _project_state_path(arguments, candidate).exists():
            return candidate
    raise ValueError(f"cannot allocate unique project id for: {base_id}")


def _project_id_from_payload(arguments: dict[str, Any], payload: dict[str, Any]) -> str:
    explicit = payload.get("projectId") or payload.get("project_id")
    if explicit:
        project_id = _safe_id(explicit, "projectId")
        if _project_state_path(arguments, project_id).exists():
            raise ValueError(f"project already exists: {project_id}")
        return project_id
    title = str(payload.get("title") or payload.get("name") or "project")
    base_id = f"{_slugify(title, 'project')}-{_project_timestamp()}"
    return _unique_project_id(arguments, base_id)


def _normalize_task(raw: dict[str, Any], previous: dict[str, Any] | None = None) -> dict[str, Any]:
    task_id = _safe_id(raw.get("taskId") or raw.get("task_id"), "taskId")
    previous = previous or {}
    status = str(raw.get("status") or previous.get("status") or "planned")
    if status == "pending":
        status = "planned"
    cancellation = previous.get("cancellation") if isinstance(previous.get("cancellation"), dict) else None
    if cancellation and raw.get("status") is not None and status != str(previous.get("status") or ""):
        raise ValueError(f"task {task_id} has a committed cancellation and cannot be reopened")
    normalized = {
        "task_id": task_id,
        "title": str(raw.get("title") or previous.get("title") or task_id),
        "assigned_to": str(raw.get("assignedTo") or raw.get("assigned_to") or previous.get("assigned_to") or ""),
        "depends_on": [str(item) for item in (raw.get("dependsOn") or raw.get("depends_on") or previous.get("depends_on") or [])],
        "status": status,
    }
    if cancellation:
        normalized["cancellation"] = dict(cancellation)
    return normalized


def _normalize_tasks(
    raw_tasks: list[Any],
    previous: dict[str, dict[str, Any]],
) -> list[dict[str, Any]]:
    planned = [
        _normalize_task(task, previous.get(str(task.get("taskId") or task.get("task_id"))))
        for task in raw_tasks
        if isinstance(task, dict)
    ]
    included = {str(task.get("task_id") or "") for task in planned}
    for task_id, old_task in previous.items():
        if isinstance(old_task.get("cancellation"), dict) and task_id not in included:
            raise ValueError(f"task {task_id} has a committed cancellation and cannot be removed")
    return planned


def _validate_task_graph(tasks: list[dict[str, Any]]) -> None:
    seen: set[str] = set()
    task_ids: set[str] = set()
    for task in tasks:
        task_id = str(task.get("task_id") or "")
        if task_id in seen:
            raise ValueError(f"duplicate task id: {task_id}")
        seen.add(task_id)
        task_ids.add(task_id)

    for task in tasks:
        task_id = str(task.get("task_id") or "")
        for dep in task.get("depends_on", []):
            if dep not in task_ids:
                raise ValueError(f"task {task_id} depends on unknown task: {dep}")

    visiting: set[str] = set()
    visited: set[str] = set()
    deps_by_id = {
        str(task.get("task_id")): [str(dep) for dep in task.get("depends_on", [])]
        for task in tasks
    }

    def visit(task_id: str, path: list[str]) -> None:
        if task_id in visited:
            return
        if task_id in visiting:
            raise ValueError(f"task dependency cycle detected: {' -> '.join(path + [task_id])}")
        visiting.add(task_id)
        for dep in deps_by_id.get(task_id, []):
            visit(dep, path + [task_id])
        visiting.remove(task_id)
        visited.add(task_id)

    for task_id in deps_by_id:
        visit(task_id, [])


def _safe_loop_status(value: Any) -> str:
    status = str(value or "running").strip()
    allowed = {"running", "waiting_user", "completed", "blocked"}
    if status not in allowed:
        raise ValueError(f"status must be one of: {', '.join(sorted(allowed))}")
    return status


def _safe_loop_decision(value: Any) -> str:
    decision = str(value or "").strip()
    allowed = {"continue", "replan", "ask_user", "stop_success", "stop_blocked"}
    if decision not in allowed:
        raise ValueError(f"decision must be one of: {', '.join(sorted(allowed))}")
    return decision


def _write_project_plan(project_dir: Path, project: dict[str, Any]) -> None:
    lines = [
        f"# {project.get('title') or project.get('project_id')}",
        "",
        f"- Project ID: `{project.get('project_id')}`",
        f"- Status: `{project.get('status')}`",
    ]
    plan_type = str(project.get("plan_type") or "").strip()
    if plan_type:
        lines.append(f"- Plan Type: `{plan_type}`")
    requester = project.get("requester")
    if requester:
        lines.append(f"- Requester: `{requester}`")
    source_room_id = project.get("source_room_id")
    if source_room_id:
        lines.append(f"- Source Room ID: `{source_room_id}`")
    reply_route = project.get("reply_route")
    if isinstance(reply_route, dict):
        channel = str(reply_route.get("channel") or "").strip()
        target_user = str(reply_route.get("target_user") or "").strip()
        target_session = str(reply_route.get("target_session") or "").strip()
        if channel == "matrix" and target_session:
            target = f"{target_user}/{target_session}" if target_user else target_session
            lines.append(f"- Reply Route: `{channel}/{target}`")
        elif channel and target_user and target_session:
            lines.append(f"- Reply Route: `{channel}/{target_user}/{target_session}`")
    loop = project.get("loop") if isinstance(project.get("loop"), dict) else {}
    if plan_type == "loop":
        lines.extend([
            f"- Loop Goal: {loop.get('goal') or ''}",
            f"- Stop Condition: {loop.get('stop_condition') or ''}",
            f"- Current Iteration: `{loop.get('current_iteration', 0)}` / `{loop.get('max_iterations', 0)}`",
            f"- Loop Status: `{loop.get('status') or 'running'}`",
            "",
            "## Iteration Template",
            "",
            str(loop.get("iteration_template") or ""),
        ])
        tasks = loop.get("tasks", []) if isinstance(loop.get("tasks"), list) else []
    else:
        tasks = project.get("tasks", []) if isinstance(project.get("tasks"), list) else []
    lines.extend(["", "## Tasks"])
    for task in tasks:
        deps = ", ".join(task.get("depends_on", [])) or "none"
        owner = task.get("assigned_to") or "unassigned"
        lines.append(f"- `{task['task_id']}` {task.get('title')} -> {owner}; deps: {deps}; status: {task.get('status')}")
    if plan_type == "loop":
        history = loop.get("history", []) if isinstance(loop.get("history"), list) else []
        if history:
            lines.extend(["", "## Loop History"])
            for item in history:
                if isinstance(item, dict):
                    iteration = item.get("iteration")
                    decision = item.get("decision")
                    summary = item.get("summary")
                    next_action = item.get("next_action")
                    detail = f"- Iteration {iteration}: `{decision}` - {summary}"
                    if next_action:
                        detail += f" Next: {next_action}"
                    lines.append(detail)
                else:
                    lines.append(f"- {item}")
    project_dir.mkdir(parents=True, exist_ok=True)
    _atomic_write_text(project_dir / "plan.md", "\n".join(lines) + "\n")


refs.define("_write_project_plan", _write_project_plan)


def _ready_nodes(project: dict[str, Any]) -> list[dict[str, Any]]:
    if project.get("plan_type") == "loop":
        raise ValueError(f"project plan is not a DAG: {project.get('project_id')}")
    if str(project.get("status") or "active") != "active":
        return []
    tasks = project.get("tasks", []) if isinstance(project.get("tasks"), list) else []
    status_by_id = {task.get("task_id"): task.get("status") for task in tasks}
    ready: list[dict[str, Any]] = []
    for task in tasks:
        if task.get("status") not in {"planned", "assigned"}:
            continue
        if all(status_by_id.get(dep) == "completed" for dep in task.get("depends_on", [])):
            ready.append(task)
    return ready


def _loop_pause_state(project: dict[str, Any]) -> tuple[bool, list[str]]:
    """Return (paused, sources). loop.pause_sources plus legacy status=paused."""
    loop = project.get("loop") if isinstance(project.get("loop"), dict) else {}
    raw_sources = loop.get("pause_sources")
    raw_sources = raw_sources if isinstance(raw_sources, list) else []
    sources = {str(source).strip().lower() for source in raw_sources if str(source).strip()}
    if project.get("status") == "paused":
        sources.add("project")
    return bool(sources), sorted(sources)


def _ready_loop_nodes(project: dict[str, Any]) -> list[dict[str, Any]]:
    if _loop_pause_state(project)[0]:
        return []
    if str(project.get("status") or "active") != "active":
        return []
    loop = project.get("loop")
    if not isinstance(loop, dict):
        raise ValueError(f"project has no loop plan: {project.get('project_id')}")
    if str(loop.get("status") or "running") in {"completed", "blocked", "waiting_user"}:
        return []
    tasks = loop.get("tasks", []) if isinstance(loop.get("tasks"), list) else []
    status_by_id = {task.get("task_id"): task.get("status") for task in tasks}
    ready: list[dict[str, Any]] = []
    for task in tasks:
        if task.get("status") not in {"planned", "assigned"}:
            continue
        if all(status_by_id.get(dep) == "completed" for dep in task.get("depends_on", [])):
            ready.append(task)
    return ready


def _resolve_project(arguments: dict[str, Any], payload: dict[str, Any]) -> dict[str, Any]:
    task: dict[str, Any] = {}
    project_id_value = payload.get("projectId") or payload.get("project_id")
    task_id_value = payload.get("taskId") or payload.get("task_id")
    if task_id_value:
        task_id = _safe_id(task_id_value, "taskId")
        task = _read_json(_task_state_path(arguments, task_id))
        if not task:
            raise ValueError("task not found")
        project_id_value = task.get("project_id")
    if not project_id_value:
        raise ValueError("projectId or taskId is required")
    project_id = _safe_id(project_id_value, "projectId")
    project = _read_json(_project_state_path(arguments, project_id))
    if not project:
        raise ValueError("project not found")
    plan_type = str(project.get("plan_type") or "dag")
    ready = _ready_loop_nodes(project) if plan_type == "loop" else _ready_nodes(project)
    result = {
        "ok": True,
        "tool": "projectflow",
        "action": "resolve_project",
        "project": project,
        "planType": plan_type,
        "replyRoute": project.get("reply_route"),
        "sourceRoomId": project.get("source_room_id") or (task.get("source_room_id") if task else None),
        "readyNodes": ready,
    }
    if task:
        result["task"] = task
    return result


def _project_task_status(project: dict[str, Any], task_id: str) -> str:
    tasks = project.get("tasks", []) if isinstance(project.get("tasks"), list) else []
    loop = project.get("loop") if isinstance(project.get("loop"), dict) else {}
    loop_tasks = loop.get("tasks", []) if isinstance(loop.get("tasks"), list) else []
    for task in tasks + loop_tasks:
        if isinstance(task, dict) and task.get("task_id") == task_id:
            return str(task.get("status") or "")
    return ""


def _mark_requester_report_sent(arguments: dict[str, Any], payload: dict[str, Any]) -> dict[str, Any]:
    project_id = _safe_id(payload.get("projectId") or payload.get("project_id"), "projectId")
    project = _read_json(_project_state_path(arguments, project_id))
    if not project:
        raise ValueError("project not found")
    requester_report = project.get("requester_report") if isinstance(project.get("requester_report"), dict) else {}
    requester_report["pending"] = False
    requester_report["sent_at"] = str(payload.get("sentAt") or payload.get("sent_at") or time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()))
    project["requester_report"] = requester_report
    _write_json(_project_state_path(arguments, project_id), project)
    _sync_project(arguments, project_id)
    return {
        "ok": True,
        "tool": "projectflow",
        "action": "mark_requester_report_sent",
        "project": project,
    }


def _validate_loop_id_mapping(
    arguments: dict[str, Any],
    project_id: str,
    loop: dict[str, Any],
    payload: dict[str, Any],
) -> dict[str, Any] | None:
    """Validate the loop task mapping before plan_loop writes any state.

    Only the project state (B) <-> task meta (C) directions are checked:
    plan_loop has no k8s client and never reads the Loop CRD (A). Keeping
    A and B consistent is the reconciler's job; a provided ``loopId`` is
    recorded in the loop dict purely as the anchor for that later check.

    The loop's ``task_id`` (B) is a stable anchor: an existing loop cannot
    be re-pointed at a different task, and the task meta (C) of the mapped
    task must not claim a different identity. Returns the structured
    ``loop_id_mismatch`` error on conflict (the caller must perform no
    writes), or ``None`` when the mapping is consistent or unmapped
    (unmapped is allowed, keeping legacy calls byte-identical).
    """
    task_id = loop.get("task_id")
    if not task_id:
        return None
    project_state = _read_json(_project_state_path(arguments, project_id), {})
    previous_loop = project_state.get("loop") if isinstance(project_state.get("loop"), dict) else {}
    previous_task_id = previous_loop.get("task_id")
    task_meta = _read_json(_task_state_path(arguments, task_id))
    meta_task_id = task_meta.get("task_id") if task_meta else None
    if previous_task_id and previous_task_id != task_id:
        return {
            "ok": False,
            "error": "loop_id_mismatch",
            "crd_task_id": None,
            "project_loop_task_id": previous_task_id,
            "task_meta_task_id": meta_task_id,
        }
    if meta_task_id and meta_task_id != task_id:
        return {
            "ok": False,
            "error": "loop_id_mismatch",
            "crd_task_id": None,
            "project_loop_task_id": task_id,
            "task_meta_task_id": meta_task_id,
        }
    return None


