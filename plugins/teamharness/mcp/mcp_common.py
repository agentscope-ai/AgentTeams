"""Shared TeamHarness MCP project/task protocol helpers."""

from __future__ import annotations

import datetime
import json
import os
import re
import time
from pathlib import Path
from typing import Any

from common.runtime_config import load_runtime_config, section as _runtime_section
from protocol_bridge import validate_task_graph as _protocol_validate_task_graph

MC_ALIAS = "agentteams"
ALLOWED_TASK_RESULT_STATUSES = {"SUCCESS", "SUCCESS_WITH_NOTES", "REVISION_NEEDED", "BLOCKED", "FAILED", "PARTIAL"}
TERMINAL_TASK_STATUSES = {"completed", "revision", "blocked", "cancelled"}


def _matrix_target(target: str) -> tuple[str, str]:
    import server as _server

    return _server._matrix_target(target)


def _attachment_parent_event_id(*sources: dict[str, Any]) -> str:
    import server as _server

    return _server._attachment_parent_event_id(*sources)


def _publish_task_artifacts(
    arguments: dict[str, Any],
    task: dict[str, Any],
    task_id: str,
    deliverables: list[Any],
    parent_event_id: str,
) -> list[dict[str, Any]]:
    import server as _server

    return _server._publish_task_artifacts(arguments, task, task_id, deliverables, parent_event_id)


def _publish_project_artifacts(
    arguments: dict[str, Any],
    project: dict[str, Any],
    project_id: str,
    task_id: str,
    parent_event_id: str,
) -> list[dict[str, Any]]:
    import server as _server

    return _server._publish_project_artifacts(arguments, project, project_id, task_id, parent_event_id)


def _normalize_workspace_artifact_path(raw_path: str) -> tuple[str, bool]:
    import server as _server

    return _server._normalize_workspace_artifact_path(raw_path)


def _path_is_under(normalized: str, prefix: str) -> bool:
    import server as _server

    return _server._path_is_under(normalized, prefix)


def _accept_task_result(arguments: dict[str, Any], payload: dict[str, Any]) -> dict[str, Any]:
    project_id = _safe_id(payload.get("projectId") or payload.get("project_id"), "projectId")
    task_id = _safe_id(payload.get("taskId") or payload.get("task_id"), "taskId")
    project = _read_json(_project_state_path(arguments, project_id))
    if not project:
        raise ValueError("project not found")
    result_status_value = payload.get("resultStatus") or payload.get("result_status")
    accepted = _payload_bool(payload.get("accepted"), True)
    node_status = _accepted_node_status(result_status_value)
    if not accepted and node_status == "completed":
        result_status_value = "REVISION_NEEDED"
        node_status = "revision"
    changed = False
    for task in project.get("tasks", []):
        if task.get("task_id") == task_id:
            task["status"] = node_status
            changed = True
            break
    loop = project.get("loop") if isinstance(project.get("loop"), dict) else {}
    loop_tasks = loop.get("tasks", []) if isinstance(loop.get("tasks"), list) else []
    for task in loop_tasks:
        if task.get("task_id") == task_id:
            task["status"] = node_status
            project["loop"] = loop
            changed = True
            break
    if not changed:
        raise ValueError("task not found in project plan")
    result_status = str(result_status_value or "SUCCESS")
    if node_status == "completed":
        project["requester_report"] = {
            "pending": True,
            "reason": "task_result_accepted",
            "task_id": task_id,
            "result_status": result_status,
            "summary": str(payload.get("summary") or ""),
            "report_path": f"shared/projects/{project_id}/result.md",
        }
    else:
        requester_report = project.get("requester_report") if isinstance(project.get("requester_report"), dict) else {}
        if requester_report.get("task_id") == task_id:
            requester_report["pending"] = False
            requester_report["reason"] = f"task_result_{node_status}"
            project["requester_report"] = requester_report
    _write_json(_project_state_path(arguments, project_id), project)
    _write_project_plan(_project_dir(arguments, project_id), project)
    publish_artifacts = _payload_bool_field(payload, ("publishArtifacts", "publish_artifacts"), False)
    published_artifacts = (
        _publish_project_artifacts(
            arguments,
            project,
            project_id,
            task_id,
            _attachment_parent_event_id(payload, arguments),
        )
        if node_status == "completed" and publish_artifacts else []
    )
    requester_report = project.get("requester_report") if isinstance(project.get("requester_report"), dict) else {}
    requester_report_pending = requester_report.get("pending") is True and requester_report.get("task_id") == task_id
    return {
        "ok": True,
        "tool": "projectflow",
        "action": "accept_task_result",
        "project": project,
        "taskId": task_id,
        "nodeStatus": node_status,
        "accepted": node_status == "completed",
        "publishedArtifacts": published_artifacts,
        "notificationNeeded": _notification_needed(
            "accept_task_result",
            project,
            summary=f"accept_task_result: {task_id} -> {node_status}",
            include_reply_route=requester_report_pending,
        ),
    }

