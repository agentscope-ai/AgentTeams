"""Finite collaboration plans belong to main Tasks; Project is optional membership."""

from __future__ import annotations

import os
from typing import Any

from mcp.common import refs
from mcp.common.config import _runtime_team_name
from mcp.common.io import _read_json, _utc_timestamp
from mcp.common.paths import _task_dir, _task_state_path, _project_state_path
from mcp.common.payload import _safe_id
from mcp.project.state import _normalize_tasks, _validate_task_graph, _ready_nodes


def _load_main_plan(arguments: dict[str, Any], main_id: str) -> dict[str, Any]:
    main_id = _safe_id(main_id, "mainTaskId")
    from mcp.artifact.sync import _load_authoritative_task

    main = _load_authoritative_task(arguments, main_id)
    if main.get("task_kind") != "main" or main.get("task_id") != main_id:
        raise ValueError("mainTaskId must identify an existing coordinating main task")
    return main


def _sync_main_plan(
    arguments: dict[str, Any], main_id: str, *, task_update=None, completing=False
) -> bool:
    """Merge only this node's change, using the existing conditional Task commit.

    Execution Workers never write the parent plan. Concurrent Leader acceptance
    sessions cannot overwrite each other's node decisions or reopen cancellation.
    """
    from mcp.artifact.sync import (
        _remote_meta_consistent_read,
        _commit_task_meta,
        _push_task_files,
    )

    local = _read_json(_task_state_path(arguments, main_id))
    if not os.getenv("AGENTTEAMS_CONTROLLER_URL"):
        return refs.resolve("_sync_task")(arguments, main_id)
    key = f"shared/tasks/{main_id}/meta.json"
    for _ in range(3):
        remote, etag, error = _remote_meta_consistent_read(arguments, key)
        if error or not remote or etag is None:
            return False
        if remote.get("task_kind") != "main" or remote.get("status") in {
            "paused",
            "cancelled",
            "blocked",
        }:
            return False
        _read_status = remote.get("status")
        if completing:
            if remote.get("status") == "completed":
                return remote.get("summary") == local.get("summary")
            if not remote.get("tasks") or any(
                n.get("status") != "completed" for n in remote["tasks"]
            ):
                return False
            for field in (
                "status",
                "summary",
                "result_path",
                "result_status",
                "deliverables",
                "completed_at",
            ):
                remote[field] = local[field]
        elif task_update:
            task_id, updates = task_update
            node = next(
                (n for n in remote.get("tasks", []) if n.get("task_id") == task_id),
                None,
            )
            if node is None or (
                node.get("status") in {"completed", "cancelled", "blocked", "revision"}
                and node.get("status") != updates.get("status")
            ):
                return False
            node.update(updates)
        else:
            # Delivery receipts are advisory; never copy a stale whole plan.
            for field in ("requesterCompletionEventId", "requester_report"):
                if field in local:
                    remote[field] = local[field]
        refs.resolve("_write_task")(arguments, remote)
        if not _push_task_files(
            arguments,
            main_id,
            exclude=["spec.md", "base/"],
            result_paths=None,
            include_meta=False,
        ):
            return False
        # Keep the shared Task lifecycle: in_progress -> submitted -> completed.
        # The final-report tool performs both durable steps, without another
        # model turn or a special main-task transition lane.
        submitting = completing and _read_status == "in_progress"
        if submitting:
            remote["status"] = "submitted"
        if completing and _read_status != remote["status"]:
            from .transitions import _append_transition_history, _transition_actor

            _append_transition_history(
                remote,
                _read_status,
                remote["status"],
                "complete_task",
                _transition_actor(arguments),
            )
        commit = _commit_task_meta(main_id, remote, etag, _runtime_team_name())
        if commit.get("ok"):
            if submitting:
                continue
            return True
        if not commit.get("conflict"):
            return False
    return False


