"""TeamHarness MCP projectflow tool."""

from __future__ import annotations

from typing import Any

import mcp_common as common
from protocol_bridge import protocol_core_enabled


def _maybe_dual_run(action: str, arguments: dict[str, Any], payload: dict[str, Any]) -> None:
    if not protocol_core_enabled():
        return
    # Phase 5: flag enables extra protocol validation hooks; full store dual-run in characterization.
    if action in {"plan_dag", "plan_loop"} and isinstance(payload.get("tasks"), list):
        common._validate_task_graph(
            [
                common._normalize_task(task)
                for task in payload["tasks"]
                if isinstance(task, dict)
            ]
        )


def projectflow(arguments: dict[str, Any]) -> dict[str, Any]:
    action = str(arguments.get("action") or "").strip()
    payload = common._payload(arguments)
    try:
        if action == "create_project":
            project_id = common._project_id_from_payload(arguments, payload)
            project = {
                "project_id": project_id,
                "title": str(payload.get("title") or project_id),
                "source": str(payload.get("source") or ""),
                "requester": str(payload.get("requester") or ""),
                "status": "active",
                "tasks": [],
            }
            reply_route = common._normalize_reply_route(payload.get("replyRoute") or payload.get("reply_route"))
            if not reply_route:
                reply_route = common._reply_route_from_requester(project["requester"])
            if reply_route:
                project["reply_route"] = reply_route
            source_room_id = common._source_room_id_from_payload(payload, reply_route)
            if source_room_id:
                project["source_room_id"] = source_room_id
            project_dir = common._project_dir(arguments, project_id)
            common._write_json(common._project_state_path(arguments, project_id), project)
            common._write_project_plan(project_dir, project)
            return {
                "ok": True,
                "tool": "projectflow",
                "action": action,
                "project": project,
                "notificationNeeded": common._notification_needed(action, project),
            }

        if action == "create_quick_project":
            project_id = common._project_id_from_payload(arguments, payload)
            title = str(payload.get("title") or project_id)
            assigned_to = str(payload.get("assignedTo") or payload.get("assigned_to") or "").strip()
            if not assigned_to:
                raise ValueError("assignedTo is required")
            room_id = str(payload.get("roomId") or payload.get("room_id") or "").strip()
            if not room_id:
                raise ValueError("roomId is required")
            spec = str(payload.get("spec") or "").strip()
            if not spec:
                raise ValueError("spec is required")
            task_id = common._safe_id(payload.get("taskId") or payload.get("task_id") or f"{project_id}-01", "taskId")
            if not task_id.startswith(f"{project_id}-"):
                raise ValueError("taskId must belong to projectId")
            if common._task_state_path(arguments, task_id).exists():
                raise ValueError(f"task already exists: {task_id}")
            task_node = {
                "task_id": task_id,
                "title": title,
                "assigned_to": assigned_to,
                "depends_on": [],
                "status": "assigned",
            }
            project = {
                "project_id": project_id,
                "title": title,
                "source": str(payload.get("source") or ""),
                "requester": str(payload.get("requester") or ""),
                "status": "active",
                "mode": "quick",
                "plan_type": "dag",
                "tasks": [task_node],
            }
            reply_route = common._normalize_reply_route(payload.get("replyRoute") or payload.get("reply_route"))
            if not reply_route:
                reply_route = common._reply_route_from_requester(project["requester"])
            if reply_route:
                project["reply_route"] = reply_route
            source_room_id = common._source_room_id_from_payload(payload, reply_route)
            if source_room_id:
                project["source_room_id"] = source_room_id
                task_node["source_room_id"] = source_room_id
            common._validate_assignment_room(project, room_id)
            project_dir = common._project_dir(arguments, project_id)
            common._write_json(common._project_state_path(arguments, project_id), project)
            common._write_project_plan(project_dir, project)

            task_dir = common._task_dir(arguments, task_id)
            task_dir.mkdir(parents=True, exist_ok=True)
            (task_dir / "spec.md").write_text(spec + "\n", encoding="utf-8")
            task = {
                "task_id": task_id,
                "project_id": project_id,
                "room_id": room_id,
                "status": "assigned",
                "spec_path": f"shared/tasks/{task_id}/spec.md",
                "assigned_to": assigned_to,
            }
            if source_room_id:
                task["source_room_id"] = source_room_id
            common._write_task(arguments, task)
            return {
                "ok": True,
                "tool": "projectflow",
                "action": action,
                "project": project,
                "task": task,
                "synced": common._sync_task(arguments, task_id),
                "notificationNeeded": common._notification_needed(action, project, task),
            }

        if action == "resolve_project":
            return common._resolve_project(arguments, payload)

        if action == "accept_task_result":
            return common._accept_task_result(arguments, payload)

        if action == "mark_requester_report_sent":
            return common._mark_requester_report_sent(arguments, payload)

        if action == "plan_dag":
            project_id = common._safe_id(payload.get("projectId") or payload.get("project_id"), "projectId")
            state_path = common._project_state_path(arguments, project_id)
            project = common._read_json(state_path, {"project_id": project_id, "title": project_id, "status": "active", "tasks": []})
            previous = {task.get("task_id"): task for task in project.get("tasks", [])}
            raw_tasks = payload.get("tasks")
            if not isinstance(raw_tasks, list):
                raise ValueError("tasks must be a list")
            planned_tasks = [
                common._normalize_task(task, previous.get(str(task.get("taskId") or task.get("task_id"))))
                for task in raw_tasks
                if isinstance(task, dict)
            ]
            common._validate_task_graph(planned_tasks)
            _maybe_dual_run(action, arguments, payload)
            project["tasks"] = planned_tasks
            project["plan_type"] = "dag"
            project_dir = common._project_dir(arguments, project_id)
            common._write_json(state_path, project)
            common._write_project_plan(project_dir, project)
            return {
                "ok": True,
                "tool": "projectflow",
                "action": action,
                "project": project,
                "readyNodes": common._ready_nodes(project),
                "notificationNeeded": common._notification_needed(action, project),
            }

        if action == "plan_loop":
            project_id = common._safe_id(payload.get("projectId") or payload.get("project_id"), "projectId")
            state_path = common._project_state_path(arguments, project_id)
            project = common._read_json(state_path, {"project_id": project_id, "title": project_id, "status": "active", "tasks": []})
            previous_loop = project.get("loop") if isinstance(project.get("loop"), dict) else {}
            previous_tasks = {
                task.get("task_id"): task
                for task in (previous_loop.get("tasks", []) if isinstance(previous_loop.get("tasks"), list) else [])
            }
            raw_tasks = payload.get("tasks") or []
            if not isinstance(raw_tasks, list):
                raise ValueError("tasks must be a list")
            max_iterations = common._positive_int(payload.get("maxIterations") or payload.get("max_iterations"), "maxIterations")
            current_iteration = common._non_negative_int(
                payload.get("currentIteration") or payload.get("current_iteration") or previous_loop.get("current_iteration") or 0,
                "currentIteration",
            )
            if current_iteration > max_iterations:
                raise ValueError("currentIteration cannot exceed maxIterations")
            planned_tasks = [
                common._normalize_task(task, previous_tasks.get(str(task.get("taskId") or task.get("task_id"))))
                for task in raw_tasks
                if isinstance(task, dict)
            ]
            common._validate_task_graph(planned_tasks)
            _maybe_dual_run(action, arguments, payload)
            loop = {
                "goal": str(payload.get("goal") or previous_loop.get("goal") or "").strip(),
                "stop_condition": str(payload.get("stopCondition") or payload.get("stop_condition") or previous_loop.get("stop_condition") or "").strip(),
                "iteration_template": str(payload.get("iterationTemplate") or payload.get("iteration_template") or previous_loop.get("iteration_template") or "").strip(),
                "max_iterations": max_iterations,
                "current_iteration": current_iteration,
                "status": common._safe_loop_status(payload.get("status") or previous_loop.get("status") or "running"),
                "tasks": planned_tasks,
                "history": previous_loop.get("history", []) if isinstance(previous_loop.get("history"), list) else [],
            }
            if not loop["goal"]:
                raise ValueError("goal is required")
            if not loop["stop_condition"]:
                raise ValueError("stopCondition is required")
            if not loop["iteration_template"]:
                raise ValueError("iterationTemplate is required")
            project["plan_type"] = "loop"
            project["loop"] = loop
            project_dir = common._project_dir(arguments, project_id)
            common._write_json(state_path, project)
            common._write_project_plan(project_dir, project)
            return {
                "ok": True,
                "tool": "projectflow",
                "action": action,
                "project": project,
                "loop": loop,
                "readyLoopNodes": common._ready_loop_nodes(project),
                "notificationNeeded": common._notification_needed(action, project),
            }

        if action == "ready_nodes":
            project_id = common._safe_id(payload.get("projectId") or payload.get("project_id"), "projectId")
            project = common._read_json(common._project_state_path(arguments, project_id))
            if not project:
                raise ValueError("project not found")
            return {
                "ok": True,
                "tool": "projectflow",
                "action": action,
                "project": project,
                "readyNodes": common._ready_nodes(project),
            }

        if action == "ready_loop_nodes":
            project_id = common._safe_id(payload.get("projectId") or payload.get("project_id"), "projectId")
            project = common._read_json(common._project_state_path(arguments, project_id))
            if not project:
                raise ValueError("project not found")
            return {
                "ok": True,
                "tool": "projectflow",
                "action": action,
                "project": project,
                "loop": project.get("loop", {}),
                "readyLoopNodes": common._ready_loop_nodes(project),
            }

        if action == "record_loop_iteration":
            project_id = common._safe_id(payload.get("projectId") or payload.get("project_id"), "projectId")
            project = common._read_json(common._project_state_path(arguments, project_id))
            if not project:
                raise ValueError("project not found")
            loop = project.get("loop") if isinstance(project.get("loop"), dict) else {}
            if not loop:
                raise ValueError(f"project has no loop plan: {project_id}")
            iteration = common._positive_int(payload.get("iteration"), "iteration")
            max_iterations = common._positive_int(loop.get("max_iterations"), "maxIterations")
            if iteration > max_iterations:
                raise ValueError("iteration cannot exceed maxIterations")
            decision = common._safe_loop_decision(payload.get("decision"))
            loop["status"] = {
                "continue": "running",
                "replan": "running",
                "ask_user": "waiting_user",
                "stop_success": "completed",
                "stop_blocked": "blocked",
            }[decision]
            loop["current_iteration"] = max(common._non_negative_int(loop.get("current_iteration") or 0, "currentIteration"), iteration)
            history = loop.get("history", []) if isinstance(loop.get("history"), list) else []
            history.append({
                "iteration": iteration,
                "decision": decision,
                "summary": str(payload.get("summary") or "").strip(),
                "next_action": str(payload.get("nextAction") or payload.get("next_action") or "").strip(),
            })
            loop["history"] = history
            project["plan_type"] = "loop"
            project["loop"] = loop
            project_dir = common._project_dir(arguments, project_id)
            common._write_json(common._project_state_path(arguments, project_id), project)
            common._write_project_plan(project_dir, project)
            return {
                "ok": True,
                "tool": "projectflow",
                "action": action,
                "project": project,
                "loop": loop,
                "readyLoopNodes": common._ready_loop_nodes(project),
                "notificationNeeded": common._notification_needed(
                    action, project, summary=f"record_loop_iteration: iteration {iteration} -> {decision}",
                ),
            }

        if action in {"pause_project", "resume_project", "complete_project"}:
            project_id = common._safe_id(payload.get("projectId") or payload.get("project_id"), "projectId")
            state_path = common._project_state_path(arguments, project_id)
            project = common._read_json(state_path)
            if not project:
                raise ValueError("project not found")
            if action == "pause_project":
                project["status"] = "paused"
            elif action == "resume_project":
                project["status"] = "active"
            else:
                project["status"] = "completed"
                loop = project.get("loop") if isinstance(project.get("loop"), dict) else {}
                if loop:
                    loop["status"] = "completed"
                    project["loop"] = loop
            project_dir = common._project_dir(arguments, project_id)
            common._write_json(state_path, project)
            common._write_project_plan(project_dir, project)
            result = {
                "ok": True,
                "tool": "projectflow",
                "action": action,
                "project": project,
            }
            publish_artifacts = common._payload_bool_field(payload, ("publishArtifacts", "publish_artifacts"), False)
            if action == "complete_project" and publish_artifacts and (project_dir / "result.md").is_file():
                result["publishedArtifacts"] = common._publish_project_artifacts(
                    arguments,
                    project,
                    project_id,
                    "",
                    common._attachment_parent_event_id(payload, arguments),
                )
            result["notificationNeeded"] = common._notification_needed(action, project)
            return result
    except ValueError as exc:
        return {"ok": False, "tool": "projectflow", "action": action, "error": str(exc)}

    return {"ok": False, "tool": "projectflow", "action": action, "error": f"unsupported action: {action}"}