def _accepted_node_status(result_status: Any) -> str:
    status = str(result_status or "SUCCESS").strip()
    if status in {"SUCCESS", "SUCCESS_WITH_NOTES"}:
        return "completed"
    if status == "REVISION_NEEDED":
        return "revision"
    if status in {"BLOCKED", "INTERRUPTED"}:
        return "blocked"
    raise ValueError(f"unsupported result status: {status}")

def _canonical_room_id(value: Any) -> str:
    text = str(value or "").strip()
    if text.startswith("room:"):
        text = text[len("room:") :].strip()
    return text

def _storage_root_prefix() -> str:
    return os.getenv("AGENTTEAMS_STORAGE_PREFIX", "").strip().strip("/")


def _with_storage_root(prefix: str) -> str:
    clean = (prefix or "").strip().strip("/")
    root = _storage_root_prefix()
    if not root:
        return clean
    if not clean:
        return root
    if clean == root or clean.startswith(f"{root}/"):
        return clean
    return f"{root}/{clean}"


def _default_global_shared_prefix() -> str:
    """Derive global-shared storage prefix from environment or runtime.yaml."""
    storage = _section(_load_runtime_config(), "storage")
    prefix = str(storage.get("globalSharedPrefix") or "").strip()
    if prefix:
        return _with_storage_root(prefix)
    storage_prefix = _storage_root_prefix()
    if storage_prefix:
        return f"{storage_prefix}/shared"
    return "shared"

def _default_shared_prefix() -> str:
    """Derive storage shared prefix from environment or runtime.yaml."""
    configured = os.getenv("AGENTTEAMS_SHARED_STORAGE_PREFIX", "").strip()
    if configured:
        return _with_storage_root(configured)
    storage = _section(_load_runtime_config(), "storage")
    prefix = str(storage.get("sharedPrefix") or "").strip()
    if prefix:
        return _with_storage_root(prefix)
    storage_prefix = _storage_root_prefix()
    if storage_prefix:
        return f"{storage_prefix}/shared"
    return "shared"

def _default_workspace_dir() -> str:
    """Derive workspace dir from environment (set by qwenpaw-worker / copaw-worker)."""
    for env_key in ("QWENPAW_WORKING_DIR", "COPAW_WORKING_DIR"):
        working_dir = os.getenv(env_key, "").strip()
        if working_dir:
            return str(Path(working_dir) / "workspaces" / "default")
    shared_dir = os.getenv("TEAMHARNESS_SHARED_DIR", "").strip() or os.getenv("AGENTTEAMS_SHARED_DIR", "").strip()
    if shared_dir:
        return str(Path(shared_dir).parent)
    return ""