def _advance_main_task(
    arguments: dict[str, Any], main: dict[str, Any]
) -> dict[str, Any]:
    if main.get("status") != "in_progress":
        raise ValueError("dispatch requires an active main task")
    assignments = []
    for node in _ready_nodes(main):
        if node.get("status") != "planned":
            continue
        if node.get("task_kind") not in {None, "execution"}:
            raise ValueError(
                "a coordinating main task cannot be dispatched as Worker execution"
            )
        result = refs.resolve("_taskflow")(
            {
                **arguments,
                "action": "delegate_task",
                "payload": {
                    "mainTaskId": main["task_id"],
                    "projectId": main.get("project_id", ""),
                    "taskId": node["task_id"],
                    "assignedTo": node["assigned_to"],
                    "roomId": main["room_id"],
                    "spec": node["spec"],
                    "decisionClass": "r3",
                },
            }
        )
        assignments.append(result)
        if not result.get("ok"):
            return {
                "ok": False,
                "assignments": assignments,
                "retryable": True,
                "error": result.get("error"),
            }
    return {"ok": True, "assignments": assignments}


def _start_task(arguments: dict[str, Any], payload: dict[str, Any]) -> dict[str, Any]:
    main_id = _safe_id(payload.get("taskId"), "taskId")
    project_id = str(payload.get("projectId") or "")
    if project_id:
        project_id = _safe_id(project_id, "projectId")
        if not refs.resolve("_pull_project")(arguments, project_id):
            raise ValueError("cannot read optional project membership (retry)")
        project = _read_json(_project_state_path(arguments, project_id))
        if not project or project.get("status") not in {"active", "running"}:
            raise ValueError(
                "projectId must identify an existing active Project; start_task never creates Projects"
            )
    spec = payload.get("spec")
    if not isinstance(spec, str) or not spec.strip():
        raise ValueError("start_task requires the original request in spec")
    raw = payload.get("tasks")
    if (
        not isinstance(raw, list)
        or not raw
        or any(not isinstance(n, dict) for n in raw)
    ):
        raise ValueError("start_task requires execution subtasks in tasks")
    if any(n.get("task_kind", n.get("taskKind", "execution")) != "execution" for n in raw):
        raise ValueError("a coordinating main task cannot be an execution subtask")
    nodes = _normalize_tasks(raw, {})
    # The first version supports one coordinating main with execution leaves.
    # Parentage and dependency are separate, not inferred from the DAG edges.
    for node in nodes:
        if node["task_id"] == main_id or node.get("parent_task_id", main_id) != main_id:
            raise ValueError("every execution task must belong to this main task")
        if (
            not node.get("assigned_to")
            or not node.get("spec")
            or node["status"] != "planned"
        ):
            raise ValueError(
                "every execution task requires spec, assignedTo and initial planned status"
            )
        node.pop("parent_task_id", None)
    _validate_task_graph(nodes)
    for node in nodes:
        node["parent_task_id"] = main_id
        node["task_kind"] = "execution"
        node["spec"] = (
            "# Original request\n"
            + spec.strip()
            + "\n\n# Your assignment\n"
            + node["spec"]
        )
    if os.getenv("TEAMHARNESS_COORDINATION_URL"):
        from mcp.transport.coordination import native_team_members

        members = native_team_members(
            str(payload.get("teamId") or _runtime_team_name())
        )
        eligible = {
            v
            for m in members
            if m.get("nativeCoordinationSupported") is True
            for v in (m.get("name"), m.get("matrixUserID"))
            if v
        }
        if any(n["assigned_to"] not in eligible for n in nodes):
            raise ValueError(
                "native coordination is unavailable for one or more assignees"
            )
    from mcp.artifact.sync import _load_authoritative_task_optional

    existing = _load_authoritative_task_optional(arguments, main_id)
    if existing:

        def shape(items):
            return [
                {
                    k: n.get(k)
                    for k in (
                        "task_id",
                        "title",
                        "assigned_to",
                        "depends_on",
                        "parent_task_id",
                        "spec",
                    )
                }
                for n in items
            ]

        if (
            existing.get("task_kind") != "main"
            or shape(existing.get("tasks", [])) != shape(nodes)
            or existing.get("project_id", "") != project_id
            or existing.get("requester", "") != payload.get("requester", "")
            or existing.get("source_room_id", "") != payload.get("sourceRoomId", "")
            or existing.get("reply_route", {}) != payload.get("replyRoute", {})
        ):
            raise ValueError(
                "start_task retry must preserve the original plan, membership and requester"
            )
        main = existing
    else:
        for node in nodes:
            if (
                _load_authoritative_task_optional(arguments, node["task_id"])
                is not None
            ):
                raise ValueError("subtask ID already exists: " + node["task_id"])
        from mcp.room.matrix import _assignee_matrix_id

        invite = list(
            dict.fromkeys(_assignee_matrix_id(n["assigned_to"]) for n in nodes)
        )
        if "" in invite:
            raise ValueError("assignedTo must resolve to a full Matrix identity")
        # Room identity is the main Task, even when an optional Project exists.
        room = refs.resolve("_roomflow")(
            {
                **arguments,
                "action": "create_task_room",
                "payload": {
                    **payload,
                    "projectId": "",
                    "taskId": main_id,
                    "invite": invite,
                },
            }
        )
        if not room.get("ok"):
            return room
        main = {
            "task_id": main_id,
            "task_kind": "main",
            "title": payload.get("title") or main_id,
            "team_id": payload.get("teamId") or _runtime_team_name(),
            "status": "in_progress",
            "assigned_to": refs.resolve("_runtime_identity")(),
            "project_id": project_id,
            "plan_type": "dag",
            "dispatch_mode": "automatic",
            "room_id": room["roomId"],
            "source_room_id": payload.get("sourceRoomId", ""),
            "requester": payload.get("requester", ""),
            "reply_route": payload.get("replyRoute", {}),
            "requester_reply_route": payload.get("requesterReplyRoute", {}),
            "created_at": _utc_timestamp(),
            "tasks": nodes,
            "spec_path": f"shared/tasks/{main_id}/spec.md",
        }
        directory = _task_dir(arguments, main_id)
        directory.mkdir(parents=True, exist_ok=True)
        (directory / "spec.md").write_text(spec.strip() + "\n", encoding="utf-8")
        refs.resolve("_write_task")(arguments, main)
        if not refs.resolve("_sync_task")(arguments, main_id):
            return {
                "ok": False,
                "retryable": True,
                "error": "main task could not be synchronized; no assignments sent",
            }
    if project_id:
        # Only the main task belongs in the optional Project's aggregation.
        if not refs.resolve("_pull_project")(arguments, project_id):
            raise ValueError("cannot refresh optional project membership (retry)")
        project = _read_json(_project_state_path(arguments, project_id))
        if not any(n.get("task_id") == main_id for n in project.get("tasks", [])):
            project.setdefault("tasks", []).append(
                {
                    "task_id": main_id,
                    "title": main["title"],
                    "status": main["status"],
                    "depends_on": [],
                }
            )
            refs.resolve("_write_json")(
                _project_state_path(arguments, project_id), project
            )
            if not refs.resolve("_sync_project")(arguments, project_id):
                raise ValueError("optional project membership sync failed (retry)")
    dispatch = _advance_main_task(arguments, main)
    saved = _read_json(_task_state_path(arguments, main_id))
    receipt = {
        **saved,
        "tasks": [{k: v for k, v in n.items() if k != "spec"} for n in saved["tasks"]],
    }
    return {
        "ok": dispatch["ok"],
        "tool": "taskflow",
        "action": "start_task",
        "task": receipt,
        "roomId": main["room_id"],
        "reused": bool(existing),
        "dispatch": dispatch,
    }


