"""TeamHarness MCP taskflow tool."""

from __future__ import annotations

from typing import Any

import mcp_common as common


def taskflow(arguments: dict[str, Any]) -> dict[str, Any]:
    action = str(arguments.get("action") or "").strip()
    payload = common._payload(arguments)
    role = common._role(arguments)
    try:
        if action == "delegate_task":
            if role != "leader":
                raise ValueError("delegate_task requires leader role")
            project_id = common._safe_id(payload.get("projectId") or payload.get("project_id"), "projectId")
            task_id = common._safe_id(payload.get("taskId") or payload.get("task_id"), "taskId")
            assigned_to = str(payload.get("assignedTo") or payload.get("assigned_to") or "").strip()
            room_id = str(payload.get("roomId") or payload.get("room_id") or "")
            if not room_id:
                raise ValueError("roomId is required")
            project = common._read_json(common._project_state_path(arguments, project_id))
            if not assigned_to:
                for item in project.get("tasks", []):
                    if isinstance(item, dict) and item.get("task_id") == task_id:
                        assigned_to = str(item.get("assigned_to") or item.get("assignedTo") or "").strip()
                        break
            common._validate_assignment_room(project, room_id)
            common._validate_task_redelegation(arguments, project, task_id, room_id)
            existing_task = common._read_json(common._task_state_path(arguments, task_id), {"project_id": project_id})
            common._require_task_mutable(arguments, existing_task, task_id, action)
            assigned_to = str(payload.get("assignedTo") or payload.get("assigned_to") or "").strip()
            if not assigned_to:
                tasks = project.get("tasks", []) if isinstance(project.get("tasks"), list) else []
                loop = project.get("loop") if isinstance(project.get("loop"), dict) else {}
                if isinstance(loop.get("tasks"), list):
                    tasks = tasks + loop["tasks"]
                for item in tasks:
                    if isinstance(item, dict) and item.get("task_id") == task_id:
                        assigned_to = str(item.get("assigned_to") or "").strip()
                        break
            task_dir = common._task_dir(arguments, task_id)
            task_dir.mkdir(parents=True, exist_ok=True)
            spec = str(payload.get("spec") or "")
            (task_dir / "spec.md").write_text(spec + ("\n" if spec else ""), encoding="utf-8")
            source_room_id = common._source_room_id_from_payload(payload) or str(project.get("source_room_id") or "").strip()
            task = {
                "task_id": task_id,
                "project_id": project_id,
                "room_id": room_id,
                "status": "assigned",
                "spec_path": f"shared/tasks/{task_id}/spec.md",
            }
            if assigned_to:
                task["assigned_to"] = assigned_to
            if source_room_id:
                task["source_room_id"] = source_room_id
            if assigned_to:
                task["assigned_to"] = assigned_to
            common._write_task(arguments, task)
            project_task_updates: dict[str, Any] = {"status": "assigned"}
            if assigned_to:
                project_task_updates["assigned_to"] = assigned_to
            if source_room_id:
                project_task_updates["source_room_id"] = source_room_id
            if assigned_to:
                project_task_updates["assigned_to"] = assigned_to
            common._update_project_task(arguments, project_id, task_id, **project_task_updates)
            return {
                "ok": True,
                "tool": "taskflow",
                "action": action,
                "task": task,
                "synced": common._sync_task(arguments, task_id),
                "notificationNeeded": common._notification_needed(
                    "delegate_task",
                    project or {"project_id": project_id},
                    task,
                    summary=f"delegate_task: {task_id} assigned to {assigned_to}",
                ),
            }

        if action == "ack_task":
            if role not in {"worker", "remote-member"}:
                raise ValueError("ack_task requires worker or remote-member role")
            task_id = common._safe_id(payload.get("taskId") or payload.get("task_id"), "taskId")
            pulled = common._pull_task(arguments, task_id)
            task = common._load_task(arguments, task_id)
            common._require_task_mutable(arguments, task, task_id, action)
            task["status"] = "in_progress"
            task["acknowledged_by_role"] = role
            common._write_task(arguments, task)
            common._update_project_task(arguments, task.get("project_id", ""), task_id, status="in_progress")
            spec_path = common._task_dir(arguments, task_id) / "spec.md"
            spec = spec_path.read_text(encoding="utf-8") if spec_path.exists() else ""
            return {
                "ok": True,
                "tool": "taskflow",
                "action": action,
                "task": task,
                "spec": spec,
                "pulled": pulled,
                "synced": common._sync_task(arguments, task_id, exclude=["spec.md", "base/"]),
            }

        if action == "submit_task":
            if role not in {"worker", "remote-member"}:
                raise ValueError("submit_task requires worker or remote-member role")
            task_id = common._safe_id(payload.get("taskId") or payload.get("task_id"), "taskId")
            task = common._load_task(arguments, task_id)
            common._require_task_mutable(arguments, task, task_id, action)
            summary = str(payload.get("summary") or "")
            status = str(payload.get("status") or "SUCCESS")
            deliverables = payload.get("deliverables") or []
            if not isinstance(deliverables, list):
                raise ValueError("deliverables must be a list")
            deliverables = common._validate_task_deliverables(task_id, deliverables)
            task_dir = common._task_dir(arguments, task_id)
            task_dir.mkdir(parents=True, exist_ok=True)
            task.update({
                "status": "submitted",
                "result_status": status,
                "summary": summary,
                "deliverables": deliverables,
                "submitted_by_role": role,
            })
            if (task_dir / "result.md").is_file():
                task["result_path"] = f"shared/tasks/{task_id}/result.md"
            else:
                task.pop("result_path", None)
            common._write_task(arguments, task)
            common._update_project_task(arguments, task.get("project_id", ""), task_id, status="submitted")
            published_artifacts = common._publish_task_artifacts(
                arguments,
                task,
                task_id,
                deliverables,
                common._attachment_parent_event_id(payload, arguments),
            )
            return {
                "ok": True,
                "tool": "taskflow",
                "action": action,
                "task": task,
                "publishedArtifacts": published_artifacts,
                "synced": common._sync_task(arguments, task_id, exclude=["spec.md", "base/"]),
                "notificationNeeded": common._notification_needed(
                    "submit_task",
                    {"project_id": task.get("project_id", "")},
                    task,
                    summary=f"submit_task: {task_id} ({status})",
                ),
            }

        if action == "cancel_task":
            if role != "leader":
                raise ValueError("cancel_task requires leader role")
            task_id = common._safe_id(payload.get("taskId") or payload.get("task_id"), "taskId")
            task = common._load_task(arguments, task_id)
            project_id = str(task.get("project_id") or "")
            terminal_status = common._terminal_task_status(arguments, task, task_id)
            if terminal_status:
                raise ValueError(f"cannot cancel terminal task: {terminal_status}")
            reason = str(payload.get("reason") or payload.get("cancelReason") or payload.get("cancel_reason") or "").strip()
            if not reason:
                raise ValueError("reason is required")
            replacement_task_id = payload.get("replacementTaskId") or payload.get("replacement_task_id")

            task["status"] = "cancelled"
            task["cancel_reason"] = reason
            if replacement_task_id:
                task["replacement_task_id"] = common._safe_id(replacement_task_id, "replacementTaskId")
            else:
                task.pop("replacement_task_id", None)
            common._write_task(arguments, task)

            common._update_project_task(arguments, project_id, task_id, status="cancelled")
            return {
                "ok": True,
                "tool": "taskflow",
                "action": action,
                "task": task,
                "project": common._read_json(common._project_state_path(arguments, project_id)) if project_id else {},
                "synced": common._sync_task(arguments, task_id, exclude=["spec.md", "base/"]),
            }

        if action == "check_task":
            if role != "leader":
                raise ValueError("check_task requires leader role")
            task_id = common._safe_id(payload.get("taskId") or payload.get("task_id"), "taskId")
            pulled = common._pull_task(arguments, task_id)
            task = common._load_task(arguments, task_id)
            result, validation_errors = common._task_result_from_meta(task)
            effective = task.get("status") == "submitted" and not validation_errors
            return {
                "ok": True,
                "tool": "taskflow",
                "action": action,
                "task": task,
                "result": result,
                "validationErrors": validation_errors,
                "effective": effective,
                "pulled": pulled,
            }
    except ValueError as exc:
        return {"ok": False, "tool": "taskflow", "action": action, "error": str(exc)}

    return {"ok": False, "tool": "taskflow", "action": action, "error": f"unsupported action: {action}"}