def _ensure_console_task_meta(arguments: dict[str, Any], task: dict[str, Any]) -> None:
    _preserve_task_meta_fields(arguments, task)
    project_task = _project_task_for_meta(arguments, task)
    task["task_id"] = _first_text(task.get("task_id"), task.get("taskId"))
    task["project_id"] = _first_text(task.get("project_id"), task.get("projectId"))
    task["room_id"] = _first_text(task.get("room_id"), task.get("roomId"))
    task["spec_path"] = _first_text(task.get("spec_path"), task.get("specPath"))
    source_room_id = _first_text(task.get("source_room_id"), task.get("sourceRoomId"), project_task.get("source_room_id"))
    if source_room_id:
        task["source_room_id"] = source_room_id
    task["task_title"] = _first_text(
        task.get("task_title"),
        task.get("taskTitle"),
        project_task.get("title"),
        task.get("title"),
        task["task_id"],
    )
    task["assigned_to"] = _first_text(
        task.get("assigned_to"),
        task.get("assignedTo"),
        project_task.get("assigned_to"),
        project_task.get("assignedTo"),
    )
    task["assigned_at"] = _first_text(
        task.get("assigned_at"),
        task.get("assignedAt"),
        task.get("created_at"),
        task.get("createdAt"),
    ) or _utc_timestamp()
    for snake_key, camel_key in (
        ("acknowledged_by_role", "acknowledgedByRole"),
        ("result_status", "resultStatus"),
        ("result_path", "resultPath"),
        ("submitted_by_role", "submittedByRole"),
    ):
        value = _first_text(task.get(snake_key), task.get(camel_key))
        if value:
            task[snake_key] = value
    for key in (
        "taskId",
        "projectId",
        "roomId",
        "specPath",
        "sourceRoomId",
        "taskTitle",
        "assignedTo",
        "assignedAt",
        "createdAt",
        "acknowledgedByRole",
        "resultStatus",
        "resultPath",
        "submittedByRole",
    ):
        task.pop(key, None)

def _external_requester_channel(project: dict[str, Any]) -> str:
    reply_route = project.get("reply_route") if isinstance(project.get("reply_route"), dict) else {}
    channel = str(reply_route.get("channel") or project.get("source") or "").strip().lower()
    if channel and channel != "matrix":
        return channel
    requester_route = _reply_route_from_requester(project.get("requester"))
    channel = str(requester_route.get("channel") or "").strip().lower()
    return channel if channel and channel != "matrix" else ""

def _first_text(*values: Any) -> str:
    for value in values:
        text = str(value or "").strip()
        if text:
            return text
    return ""

def _load_runtime_config() -> dict[str, Any]:
    return load_runtime_config()

def _load_task(arguments: dict[str, Any], task_id: str) -> dict[str, Any]:
    task = _read_json(_task_state_path(arguments, task_id))
    if not task:
        raise ValueError("task not found")
    if not task.get("task_id") and task.get("taskId"):
        task["task_id"] = str(task["taskId"])
    return task

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
    return {
        "ok": True,
        "tool": "projectflow",
        "action": "mark_requester_report_sent",
        "project": project,
    }

def _non_negative_int(value: Any, field: str) -> int:
    try:
        parsed = int(value)
    except (TypeError, ValueError):
        raise ValueError(f"{field} must be an integer") from None
    if parsed < 0:
        raise ValueError(f"{field} must be zero or greater")
    return parsed

def _normalize_reply_route(raw: Any) -> dict[str, str]:
    route = raw if isinstance(raw, dict) else {}
    channel = str(route.get("channel") or "").strip()
    target_user = str(route.get("targetUser") or route.get("target_user") or route.get("userId") or route.get("user_id") or "").strip()
    target_session = str(route.get("targetSession") or route.get("target_session") or route.get("sessionId") or route.get("session_id") or "").strip()
    if channel.lower() == "matrix":
        target = str(
            route.get("target")
            or route.get("roomId")
            or route.get("room_id")
            or route.get("targetRoom")
            or route.get("target_room")
            or target_session
            or ""
        ).strip()
        if not target:
            return {}
        try:
            target_kind, target_id = _matrix_target(target)
        except ValueError:
            return {}
        if target_kind != "room":
            return {}
        normalized = {
            "channel": "matrix",
            "target_session": target_id,
        }
        if target_user:
            normalized["target_user"] = target_user
        return normalized
    if not (channel and target_user and target_session):
        return {}
    return {
        "channel": channel,
        "target_user": target_user,
        "target_session": target_session,
    }