def _complete_task(
    arguments: dict[str, Any], payload: dict[str, Any]
) -> dict[str, Any]:
    main_id = _safe_id(payload.get("taskId") or payload.get("mainTaskId"), "taskId")
    main = _load_main_plan(arguments, main_id)
    report = payload.get("report")
    if not isinstance(report, str) or not report.strip():
        raise ValueError("complete_task requires the verified final report")
    if main["status"] not in {"in_progress", "submitted", "completed"}:
        raise ValueError("main task must be active and unpaused before completion")
    from mcp.artifact.sync import _load_authoritative_task
    from mcp.project.flow import _completion_artifact_registered

    if not main.get("tasks") or any(
        n.get("status") != "completed" for n in main["tasks"]
    ):
        raise ValueError(
            "complete_task requires every execution subtask to be accepted"
        )
    for node in main["tasks"]:
        child = _load_authoritative_task(arguments, node["task_id"])
        if (
            child.get("main_task_id") != main_id
            or child.get("status") != "completed"
            or not _completion_artifact_registered(arguments, node["task_id"])
        ):
            raise ValueError(
                "complete_task requires accepted, artifact-registered subtasks of this main task"
            )
    if main["status"] == "completed" and main.get("summary") != report.strip():
        raise ValueError("completed task report is immutable")
    report_path = _task_dir(arguments, main_id) / "result.md"
    report_path.write_text(report.strip() + "\n", encoding="utf-8")
    main.update(
        status="completed",
        summary=report.strip(),
        result_status="SUCCESS",
        result_path=f"shared/tasks/{main_id}/result.md",
        deliverables=[f"shared/tasks/{main_id}/result.md"],
        completed_at=main.get("completed_at") or _utc_timestamp(),
    )
    refs.resolve("_write_task")(arguments, main)
    if not _sync_main_plan(arguments, main_id, completing=True):
        return {
            "ok": False,
            "retryable": True,
            "error": "main task completion was not committed; notification withheld",
        }
    from mcp.room.notify import _send_requester_completion

    notice = _send_requester_completion(arguments, main, main_id)
    refs.resolve("_write_task")(arguments, main)
    synced = _sync_main_plan(arguments, main_id)
    if main.get("project_id"):
        refs.resolve("_update_project_task")(
            arguments, main["project_id"], main_id, status="completed"
        )
    return {
        "ok": bool(synced and (notice.get("sent") or notice.get("skipped"))),
        "tool": "taskflow",
        "action": "complete_task",
        "task": main,
        "requesterNotification": notice,
        "synced": synced,
    }


