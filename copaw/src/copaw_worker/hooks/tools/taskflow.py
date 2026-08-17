"""CoPaw-native taskflow tool for AgentTeams task state."""

from __future__ import annotations

from dataclasses import asdict
import json
import logging
import os
from pathlib import Path
import re
from typing import Any

import yaml

from agentscope.message import TextBlock
from agentscope.tool import ToolResponse

from copaw_worker.hooks.tools.filesync import create_sync
from copaw_worker.task import (
    DagTask,
    FileSystemTaskStore,
    RESULT_STATUSES,
    TaskMeta,
    TaskResult,
    TaskflowError,
    ack_task,
    canonical_worker_id,
    check_task,
    commit_task_assignment,
    is_effective_result,
    prepare_task,
    submit_task,
    validate_delegate_task,
    validate_task_result,
)

logger = logging.getLogger(__name__)

_MATRIX_USER_ID_RE = re.compile(
    r"@[a-zA-Z0-9._=+/\-]+:[a-zA-Z0-9.\-]+(?::\d+)?",
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


def _working_dir() -> Path:
    configured = os.getenv("QWENPAW_WORKING_DIR") or os.getenv("COPAW_WORKING_DIR")
    if configured:
        return Path(configured).expanduser().resolve()
    # qwenpaw is the successor of copaw (renamed package).
    # In the qwenpaw 2.0 venv the copaw package does not exist.
    try:
        from qwenpaw.constant import WORKING_DIR  # type: ignore[import-untyped]
    except ImportError:
        from copaw.constant import WORKING_DIR  # type: ignore[import-untyped]
    return Path(WORKING_DIR).expanduser().resolve()


def _workspace_dir() -> Path:
    return _working_dir() / "workspaces" / "default"


def _store() -> FileSystemTaskStore:
    return FileSystemTaskStore(_workspace_dir())


def _runtime_root() -> Path:
    return _working_dir().parent


def _strip_yaml_string(value: str) -> str:
    text = value.strip()
    if not text or text in {"null", "~"}:
        return ""
    if "#" in text:
        text = text.split("#", 1)[0].strip()
    if len(text) >= 2 and text[0] == text[-1] and text[0] in {"'", '"'}:
        return text[1:-1]
    return text


def _runtime_config_field(section: str, key: str) -> str:
    path = _runtime_root() / "runtime" / "runtime.yaml"
    if not path.exists():
        return ""

    in_section = False
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except OSError:
        return ""
    for raw_line in lines:
        if not raw_line.strip() or raw_line.lstrip().startswith("#"):
            continue
        if not raw_line.startswith((" ", "\t")):
            in_section = raw_line.strip() == f"{section}:"
            continue
        if not in_section:
            continue
        stripped = raw_line.strip()
        if ":" not in stripped:
            continue
        field, value = stripped.split(":", 1)
        if field.strip() == key:
            return _strip_yaml_string(value)
    return ""


def _normalize_room_id(room_id: str) -> str:
    text = (room_id or "").strip()
    if text.startswith("room:"):
        text = text[len("room:") :].strip()
    return text


def _room_target(room_id: str) -> str:
    text = (room_id or "").strip()
    if text.startswith("room:"):
        return text
    return f"room:{text}"


def _require_team_leader_assignment_room(room_id: str) -> None:
    role = _runtime_config_field("member", "role")
    team_room_id = _runtime_config_field("team", "teamRoomId")
    if role != "team_leader" or not team_room_id:
        return

    if _normalize_room_id(room_id) != _normalize_room_id(team_room_id):
        raise TaskflowError(
            "team leader task assignments must use the Team Room "
            f"{_room_target(team_room_id)}, not {room_id}",
        )


def _current_actor() -> str | None:
    configured = (
        os.getenv("AGENTTEAMS_MATRIX_USER_ID")
        or os.getenv("COPAW_MATRIX_USER_ID")
    )
    if configured:
        return configured.strip()

    try:
        # Read agent.json directly (works in both copaw and qwenpaw venvs
        # without importing either framework's config module).
        working_dir = _resolve_working_dir()
        if working_dir:
            agent_json = working_dir / "workspaces" / "default" / "agent.json"
            with open(agent_json, "r", encoding="utf-8") as f:
                data = json.load(f)
            matrix_cfg = (data.get("channels") or {}).get("matrix") or {}
            user_id = matrix_cfg.get("user_id") or matrix_cfg.get("userId")
            if user_id:
                return str(user_id).strip()
    except Exception:
        pass

    return None


def _resolve_working_dir() -> Path | None:
    """Resolve the QwenPaw/CoPaw working directory.

    Delegates to _working_dir() which handles env vars and constant
    fallback consistently with projectflow, filesync, and message tools.
    """
    return _working_dir()


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


# ------------------------------------------------------------------
# Task assignment notification (atomic with state recording)
# ------------------------------------------------------------------


def _resolve_worker_matrix_id_from_agents(worker_name: str) -> str | None:
    """Resolve a canonical worker name via the legacy AGENTS.md roster."""
    agents_path = _runtime_root() / "AGENTS.md"
    try:
        lines = agents_path.read_text(encoding="utf-8").splitlines()
    except OSError:
        return None

    in_workers = False
    for line in lines:
        if line.strip() == "- **Team Workers**:":
            in_workers = True
            continue
        if not in_workers:
            continue
        if not line.startswith("  - "):
            break
        match = _MATRIX_USER_ID_RE.search(line)
        if not match:
            continue
        matrix_id = match.group(0)
        localpart = matrix_id.split(":", 1)[0].removeprefix("@")
        if localpart == worker_name:
            return matrix_id
    return None


def _resolve_worker_matrix_id(worker_name: str) -> str | None:
    """Resolve a Team Worker Matrix ID from runtime.yaml, with legacy fallback."""
    canonical_name = canonical_worker_id(worker_name)
    runtime_config_path = _runtime_root() / "runtime" / "runtime.yaml"
    try:
        data = yaml.safe_load(runtime_config_path.read_text(encoding="utf-8")) or {}
    except (OSError, yaml.YAMLError) as exc:
        logger.debug("Unable to read Team roster from runtime.yaml: %s", exc)
        data = {}

    team = data.get("team") if isinstance(data, dict) else None
    if isinstance(team, dict) and "members" in team:
        members = team.get("members")
        if not isinstance(members, list):
            logger.warning("runtime.yaml team.members must be a list")
            return None
        for member in members:
            if not isinstance(member, dict):
                continue
            member_names = (member.get("runtimeName"), member.get("name"))
            if not any(
                canonical_worker_id(str(name)) == canonical_name
                for name in member_names
                if name
            ):
                continue
            matrix_id = str(member.get("matrixUserId") or "").strip()
            return matrix_id or None
        return None

    return _resolve_worker_matrix_id_from_agents(canonical_name)


async def _check_room_membership(room_id: str, user_id: str) -> bool:
    """Check if a user is a joined member of a Matrix room."""
    from nio import AsyncClient

    from copaw_worker.hooks.tools.message import _matrix_config_for_agent

    homeserver, access_token, client_user_id = _matrix_config_for_agent("default")
    client = AsyncClient(homeserver, user=client_user_id)
    client.access_token = access_token
    try:
        response = await client.joined_members(_normalize_room_id(room_id))
        members = getattr(response, "members", None)
        if members is None:
            return False
        return any(m.user_id == user_id for m in members)
    finally:
        await client.close()


async def _notify_task_assignment(
    *,
    task: DagTask,
    room_id: str,
    spec: str,
    txn_id: str | None = None,
) -> dict[str, Any]:
    """Send a Matrix notification for a task assignment.

    Validates room membership, sends a message with m.mentions,
    and returns the event_id on success.
    """
    assignee_matrix_id = _resolve_worker_matrix_id(
        canonical_worker_id(task.assigned_to),
    )
    if not assignee_matrix_id:
        return {
            "sent": False,
            "error": (
                f"cannot resolve Matrix ID for worker "
                f"'{canonical_worker_id(task.assigned_to)}' in AGENTS.md"
            ),
        }

    try:
        membership_ok = await _check_room_membership(room_id, assignee_matrix_id)
    except Exception as exc:
        return {"sent": False, "error": f"room membership check failed: {exc}"}
    if not membership_ok:
        return {
            "sent": False,
            "error": (
                f"worker {assignee_matrix_id} is not a joined member "
                f"of room {room_id}"
            ),
        }

    try:
        from copaw_worker.hooks.tools.message import (
            _send_matrix_room_message,
            build_matrix_text_content,
        )
    except ImportError:
        return {
            "sent": False,
            "error": "message tool dependencies not available",
        }

    spec_preview = spec[:500] + ("..." if len(spec) > 500 else "")
    notification_text = (
        f"{assignee_matrix_id} "
        f"You are assigned task **{task.task_id}**: {task.title}\n\n"
        f"{spec_preview}"
    )
    mentions = [assignee_matrix_id]
    content = build_matrix_text_content(notification_text, mentions)

    try:
        event_id = await _send_matrix_room_message(
            room_id=room_id,
            content=content,
            account_id="default",
            txn_id=txn_id,
        )
    except Exception as exc:
        return {"sent": False, "error": f"Matrix send failed: {exc}"}

    return {
        "sent": True,
        "eventId": event_id,
        "roomId": _normalize_room_id(room_id),
        "assignee": assignee_matrix_id,
    }


async def taskflow(
    action: str,
    payload: dict[str, Any] | str | None = None,
    dryRun: bool = False,
) -> ToolResponse:
    """Manage AgentTeams task state with action-specific payload fields."""
    payload_data: dict[str, Any] = {}
    try:
        store = _store()
        payload_data = _coerce_payload(payload)

        if action == "delegate_task":
            project_id = _required_str(payload_data, "projectId")
            task_id = _required_str(payload_data, "taskId")
            room_id = _required_str(payload_data, "roomId")
            spec = _required_str(payload_data, "spec")
            _require_team_leader_assignment_room(room_id)
            if dryRun:
                return _ok(
                    dryRun=True,
                    action=action,
                    projectId=project_id,
                    taskId=task_id,
                )
            task_path = f"shared/tasks/{task_id}/"
            sync = create_sync()

            # 0. Detect retry state. After a partial failure the task may
            #    already be prepared (files written, no event recorded) or
            #    fully assigned (event recorded). Reading meta here makes
            #    the whole flow idempotent and retry-safe.
            try:
                existing_meta = store.read_task_meta(task_id)
            except TaskflowError:
                existing_meta = None

            if existing_meta is not None and existing_meta.status == "assigned":
                # Already fully assigned (event_id recorded). Nothing to do.
                sync.push_shared_path(task_path)
                return _ok(
                    action=action,
                    task=asdict(existing_meta),
                    synced=True,
                    notification={
                        "sent": True,
                        "eventId": existing_meta.event_id,
                        "roomId": _normalize_room_id(room_id),
                        "assignee": canonical_worker_id(existing_meta.assigned_to),
                        "reused": True,
                    },
                )

            if existing_meta is None:
                # 1. Fresh delegation: validate preconditions (CAS: task
                #    must be pending), then claim the node — write
                #    meta.json (status=prepared) + spec.md and mark the
                #    plan node delegated so ready-node queries stop
                #    returning it. Files become visible to Workers BEFORE
                #    the Matrix notification arrives.
                task = validate_delegate_task(
                    store,
                    project_id=project_id,
                    task_id=task_id,
                    spec=spec,
                )
                meta = prepare_task(
                    store,
                    project_id=project_id,
                    task_id=task_id,
                    spec=spec,
                    room_id=room_id,
                )
            else:
                # 2. Retry of a prepared task after notification failed.
                #    The plan node is already delegated; rebuild the DAG
                #    task from the recorded meta and skip validate/prepare.
                task = DagTask(
                    task_id=existing_meta.task_id,
                    title=existing_meta.task_title,
                    assigned_to=existing_meta.assigned_to,
                    depends_on=existing_meta.depends_on,
                    status="delegated",
                )
                meta = existing_meta

            # 3. Publish task files to shared storage first so a Worker
            #    that receives the notification can read spec.md/metadata.
            sync.push_shared_path(task_path)

            # 4. Retry-safe notification. Use a stable transaction ID so
            #    re-sending after a partial failure is idempotent.
            notification = await _notify_task_assignment(
                task=task,
                room_id=room_id,
                spec=spec,
                txn_id=f"delegate-{task_id}",
            )
            if not notification.get("sent"):
                return _error(
                    notification.get("error", "notification failed"),
                    action=action,
                    projectId=project_id,
                    taskId=task_id,
                    notification=notification,
                    task=asdict(meta),
                    retryable=True,
                )
            # 5. Record event_id and mark assigned only after the send
            #    succeeded, so a retry cannot produce a duplicate
            #    notification.
            meta = commit_task_assignment(
                store,
                project_id=project_id,
                task_id=task_id,
                event_id=notification.get("eventId"),
            )

            # 6. Re-push so the assigned status/event_id is visible
            #    remotely.
            sync.push_shared_path(task_path)
            return _ok(
                action=action,
                task=asdict(meta),
                synced=True,
                notification=notification,
            )

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