def _normalize_role(role: str) -> str:
    value = (role or "").strip().replace("_", "-").lower()
    return {
        "team-leader": "leader",
        "teamleader": "leader",
        "team-leader-agent": "leader",
        "remote": "remote-member",
        "remote-member-agent": "remote-member",
    }.get(value, value)

def _normalize_task(raw: dict[str, Any], previous: dict[str, Any] | None = None) -> dict[str, Any]:
    task_id = _safe_id(raw.get("taskId") or raw.get("task_id"), "taskId")
    previous = previous or {}
    status = str(raw.get("status") or previous.get("status") or "planned")
    if status == "pending":
        status = "planned"
    return {
        "task_id": task_id,
        "title": str(raw.get("title") or previous.get("title") or task_id),
        "assigned_to": str(raw.get("assignedTo") or raw.get("assigned_to") or previous.get("assigned_to") or ""),
        "depends_on": [str(item) for item in (raw.get("dependsOn") or raw.get("depends_on") or previous.get("depends_on") or [])],
        "status": status,
    }

def _notification_needed(
    action: str,
    project: dict[str, Any],
    task: dict[str, Any] | None = None,
    summary: str = "",
    include_reply_route: bool = True,
) -> dict[str, Any]:
    """Build a notificationNeeded hint for the calling agent.

    This does NOT send any message. It returns structured metadata that tells the
    agent which room to notify and what changed, so the agent can follow up with
    a message tool call.
    """
    project_id = str(project.get("project_id") or "")
    title = str(project.get("title") or project_id)
    # Determine best target room
    target_room = ""
    if task and str(task.get("room_id") or ""):
        target_room = str(task["room_id"])
    elif str(project.get("source_room_id") or ""):
        target_room = str(project["source_room_id"])
    reply_route = project.get("reply_route") if include_reply_route and isinstance(project.get("reply_route"), dict) else {}
    if not target_room and reply_route:
        target_session = str(reply_route.get("target_session") or "").strip()
        if target_session:
            target_room = target_session
    if not summary:
        summary = f"{action}: {title}"
    result: dict[str, Any] = {
        "event": action,
        "projectId": project_id,
        "summary": summary,
    }
    if target_room:
        result["targetRoom"] = target_room
    if reply_route:
        result["replyRoute"] = reply_route
    return result

def _optional_workspace_dir(arguments: dict[str, Any]) -> Path | None:
    value = str(arguments.get("workspaceDir") or "").strip()
    if not value:
        value = _default_workspace_dir()
    return Path(value).expanduser() if value else None

def _payload(arguments: dict[str, Any]) -> dict[str, Any]:
    payload = arguments.get("payload")
    if isinstance(payload, dict):
        data = dict(payload)
    elif isinstance(payload, str) and payload.strip():
        try:
            decoded = json.loads(payload)
        except json.JSONDecodeError:
            data = {}
        else:
            data = decoded if isinstance(decoded, dict) else {}
    else:
        data = {}

    aliases = {
        "projectId": ("projectId", "project_id"),
        "taskId": ("taskId", "task_id"),
        "roomId": ("roomId", "room_id"),
        "sourceRoomId": ("sourceRoomId", "source_room_id"),
        "sender": ("sender", "senderId", "sender_id", "senderUserId", "sender_user_id", "sourceUserId", "source_user_id"),
        "assignedTo": ("assignedTo", "assigned_to"),
        "dependsOn": ("dependsOn", "depends_on"),
        "replacementTaskId": ("replacementTaskId", "replacement_task_id"),
    }
    for canonical, keys in aliases.items():
        if any(data.get(key) for key in keys):
            continue
        for key in keys:
            value = arguments.get(key)
            if value is not None:
                data[canonical] = value
                break

    for key in (
        "title", "name", "source", "requester", "spec", "status", "summary", "notes", "topic", "admin",
        "invite", "replyRoute", "reply_route", "accepted", "resultStatus", "result_status", "reason",
        "cancelReason", "cancel_reason",
        "targetUser", "target_user",
    ):
        if key not in data and arguments.get(key) is not None:
            data[key] = arguments[key]

    if "deliverables" not in data:
        if arguments.get("deliverables") is not None:
            data["deliverables"] = arguments["deliverables"]
        elif arguments.get("deliverable") is not None:
            data["deliverables"] = [arguments["deliverable"]]
    if "tasks" not in data and arguments.get("tasks") is not None:
        data["tasks"] = arguments["tasks"]
    return data