def _main_task_action(
    arguments: dict[str, Any], payload: dict[str, Any]
) -> dict[str, Any]:
    if refs.resolve("_role")(arguments) != "leader":
        raise ValueError("main task orchestration requires leader role")
    action = arguments["action"]
    if action == "start_task":
        return _start_task(arguments, payload)
    if action == "complete_task":
        return _complete_task(arguments, payload)
    main_id = _safe_id(payload.get("mainTaskId"), "mainTaskId")
    main = _load_main_plan(arguments, main_id)
    if action == "advance_task":
        return _advance_main_task(arguments, main)
    from mcp.project.flow import _accept_task_result

    result = _accept_task_result(arguments, payload, main_task_id=main_id)
    if "project" in result:
        result["mainTask"] = result.pop("project")
    result["tool"] = "taskflow"
    if result.get("ok") and result.get("accepted"):
        main = _load_main_plan(arguments, main_id)
        result["mainTaskReady"] = bool(main.get("tasks")) and all(
            n.get("status") == "completed" for n in main["tasks"]
        )
        if result["mainTaskReady"] and not os.getenv("TEAMHARNESS_COORDINATION_URL"):
            result["nextAction"] = {
                "tool": "taskflow",
                "action": "complete_task",
                "taskId": main_id,
                "requires": "verified final report",
            }
        if main.get("status") == "in_progress":
            result["dispatch"] = _advance_main_task(arguments, main)
            if not result["dispatch"].get("ok"):
                result.update(
                    ok=False,
                    retryable=True,
                    error="result accepted; downstream dispatch needs retry",
                )
    return result