def _payload_bool(value: Any, default: bool) -> bool:
    if value is None:
        return default
    if isinstance(value, bool):
        return value
    text = str(value).strip().lower()
    if text in {"true", "1", "yes", "y", "accepted"}:
        return True
    if text in {"false", "0", "no", "n", "rejected"}:
        return False
    return default

def _payload_bool_field(payload: dict[str, Any], names: tuple[str, ...], default: bool) -> bool:
    for name in names:
        if name in payload:
            return _payload_bool(payload.get(name), default)
    return default

def _positive_int(value: Any, field: str) -> int:
    try:
        parsed = int(value)
    except (TypeError, ValueError):
        raise ValueError(f"{field} must be an integer") from None
    if parsed < 1:
        raise ValueError(f"{field} must be greater than zero")
    return parsed

def _preserve_task_meta_fields(arguments: dict[str, Any], task: dict[str, Any]) -> None:
    """Keep correlation fields when taskflow writes after a remote pull.

    filesync pull and ack_task can replace local ``meta.json`` with a remote
    copy that omits ``room_id`` / ``assigned_to``.  Preserve non-empty local
    values and fall back to the project plan node when still missing.
    """
    task_id = _first_text(task.get("task_id"), task.get("taskId"))
    if not task_id:
        return
    existing = _read_json(_task_state_path(arguments, task_id))
    project_task = _project_task_for_meta(arguments, task)
    for snake, camel in (
        ("room_id", "roomId"),
        ("assigned_to", "assignedTo"),
        ("project_id", "projectId"),
    ):
        if _first_text(task.get(snake), task.get(camel)):
            continue
        preserved = _first_text(
            existing.get(snake),
            existing.get(camel),
            project_task.get(snake),
            project_task.get(camel),
        )
        if preserved:
            task[snake] = preserved

def _project_dir(arguments: dict[str, Any], project_id: str) -> Path:
    return _workspace_dir(arguments) / "shared" / "projects" / project_id

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

def _project_state_path(arguments: dict[str, Any], project_id: str) -> Path:
    return _project_dir(arguments, project_id) / "meta.json"

def _project_task_for_meta(arguments: dict[str, Any], task: dict[str, Any]) -> dict[str, Any]:
    project_id = _first_text(task.get("project_id"), task.get("projectId"))
    task_id = _first_text(task.get("task_id"), task.get("taskId"))
    if not project_id or not task_id:
        return {}
    project = _read_json(_project_state_path(arguments, project_id))
    tasks = list(project.get("tasks", []) if isinstance(project.get("tasks"), list) else [])
    loop = project.get("loop") if isinstance(project.get("loop"), dict) else {}
    if isinstance(loop.get("tasks"), list):
        tasks.extend(loop["tasks"])
    for item in tasks:
        if isinstance(item, dict) and _first_text(item.get("task_id"), item.get("taskId")) == task_id:
            return item
    return {}

def _project_timestamp() -> str:
    return time.strftime("%Y%m%d-%H%M%S")

def _pull_task(arguments: dict[str, Any], task_id: str) -> bool:
    from tools.filesync import filesync

    existing = _read_json(_task_state_path(arguments, task_id))
    sync_args = dict(arguments)
    sync_args.update({
        "action": "pull",
        "path": f"shared/tasks/{task_id}",
    })
    result = filesync(sync_args)
    if not result.get("ok"):
        return False
    if existing:
        task = _read_json(_task_state_path(arguments, task_id))
        if task:
            for snake, camel in (
                ("room_id", "roomId"),
                ("assigned_to", "assignedTo"),
                ("project_id", "projectId"),
            ):
                if _first_text(task.get(snake), task.get(camel)):
                    continue
                preserved = _first_text(existing.get(snake), existing.get(camel))
                if preserved:
                    task[snake] = preserved
            _write_task(arguments, task)
    return True

def _read_json(path: Path, default: dict[str, Any] | None = None) -> dict[str, Any]:
    if not path.exists():
        return dict(default or {})
    return json.loads(path.read_text(encoding="utf-8"))

def _ready_loop_nodes(project: dict[str, Any]) -> list[dict[str, Any]]:
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

def _remote_root(value: str) -> str:
    text = (value or "").strip()
    if not text:
        raise ValueError("storage sharedPrefix is required")
    return text.rstrip("/") + "/"

def _reply_route_from_requester(requester: Any) -> dict[str, str]:
    text = str(requester or "").strip()
    if text.startswith("matrix:"):
        try:
            target_kind, target_id = _matrix_target(text)
        except ValueError:
            return {}
        if target_kind != "room":
            return {}
        return {
            "channel": "matrix",
            "target_session": target_id,
        }
    if not text.startswith("dingtalk:"):
        return {}
    parts = text.split(":", 2)
    if len(parts) != 3 or not parts[1] or not parts[2]:
        return {}
    return {
        "channel": "dingtalk",
        "target_user": parts[1],
        "target_session": parts[2],
    }

def _require_task_mutable(arguments: dict[str, Any], task: dict[str, Any], task_id: str, action: str) -> None:
    terminal_status = _terminal_task_status(arguments, task, task_id)
    if terminal_status:
        raise ValueError(f"{action} cannot update terminal task: {terminal_status}")

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

def _role(arguments: dict[str, Any]) -> str:
    role = str(arguments.get("role") or "").strip()
    if not role:
        return _runtime_role()
    return _normalize_role(role)

def _runtime_role() -> str:
    role = os.getenv("AGENTTEAMS_AGENT_ROLE", "").strip() or os.getenv("AGENTTEAMS_WORKER_ROLE", "").strip()
    if not role:
        role = str(_section(_load_runtime_config(), "member").get("role") or "").strip()
    return _normalize_role(role)

def _runtime_team_room_id() -> str:
    team = _section(_load_runtime_config(), "team")
    return str(team.get("teamRoomId") or team.get("team_room_id") or "").strip()

def _safe_id(value: Any, field: str) -> str:
    text = str(value or "").strip()
    if not text or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]*", text):
        raise ValueError(f"{field} must be a safe id")
    return text

def _safe_loop_decision(value: Any) -> str:
    decision = str(value or "").strip()
    allowed = {"continue", "replan", "ask_user", "stop_success", "stop_blocked"}
    if decision not in allowed:
        raise ValueError(f"decision must be one of: {', '.join(sorted(allowed))}")
    return decision

def _safe_loop_status(value: Any) -> str:
    status = str(value or "running").strip()
    allowed = {"running", "waiting_user", "completed", "blocked"}
    if status not in allowed:
        raise ValueError(f"status must be one of: {', '.join(sorted(allowed))}")
    return status

def _section(data: dict[str, Any], name: str) -> dict[str, Any]:
    return _runtime_section(data, name)

def _slugify(value: Any, fallback: str) -> str:
    text = re.sub(r"[^A-Za-z0-9]+", "-", str(value or "").strip().lower()).strip("-")
    return text or fallback

def _source_room_id_from_payload(payload: dict[str, Any], reply_route: dict[str, str] | None = None) -> str:
    source_room_id = str(payload.get("sourceRoomId") or payload.get("source_room_id") or "").strip()
    if source_room_id:
        return source_room_id

    route = reply_route if isinstance(reply_route, dict) else {}
    channel = str(route.get("channel") or payload.get("source") or "").strip().lower()
    if channel:
        return str(route.get("target_session") or "").strip()

    requester_route = _reply_route_from_requester(payload.get("requester"))
    channel = str(requester_route.get("channel") or "").strip().lower()
    if channel:
        return str(requester_route.get("target_session") or "").strip()
    return ""

def _sync_task(arguments: dict[str, Any], task_id: str, exclude: list[str] | None = None) -> bool:
    from tools.filesync import filesync

    sync_args = dict(arguments)
    sync_args.update({
        "action": "push",
        "path": f"shared/tasks/{task_id}",
    })
    if exclude is not None:
        sync_args["exclude"] = exclude
    result = filesync(sync_args)
    return bool(result.get("ok"))

def _task_dir(arguments: dict[str, Any], task_id: str) -> Path:
    return _workspace_dir(arguments) / "shared" / "tasks" / task_id

def _task_result_from_meta(task: dict[str, Any]) -> tuple[dict[str, Any], list[str]]:
    deliverables = task.get("deliverables")
    if not isinstance(deliverables, list):
        deliverables = []
    result = {
        "status": str(task.get("result_status") or "").strip(),
        "summary": str(task.get("summary") or "").strip(),
        "deliverables": [str(item) for item in deliverables],
    }
    errors: list[str] = []
    status = result["status"]
    if not status:
        errors.append("missing result status")
    elif status not in ALLOWED_TASK_RESULT_STATUSES:
        errors.append(f"invalid result status: {status}")
    if not result["summary"]:
        errors.append("missing result summary")
    try:
        _validate_task_deliverables(str(task.get("task_id") or ""), result["deliverables"])
    except ValueError as exc:
        errors.append(str(exc))
    return result, errors

def _task_state_path(arguments: dict[str, Any], task_id: str) -> Path:
    return _task_dir(arguments, task_id) / "meta.json"

def _terminal_task_status(arguments: dict[str, Any], task: dict[str, Any], task_id: str) -> str:
    project_id = str(task.get("project_id") or "")
    project = _read_json(_project_state_path(arguments, project_id)) if project_id else {}
    project_tasks = project.get("tasks", []) if isinstance(project.get("tasks"), list) else []
    loop = project.get("loop") if isinstance(project.get("loop"), dict) else {}
    loop_tasks = loop.get("tasks", []) if isinstance(loop.get("tasks"), list) else []
    for node in project_tasks + loop_tasks:
        if isinstance(node, dict) and node.get("task_id") == task_id:
            node_status = str(node.get("status") or "")
            if node_status in TERMINAL_TASK_STATUSES:
                return node_status
    task_status = str(task.get("status") or "")
    return task_status if task_status in TERMINAL_TASK_STATUSES else ""

def _unique_project_id(arguments: dict[str, Any], base_id: str) -> str:
    project_id = _safe_id(base_id, "projectId")
    if not _project_state_path(arguments, project_id).exists():
        return project_id
    for index in range(1, 1000):
        candidate = _safe_id(f"{base_id}-{index:02d}", "projectId")
        if not _project_state_path(arguments, candidate).exists():
            return candidate
    raise ValueError(f"cannot allocate unique project id for: {base_id}")

def _update_project_task(arguments: dict[str, Any], project_id: str, task_id: str, **updates: Any) -> None:
    path = _project_state_path(arguments, project_id)
    project = _read_json(path)
    if not project:
        return
    changed = False
    for task in project.get("tasks", []):
        if task.get("task_id") == task_id:
            task.update(updates)
            changed = True
            break
    loop = project.get("loop") if isinstance(project.get("loop"), dict) else {}
    loop_tasks = loop.get("tasks", []) if isinstance(loop.get("tasks"), list) else []
    for task in loop_tasks:
        if task.get("task_id") == task_id:
            task.update(updates)
            changed = True
            break
    if changed:
        _write_json(path, project)
        _write_project_plan(_project_dir(arguments, project_id), project)

def _utc_timestamp() -> str:
    return (
        datetime.datetime.now(datetime.timezone.utc)
        .replace(microsecond=0)
        .isoformat()
        .replace("+00:00", "Z")
    )

def _validate_assignment_room(project: dict[str, Any], room_id: str) -> None:
    channel = _external_requester_channel(project)
    if not channel:
        return
    team_room_id = _runtime_team_room_id()
    if not team_room_id:
        return
    if _canonical_room_id(room_id) == _canonical_room_id(team_room_id):
        raise ValueError(
            f"{channel} requester tasks require a dedicated task room; "
            "call roomflow create_task_room and pass its roomId"
        )

def _validate_task_deliverables(task_id: str, deliverables: list[Any]) -> list[str]:
    expected_prefix = f"shared/tasks/{task_id}"
    normalized_deliverables: list[str] = []
    for item in deliverables:
        source = str(item or "").strip()
        if not source:
            continue
        try:
            normalized, _is_directory = _normalize_workspace_artifact_path(source)
        except ValueError as exc:
            raise ValueError(f"deliverables must be workspace-relative paths under {expected_prefix}/") from exc
        if not _path_is_under(normalized.rstrip("/"), expected_prefix):
            raise ValueError(f"deliverables must stay under {expected_prefix}/")
        normalized_deliverables.append(normalized)
    return normalized_deliverables


def _verify_task_artifacts(
    arguments: dict[str, Any],
    task_id: str,
    *,
    result: dict[str, Any],
) -> dict[str, Any]:
    """Run filesystem existence checks for task deliverables."""
    from agentteams_protocol.task import TaskResult, verify_task_artifacts

    task_result = TaskResult(
        status=str(result.get("status") or ""),
        summary=str(result.get("summary") or ""),
        deliverables=[str(item) for item in (result.get("deliverables") or [])],
    )
    workspace = _workspace_dir(arguments)
    report = verify_task_artifacts(workspace, task_id=task_id, result=task_result)
    return {
        "verified": report.verified,
        "claims": [
            {
                "path": claim.path,
                "check": claim.check,
                "passed": claim.passed,
                "required": claim.required,
                "detail": claim.detail,
            }
            for claim in report.claims
        ],
    }


def _failed_required_claims(verification: dict[str, Any]) -> list[dict[str, Any]]:
    return [
        claim
        for claim in verification.get("claims", [])
        if claim.get("required") and not claim.get("passed")
    ]


def _validate_task_graph(tasks: list[dict[str, Any]]) -> None:
    _protocol_validate_task_graph(tasks)

def _validate_task_redelegation(arguments: dict[str, Any], project: dict[str, Any], task_id: str, room_id: str) -> None:
    if not _external_requester_channel(project):
        return
    existing = _read_json(_task_state_path(arguments, task_id))
    existing_room_id = str(existing.get("room_id") or "").strip()
    if not existing_room_id:
        return
    if _canonical_room_id(existing_room_id) == _canonical_room_id(room_id):
        return
    raise ValueError(
        f"task {task_id} is already delegated to assignment room {existing_room_id}; "
        f"do not delegate it again to {room_id}"
    )

def _workspace_dir(arguments: dict[str, Any]) -> Path:
    value = str(arguments.get("workspaceDir") or "").strip()
    if not value:
        value = _default_workspace_dir()
    if not value:
        raise ValueError("workspaceDir is required")
    return Path(value).expanduser()

def _write_json(path: Path, data: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(data, ensure_ascii=False, indent=2, sort_keys=True) + "\n", encoding="utf-8")

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
    (project_dir / "plan.md").write_text("\n".join(lines) + "\n", encoding="utf-8")

def _write_task(arguments: dict[str, Any], task: dict[str, Any]) -> None:
    _ensure_console_task_meta(arguments, task)
    _write_json(_task_state_path(arguments, task["task_id"]), task)

