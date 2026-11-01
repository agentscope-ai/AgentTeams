# task meta/console helpers and the taskflow tool implementation (W1-a2: real module)
from __future__ import annotations

import datetime

import os

import uuid

from typing import Any

# Cross-group imports are top-level absolutes (`common.*`) — see the note in
# transitions.py: mcp/ on sys.path makes the identity identical in both
# `import server` and `import mcp.server` modes.
from mcp.common import refs
from mcp.common.constants import ALLOWED_TASK_RESULT_STATUSES
from mcp.common.errors import (
    _persisted_state_failure_result,
    _pull_failed_response,
    _sync_failure_result,
    _uncommitted_state_failure_result,
)
from mcp.common.identity import _canonical_assignee
from mcp.common.io import _read_json, _safe_read_bytes, _utc_timestamp
from mcp.common.payload import _first_text, _payload, _safe_id
from mcp.common.paths import _project_state_path, _task_dir, _task_state_path
from mcp.artifact.publish import (
    _attachment_parent_event_id,
    _normalize_workspace_artifact_path,
    _path_is_under,
)
from mcp.project.state import _loop_pause_state
from mcp.room.matrix import _team_human_matrix_ids, _team_leader_matrix_id
from mcp.room.notify import _send_attention_notification, _task_completion_notification
from .signal_events import _emit_signal_event
from .transitions import (
    IllegalTransitionError,
    _append_transition_history,
    _reject_task_transition,
    _require_task_mutable,
    _terminal_task_status,
    _transition_actor,
    _transition_guidance,
)

# Cross-group call-time lookups (common.refs): these names are still exec'd
# fragments or server-level definitions in the W1-a2 intermediate state; they
# resolve against the bound mcp/server.py namespace at call time, so test
# monkeypatches of server attributes stay visible.
_continuation_delivery_id = refs.proxy("_continuation_delivery_id")
_resolve_task_continuation = refs.proxy("_resolve_task_continuation")
_validate_assignment_room = refs.proxy("_validate_assignment_room")
_source_room_id_from_payload = refs.proxy("_source_room_id_from_payload")
_submission_result = refs.proxy("_submission_result")
_notification_needed = refs.proxy("_notification_needed")
_project_id_for_pull = refs.proxy("_project_id_for_pull")
_task_result_digest = refs.proxy("_task_result_digest")
_validate_task_redelegation = refs.proxy("_validate_task_redelegation")
_publish_task_artifacts = refs.proxy("_publish_task_artifacts")
_pull_project = refs.proxy("_pull_project")
_pull_task = refs.proxy("_pull_task")
_send_delegate_notification = refs.proxy("_send_delegate_notification")
_sync_task = refs.proxy("_sync_task")
_update_project_task = refs.proxy("_update_project_task")
_validate_assignee_membership = refs.proxy("_validate_assignee_membership")
_write_json = refs.proxy("_write_json")
_role = refs.proxy("_role")
_transition_task = refs.proxy("_transition_task")


_TASKFLOW_MUTATING_ACTIONS = frozenset({
    "delegate_task",
    "ack_task",
    "submit_task",
    "cancel_task",
    "report_progress",
    "request_attention",
})


def _load_task(arguments: dict[str, Any], task_id: str) -> dict[str, Any]:
    task = _read_json(_task_state_path(arguments, task_id))
    if not task:
        raise ValueError("task not found")
    if not task.get("task_id") and task.get("taskId"):
        task["task_id"] = str(task["taskId"])
    return task


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
        ("result_digest", "resultDigest"),
        ("result_path", "resultPath"),
        ("submitted_by_role", "submittedByRole"),
        ("submission_id", "submissionId"),
        ("submitted_at", "submittedAt"),
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
        "resultDigest",
        "resultPath",
        "submittedByRole",
        "submissionId",
        "submittedAt",
    ):
        task.pop(key, None)


def _write_task(arguments: dict[str, Any], task: dict[str, Any]) -> None:
    _ensure_console_task_meta(arguments, task)
    _write_json(_task_state_path(arguments, task["task_id"]), task)


refs.define("_write_task", _write_task)
# Self-calls go through the proxy so a test monkeypatch of server._write_task
# is visible from inside this module.
_write_task = refs.proxy("_write_task")


def _validate_task_result_status(value: Any) -> str:
    status = str(value or "SUCCESS").strip()
    if status not in ALLOWED_TASK_RESULT_STATUSES:
        raise ValueError(f"unsupported result status: {status}")
    return status


def _validate_iso8601(value: str, field: str) -> None:
    """Reject a deadline that is not an ISO-8601 timestamp (Z tolerated)."""
    try:
        datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as exc:
        raise ValueError(f"{field} must be an ISO-8601 timestamp") from exc


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


def _taskflow(arguments: dict[str, Any]) -> dict[str, Any]:
    action = str(arguments.get("action") or "").strip()
    payload = _payload(arguments)
    role = _role(arguments)
    try:
        # the lifecycle write API read-before-read: taskflow has no create action; every
        # action (delegate/ack/submit/cancel/check) belongs to a project.
        # Pull the authoritative meta.json first so a Controller pause /
        # resume / replan is visible before ack/submit/cancel write the
        # project node status back (otherwise the write-back would clobber
        # the Controller's paused status with a stale active).
        pid = _project_id_for_pull(arguments, payload)
        if pid and not _pull_project(arguments, pid):
            # Same rationale as _projectflow: ack/submit/cancel write the
            # project node status back — proceeding on stale local state
            # could clobber a Controller intervention.
            if action in _TASKFLOW_MUTATING_ACTIONS:
                return _pull_failed_response("taskflow", action)
        if action == "delegate_task":
            if role != "leader":
                raise ValueError("delegate_task requires leader role")
            project_id = _safe_id(payload.get("projectId") or payload.get("project_id"), "projectId")
            task_id = _safe_id(payload.get("taskId") or payload.get("task_id"), "taskId")
            assigned_to = str(payload.get("assignedTo") or payload.get("assigned_to") or "").strip()
            room_id = str(payload.get("roomId") or payload.get("room_id") or "")
            if not room_id:
                raise ValueError("roomId is required")
            project = _read_json(_project_state_path(arguments, project_id))
            paused, pause_sources = _loop_pause_state(project or {})
            if paused:
                raise ValueError(f"project is paused (sources: {', '.join(pause_sources)})")
            if not assigned_to:
                for item in project.get("tasks", []):
                    if isinstance(item, dict) and item.get("task_id") == task_id:
                        assigned_to = str(item.get("assigned_to") or item.get("assignedTo") or "").strip()
                        break
            _validate_assignment_room(project, room_id)
            _validate_task_redelegation(arguments, project, task_id, room_id)
            existing_task = _read_json(_task_state_path(arguments, task_id), {"project_id": project_id})
            _require_task_mutable(arguments, existing_task, task_id, action, to_status="assigned")
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
            assignment_mxid = str(assigned_to or "").strip()

            # Idempotent retry of a fully assigned task: the automatic
            # notification was already delivered (event_id recorded) - return
            # the existing assignment instead of sending a duplicate. The
            # assigned state must still reach shared storage: if the sync
            # fails, return a retryable failure so a later retry finishes it.
            #
            # A re-delegation to a *different* assignee is not a retry:
            # falling through re-prepares the task and sends a fresh
            # notification (with a fresh transaction id) so the new worker
            # actually receives the assignment instead of it being silently
            # deduplicated by the homeserver.
            existing_assignee = str(existing_task.get("assigned_to") or "").strip()
            redelegate = bool(
                assignment_mxid
                and _canonical_assignee(assignment_mxid) != _canonical_assignee(existing_assignee)
            )
            if (
                str(existing_task.get("status") or "") == "assigned"
                and existing_task.get("eventId")
                and not redelegate
            ):
                notification_reused = {
                    "sent": True,
                    "eventId": existing_task["eventId"],
                    "roomId": room_id,
                    "assignee": assignment_mxid,
                    "reused": True,
                }
                existing_task["notification"] = notification_reused
                synced = _sync_task(arguments, task_id)
                if not synced:
                    return {
                        "ok": False,
                        "tool": "taskflow",
                        "action": action,
                        "task": existing_task,
                        "synced": False,
                        "error": "task is assigned but shared-storage sync failed",
                        "retryable": True,
                        "notification": notification_reused,
                        "notificationNeeded": _notification_needed(
                            "delegate_task",
                            project or {"project_id": project_id},
                            existing_task,
                            summary=f"delegate_task: {task_id} assigned to {assigned_to}",
                        ),
                    }
                return {
                    "ok": True,
                    "tool": "taskflow",
                    "action": action,
                    "task": existing_task,
                    "synced": True,
                    "notification": notification_reused,
                    "notificationNeeded": _notification_needed(
                        "delegate_task",
                        project or {"project_id": project_id},
                        existing_task,
                        summary=f"delegate_task: {task_id} assigned to {assigned_to}",
                    ),
                }

            # Transition discipline: an in-flight (in_progress) or submitted
            # task must not be silently reset to prepared — rebuilding the
            # meta here would destroy the recorded progress/submission.
            # (The assigned+eventId retry above already returned; an assigned
            # task without eventId is a broken state and gets re-prepared.)
            _delegate_from = str(existing_task.get("status") or "")
            if _delegate_from in ("in_progress", "submitted"):
                raise ValueError(
                    f"delegate_task: task is '{_delegate_from}'; "
                    f"{_transition_guidance('delegate_task', _delegate_from)}"
                )

            # Atomicity contract: never mark a task assigned until the Worker
            # can actually receive it. Validate room membership first; then
            # prepare (status="prepared", files published); then send the
            # notification with a stable txn id; only commit
            # status="assigned" + eventId after the send succeeded. A send
            # failure returns a retryable error and leaves the task prepared.
            membership = _validate_assignee_membership(room_id, assignment_mxid)
            if not membership.get("ok"):
                return {
                    "ok": False,
                    "tool": "taskflow",
                    "action": action,
                    "task": existing_task,
                    "error": membership.get("error", "room membership validation failed"),
                    "retryable": True,
                    "notificationNeeded": _notification_needed(
                        "delegate_task",
                        project or {"project_id": project_id},
                        existing_task,
                        summary=f"delegate_task: {task_id} blocked before assignment",
                    ),
                }

            task_dir = _task_dir(arguments, task_id)
            task_dir.mkdir(parents=True, exist_ok=True)
            spec = str(payload.get("spec") or "")
            (task_dir / "spec.md").write_text(spec + ("\n" if spec else ""), encoding="utf-8")
            source_room_id = _source_room_id_from_payload(payload) or str(project.get("source_room_id") or "").strip()
            # Transaction id for this logical delegation attempt:
            # a re-delegation (different assignee) is a NEW attempt and
            # rotates the nonce; a retry of the SAME attempt (previous send
            # failed, or succeeded server-side but the client lost the
            # response) REUSES the persisted nonce so the homeserver
            # deduplicates the retry instead of emitting a second
            # assignment event.
            existing_notify_txn = str(existing_task.get("notifyTxn") or "").strip()
            if redelegate or not existing_notify_txn:
                notify_txn = f"delegate-{task_id}-{uuid.uuid4().hex[:12]}"
            else:
                notify_txn = existing_notify_txn
            task = {
                "task_id": task_id,
                "project_id": project_id,
                "room_id": room_id,
                "status": "prepared",
                "spec_path": f"shared/tasks/{task_id}/spec.md",
                "notifyTxn": notify_txn,
            }
            if assigned_to:
                task["assigned_to"] = assigned_to
            if source_room_id:
                task["source_room_id"] = source_room_id
            if not _delegate_from:
                # Creation is recorded with from="planned" (the project-node
                # view at this moment); prepared -> prepared retries are
                # no-op re-entries and stay out of the history.
                _append_transition_history(
                    task,
                    "planned",
                    "prepared",
                    "delegate_task",
                    _transition_actor(arguments),
                )
            else:
                # Re-delegate (a retry after a send/sync failure, or a repair
                # /re-dispatch of a broken or revision state) must not erase
                # the audit trail already recorded on this task: the fresh
                # meta dict below replaces meta.json, so carry the existing
                # entries over before the write.
                prior_history = existing_task.get("history")
                if isinstance(prior_history, list) and prior_history:
                    task["history"] = list(prior_history)
                if _delegate_from != "prepared":
                    # Only prepared -> prepared retries are silent no-op
                    # re-entries. Any other reachable re-entry changes the
                    # state and is recorded so the trail stays complete: a
                    # re-delegation to a new assignee, or an assigned task
                    # without eventId (the broken-state repair path;
                    # revision and the other terminal states are frozen
                    # upstream by the mutability guard).
                    _append_transition_history(
                        task,
                        _delegate_from,
                        "prepared",
                        "delegate_task",
                        _transition_actor(arguments),
                        note=(
                            "re-delegate: new assignee"
                            if redelegate
                            else "repair: assigned without eventId"
                        ),
                    )
            _write_task(arguments, task)
            # Publish task files to shared storage FIRST so a Worker that
            # receives the notification can read spec.md/meta.json. If the
            # publish fails, keep the task prepared and do NOT send the Matrix
            # notification - a Worker must never receive a reference to files
            # that were not published. A retry re-publishes (idempotent) and
            # only then proceeds to notify.
            synced = _sync_task(arguments, task_id)
            if not synced:
                return {
                    "ok": False,
                    "tool": "taskflow",
                    "action": action,
                    "task": task,
                    "synced": False,
                    "error": "failed to publish task files to shared storage",
                    "retryable": True,
                    "notificationNeeded": _notification_needed(
                        "delegate_task",
                        project or {"project_id": project_id},
                        task,
                        summary=f"delegate_task: {task_id} files pending publish",
                    ),
                }

            task_title = str(payload.get("title") or "").strip()
            if not task_title:
                for item in project.get("tasks", []):
                    if isinstance(item, dict) and item.get("task_id") == task_id:
                        task_title = str(item.get("title") or "").strip()
                        break
            has_matrix_env = bool(
                (os.getenv("AGENTTEAMS_MATRIX_URL", "").strip())
                and (os.getenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "").strip())
            )
            notification: dict[str, Any] = {
                "sent": False,
                "error": "notification skipped",
            }
            if assignment_mxid and _role(arguments) == "leader" and has_matrix_env:
                # Ordering contract (#1402/#1431): the dispatch decision
                # refreshes the AUTHORITATIVE pause state one last time
                # immediately before the send. The read at the top of
                # delegate_task predates membership validation, which can
                # span a Controller pause write; re-reading the local file
                # here would only replay the last pull (the maintainer's
                # round-16 repro: the pause was durable in shared storage
                # while the local copy still said active, and the
                # assignment went out anyway). A failed refresh is a
                # suppression too — never send on a state we cannot read.
                # A pause landing concurrently with the send itself does
                # not recall the in-flight Matrix event (already
                # dispatched work may finish under the agreed contract).
                if not _pull_project(arguments, project_id):
                    raise ValueError(
                        "cannot refresh the authoritative project state before "
                        "dispatch; refusing to send on a stale local copy (retry)"
                    )
                project = _read_json(_project_state_path(arguments, project_id))
                paused, pause_sources = _loop_pause_state(project or {})
                if paused:
                    raise ValueError(f"project is paused (sources: {', '.join(pause_sources)})")
                try:
                    notification = _send_delegate_notification(
                        arguments,
                        room_id=room_id,
                        task_id=task_id,
                        title=task_title or task_id,
                        assignee=assignment_mxid,
                        spec=spec,
                        txn=notify_txn,
                    )
                except Exception as exc:
                    notification = {
                        "sent": False,
                        "error": f"automatic notification failed: {exc}",
                    }
            task["notification"] = notification
            if not notification.get("sent"):
                # Retryable failure: the task stays prepared (not assigned),
                # so a retry cannot produce a duplicate assignment.
                return {
                    "ok": False,
                    "tool": "taskflow",
                    "action": action,
                    "task": task,
                    "synced": synced,
                    "notification": notification,
                    "retryable": True,
                    "notificationNeeded": _notification_needed(
                        "delegate_task",
                        project or {"project_id": project_id},
                        task,
                        summary=f"delegate_task: {task_id} notification pending",
                    ),
                }

            # Send succeeded - commit the assignment atomically with the
            # recorded event_id, then re-push the assigned state.
            # (prepared -> assigned goes through the transition table.)
            _transition_task(
                arguments,
                task,
                task_id,
                "assigned",
                "delegate_task",
                eventId=notification["eventId"],
            )
            project_task_updates: dict[str, Any] = {"status": "assigned"}
            if assigned_to:
                project_task_updates["assigned_to"] = assigned_to
            if source_room_id:
                project_task_updates["source_room_id"] = source_room_id
            _update_project_task(arguments, project_id, task_id, **project_task_updates)
            synced = _sync_task(arguments, task_id)
            if not synced:
                # The assignment was committed locally and the notification
                # was sent, but the assigned state did not reach shared
                # storage. Return a retryable failure instead of reporting
                # success: the idempotent retry (assigned + eventId) finishes
                # the sync without sending a duplicate notification.
                return {
                    "ok": False,
                    "tool": "taskflow",
                    "action": action,
                    "task": task,
                    "synced": False,
                    "error": "task assigned but shared-storage sync failed; retry to complete",
                    "retryable": True,
                    "notification": notification,
                    "notificationNeeded": _notification_needed(
                        "delegate_task",
                        project or {"project_id": project_id},
                        task,
                        summary=f"delegate_task: {task_id} assigned to {assigned_to}",
                    ),
                }
            return {
                "ok": True,
                "tool": "taskflow",
                "action": action,
                "task": task,
                "synced": True,
                "notification": notification,
                "notificationNeeded": _notification_needed(
                    "delegate_task",
                    project or {"project_id": project_id},
                    task,
                    summary=f"delegate_task: {task_id} assigned to {assigned_to}",
                ),
            }
        if action == "ack_task":
            if role not in {"worker", "remote-member"}:
                raise ValueError("ack_task requires worker or remote-member role")
            task_id = _safe_id(payload.get("taskId") or payload.get("task_id"), "taskId")
            local_task = _read_json(_task_state_path(arguments, task_id))
            if local_task:
                _require_task_mutable(arguments, local_task, task_id, action, to_status="in_progress")
            pulled = _pull_task(arguments, task_id)
            task = _load_task(arguments, task_id)
            _require_task_mutable(arguments, task, task_id, action, to_status="in_progress")
            if task.get("status") != "submitted":
                # Tightening: ack only makes sense after the assignment
                # (planned/prepared/missing -> structured error, not a
                # silent state jump).
                _ack_from = str(task.get("status") or "")
                if _ack_from not in ("assigned", "in_progress"):
                    _reject_task_transition(
                        _ack_from,
                        "in_progress",
                        "ack_task",
                        (
                            f"illegal_transition: ack_task: task is '{_ack_from}'; "
                            "delegate_task it first"
                        ),
                        arguments=arguments,
                        task_id=task_id,
                    )
                _transition_task(
                    arguments,
                    task,
                    task_id,
                    "in_progress",
                    "ack_task",
                    acknowledged_by_role=role,
                )
                _update_project_task(arguments, task.get("project_id", ""), task_id, status="in_progress")
            spec_path = _task_dir(arguments, task_id) / "spec.md"
            spec = spec_path.read_text(encoding="utf-8") if spec_path.exists() else ""
            return {
                "ok": True,
                "tool": "taskflow",
                "action": action,
                "task": task,
                "spec": spec,
                "pulled": pulled,
                "synced": _sync_task(arguments, task_id, exclude=["spec.md", "base/"]),
            }

        if action == "submit_task":
            if role not in {"worker", "remote-member"}:
                raise ValueError("submit_task requires worker or remote-member role")
            task_id = _safe_id(payload.get("taskId") or payload.get("task_id"), "taskId")
            task = _load_task(arguments, task_id)
            _require_task_mutable(arguments, task, task_id, action, to_status="submitted")
            summary = str(payload.get("summary") or "")
            status = _validate_task_result_status(payload.get("status"))
            deliverables = payload.get("deliverables") or []
            if not isinstance(deliverables, list):
                raise ValueError("deliverables must be a list")
            deliverables = _validate_task_deliverables(task_id, deliverables)
            submitted_result = {
                "status": status,
                "summary": summary,
                "deliverables": deliverables,
            }
            submitted_digest = _task_result_digest(submitted_result)
            if task.get("status") == "submitted":
                persisted_digest = _first_text(
                    task.get("result_digest"),
                    task.get("resultDigest"),
                )
                if not persisted_digest:
                    persisted_digest = _task_result_digest(_submission_result(task))
                if persisted_digest != submitted_digest:
                    raise ValueError("submit_task conflicts with existing submission")
                submission_id = _first_text(
                    task.get("submission_id"),
                    task.get("submissionId"),
                )
                if not submission_id:
                    raise ValueError("legacy submitted task has no submission identity and cannot be resubmitted")
                if not task.get("result_digest"):
                    task["result_digest"] = persisted_digest
                    try:
                        _write_task(arguments, task)
                    except OSError as exc:
                        return _persisted_state_failure_result(
                            tool="taskflow",
                            action=action,
                            error=exc,
                            task=task,
                        )
                try:
                    project_synced = _update_project_task(
                        arguments,
                        task.get("project_id", ""),
                        task_id,
                        status="submitted",
                    )
                except OSError as exc:
                    return _persisted_state_failure_result(
                        tool="taskflow",
                        action=action,
                        error=exc,
                        task=task,
                    )
                if not project_synced:
                    return _sync_failure_result({
                        "tool": "taskflow",
                        "action": action,
                        "task": task,
                        "reused": True,
                        "publishedArtifacts": [],
                    }, "submit_task project")
                # P0 ordering applies to the resubmit path too: sync
                # first, withhold the notification on a failed sync
                # (computed only on the success path), return the
                # retryable sync-failure result.
                synced = _sync_task(
                    arguments,
                    task_id,
                    exclude=["spec.md", "base/"],
                    result_paths=deliverables,
                )
                if not synced:
                    return _sync_failure_result({
                        "tool": "taskflow",
                        "action": action,
                        "task": task,
                        "reused": True,
                        "publishedArtifacts": [],
                    }, "submit_task")
                result = {
                    "ok": True,
                    "tool": "taskflow",
                    "action": action,
                    "task": task,
                    "reused": True,
                    "publishedArtifacts": [],
                    "synced": True,
                    "notification": _task_completion_notification(
                        arguments,
                        task,
                        task_id,
                        status,
                        summary,
                    ),
                    "notificationNeeded": _notification_needed(
                        "submit_task",
                        {"project_id": task.get("project_id", "")},
                        task,
                        summary=f"submit_task: {task_id} ({status})",
                    ),
                }
                if not synced:
                    return _sync_failure_result(result, "submit_task")
                result["notification"] = _task_completion_notification(
                    arguments,
                    task,
                    task_id,
                    status,
                    summary,
                )
                # S2 signal emission (INT-3): SIG-COMPLETE (idempotent path —
                # the deterministic event id makes a resubmit a no-op).
                _emit_signal_event(
                    arguments,
                    "submit_task",
                    task_id=task_id,
                    project_id=str(task.get("project_id") or ""),
                    ref=f"shared/tasks/{task_id}/meta.json",
                    ref_bytes=_safe_read_bytes(_task_state_path(arguments, task_id)),
                    payload={
                        "project_id": str(task.get("project_id") or ""),
                        "task_id": task_id,
                        "result_status": status,
                        "reused": True,
                    },
                    # Submission identity is the committed submission itself
                    # (fingerprint over task meta, incl. submission_id), not
                    # the project's latest revision. Reading the project
                    # audit_seq here made a retry unstable if another
                    # intervention advanced it (and differed from the first
                    # emit, which uses the default 0): two SIG-COMPLETE
                    # objects for one submission. Keep 0 on both paths.
                )
                return result
            # Tightening: submission requires an acknowledged assignment.
            _submit_from = str(task.get("status") or "")
            if _submit_from not in ("assigned", "in_progress"):
                _reject_task_transition(
                    _submit_from,
                    "submitted",
                    "submit_task",
                    (
                        f"illegal_transition: submit_task: task is '{_submit_from}'; "
                        "ack_task it first"
                    ),
                    arguments=arguments,
                    task_id=task_id,
                )
            task_dir = _task_dir(arguments, task_id)
            task_dir.mkdir(parents=True, exist_ok=True)
            submission_id = uuid.uuid4().hex
            submitted_at = _utc_timestamp()
            if (task_dir / "result.md").is_file():
                result_path_extra = {"result_path": f"shared/tasks/{task_id}/result.md"}
            else:
                result_path_extra = {}
                task.pop("result_path", None)
            try:
                _transition_task(
                    arguments,
                    task,
                    task_id,
                    "submitted",
                    "submit_task",
                    result_status=status,
                    summary=summary,
                    deliverables=deliverables,
                    submitted_by_role=role,
                    submission_id=submission_id,
                    submitted_at=submitted_at,
                    result_digest=submitted_digest,
                    continuation={
                        "status": "pending",
                        "delivery_id": _continuation_delivery_id(
                            _first_text(task.get("project_id"), task.get("projectId")),
                            task_id,
                            submission_id,
                        ),
                    },
                    **result_path_extra,
                )
            except OSError as exc:
                return _uncommitted_state_failure_result(tool="taskflow", action=action, error=exc)
            try:
                project_synced = _update_project_task(
                    arguments,
                    task.get("project_id", ""),
                    task_id,
                    status="submitted",
                )
            except OSError as exc:
                return _persisted_state_failure_result(
                    tool="taskflow",
                    action=action,
                    error=exc,
                    task=task,
                )
            if not project_synced:
                return _sync_failure_result({
                    "tool": "taskflow",
                    "action": action,
                    "task": task,
                    "publishedArtifacts": [],
                }, "submit_task project")
            published_artifacts = _publish_task_artifacts(
                arguments,
                task,
                task_id,
                deliverables,
                _attachment_parent_event_id(payload, arguments),
            )
            # P0 ordering (PR review 2026-09-05): the completion event is
            # an attention signal, not a receipt. Sync shared storage
            # first so the leader can immediately read result.md /
            # deliverables, and only then notify. A failed sync withholds
            # the notification (it is computed only on the success path)
            # and returns the retryable sync-failure result instead of
            # telling the leader "done" while the artifacts are
            # unreachable. The retry is idempotent (event reuse is keyed
            # by task + status).
            synced = _sync_task(
                arguments,
                task_id,
                exclude=["spec.md", "base/"],
                result_paths=deliverables,
            )
            result = {
                "ok": True,
                "tool": "taskflow",
                "action": action,
                "task": task,
                "publishedArtifacts": published_artifacts,
                "synced": synced,
                "notificationNeeded": _notification_needed(
                    "submit_task",
                    {"project_id": task.get("project_id", "")},
                    task,
                    summary=f"submit_task: {task_id} ({status})",
                ),
            }
            if not synced:
                return _sync_failure_result(result, "submit_task")
            result["notification"] = _task_completion_notification(
                arguments,
                task,
                task_id,
                status,
                summary,
            )
            # S2 signal emission (INT-3): SIG-COMPLETE after the main write
            # and sync succeeded.
            _emit_signal_event(
                arguments,
                "submit_task",
                task_id=task_id,
                project_id=str(task.get("project_id") or ""),
                ref=f"shared/tasks/{task_id}/meta.json",
                ref_bytes=_safe_read_bytes(_task_state_path(arguments, task_id)),
                payload={
                    "project_id": str(task.get("project_id") or ""),
                    "task_id": task_id,
                    "result_status": status,
                },
            )
            return result

        if action == "report_progress":
            if role not in {"worker", "remote-member"}:
                raise ValueError(
                    "report_progress requires worker or remote-member role"
                )
            task_id = _safe_id(payload.get("taskId") or payload.get("task_id"), "taskId")
            _pull_task(arguments, task_id)
            task = _load_task(arguments, task_id)
            _require_task_mutable(arguments, task, task_id, action)
            # Progress notes only exist while the task is actually in flight.
            _progress_from = str(task.get("status") or "")
            if _progress_from not in ("assigned", "in_progress"):
                _reject_task_transition(
                    _progress_from,
                    _progress_from,
                    "report_progress",
                    (
                        f"illegal_transition: report_progress: task is '{_progress_from}'; "
                        "ack_task it first"
                    ),
                    arguments=arguments,
                    task_id=task_id,
                )
            note = str(payload.get("note") or "").strip()
            if not note:
                raise ValueError("note is required")
            truncated = len(note) > 200
            if truncated:
                note = note[:200] + "…"
            # from == to on purpose: progress is a no-op re-entry recorded in
            # history (cap 50), with no state change and no notification.
            _append_transition_history(
                task,
                _progress_from,
                _progress_from,
                "progress",
                _transition_actor(arguments),
                note,
                record_noop=True,
            )
            _write_task(arguments, task)
            synced = _sync_task(arguments, task_id, exclude=["spec.md", "base/"])
            return {
                "ok": True,
                "tool": "taskflow",
                "action": action,
                "task": task,
                "synced": synced,
                "truncated": truncated,
            }

        if action == "request_attention":
            if role not in {"worker", "leader", "remote-member"}:
                raise ValueError(
                    "request_attention requires worker, leader, or remote-member role"
                )
            task_id = _safe_id(payload.get("taskId") or payload.get("task_id"), "taskId")
            task = _load_task(arguments, task_id)
            _require_task_mutable(arguments, task, task_id, action)
            kind = str(payload.get("kind") or "other").strip().lower()
            if kind not in {"approval", "decision", "escalation", "other"}:
                raise ValueError("kind must be one of approval, decision, escalation, other")
            question = str(payload.get("question") or payload.get("reason") or "").strip()
            if not question:
                raise ValueError("question is required")
            # Structured answer payload (#1418/#1394): options is the
            # validation set a human answer must echo, suggested is advisory
            # only (never auto-applied), and the deadline is recorded under
            # the canonical timeoutAt (expiresAt stays a compatibility
            # alias for the #1449/#1434 readers). Every field is optional
            # and written to the record only when provided, so records from
            # older workers keep their original shape.
            raw_options = payload.get("options")
            attention_options: list[dict[str, Any]] = []
            seen: set[str] = set()
            if raw_options is not None:
                if not isinstance(raw_options, list) or len(raw_options) > 10:
                    raise ValueError(
                        "options must be a list of {id, label} objects with at most 10 entries"
                    )
                for entry in raw_options:
                    if not isinstance(entry, dict):
                        raise ValueError("options entries must be {id, label} objects")
                    raw_id = entry.get("id")
                    raw_label = entry.get("label")
                    if not isinstance(raw_id, str) or not isinstance(raw_label, str):
                        raise ValueError("options entries require string id and label")
                    option_id = raw_id.strip()
                    option_label = raw_label.strip()
                    if not option_id or not option_label:
                        raise ValueError("options entries require non-empty string id and label")
                    if option_id in seen:
                        raise ValueError("options ids must be unique")
                    seen.add(option_id)
                    attention_entry: dict[str, Any] = {
                        "id": option_id,
                        "label": option_label,
                    }
                    raw_description = entry.get("description")
                    if raw_description is not None and not isinstance(raw_description, str):
                        raise ValueError("options entries require a string description")
                    description = (raw_description or "").strip()
                    if description:
                        attention_entry["description"] = description[:300]
                    attention_options.append(attention_entry)
            suggested_raw = payload.get("suggested")
            if suggested_raw is not None and not isinstance(suggested_raw, str):
                raise ValueError("suggested must be a string")
            suggested = (suggested_raw or "").strip()
            if suggested and suggested not in seen:
                raise ValueError("suggested must be the id of one of the provided options")
            timeout_raw = payload.get("timeoutAt", payload.get("timeout_at"))
            if timeout_raw is not None and not isinstance(timeout_raw, str):
                raise ValueError("timeoutAt must be a string")
            timeout_at = (timeout_raw or "").strip()
            if timeout_at:
                _validate_iso8601(timeout_at, "timeoutAt")
            expires_raw = payload.get("expiresAt", payload.get("expires_at"))
            if expires_raw is not None and not isinstance(expires_raw, str):
                raise ValueError("expiresAt must be a string")
            expires_at = (expires_raw or "").strip()
            if expires_at:
                _validate_iso8601(expires_at, "expiresAt")
            if timeout_at and expires_at and timeout_at != expires_at:
                raise ValueError(
                    "timeoutAt and expiresAt must be identical when both are provided"
                )
            # Canonical deadline: timeoutAt wins; a caller that used only
            # the expiresAt alias gets both keys written (same value).
            deadline_at = timeout_at or expires_at
            # S0 resolve provenance (#1394): validated up front so a bad
            # payload never reaches the write path; applied only on the
            # resolved=true close below.
            resolve_via_raw = payload.get("resolvedVia", payload.get("resolved_via"))
            if resolve_via_raw is None:
                resolve_via = "tool"
            elif not isinstance(resolve_via_raw, str):
                raise ValueError("resolvedVia must be a string")
            else:
                resolve_via = resolve_via_raw.strip().lower()
            if resolve_via not in {"tool", "dashboard", "im_reply", "im_command", "api"}:
                raise ValueError(
                    "resolvedVia must be one of tool, dashboard, im_reply, im_command, api"
                )
            resolve_by_raw = payload.get("resolvedBy", payload.get("resolved_by"))
            if resolve_by_raw is not None and not isinstance(resolve_by_raw, str):
                raise ValueError("resolvedBy must be a string")
            resolve_by = (resolve_by_raw or "").strip()[:200]
            resolve_evidence = payload.get("resolveEvidence", payload.get("resolve_evidence"))
            if resolve_evidence is not None and not isinstance(resolve_evidence, dict):
                raise ValueError("resolveEvidence must be an object")
            attention = task.get("attention")
            if not isinstance(attention, list):
                attention = []
            # Idempotent per kind while unresolved: re-requesting the same
            # kind before it was resolved reuses the recorded event instead
            # of pinging the room again. An explicit resolved=true closes
            # the open loop early (no new ping) and is sync-first like
            # submit_task: the resolved state must land in shared storage
            # before the caller is told the loop is closed.
            explicit_resolve = bool(payload.get("resolved", False))
            if explicit_resolve:
                # Target the most recent unresolved same-kind record. If
                # every same-kind record is already resolved, re-affirm the
                # latest one: this is the idempotent retry path after a
                # sync failure on a previous close (the local record is
                # already resolved, so the retry must re-sync rather than
                # raise a new record or ping). A call with no same-kind
                # record at all is rejected — there is no open loop to
                # close, and pre-creating a resolved record would still
                # ping the room, contradicting "close without a new ping".
                target = None
                for existing in reversed(attention):
                    if (
                        isinstance(existing, dict)
                        and str(existing.get("kind") or "") == kind
                        and not existing.get("resolved")
                    ):
                        target = existing
                        break
                if target is None:
                    for existing in reversed(attention):
                        if (
                            isinstance(existing, dict)
                            and str(existing.get("kind") or "") == kind
                            and existing.get("eventId")
                        ):
                            target = existing
                            break
                if target is None:
                    raise ValueError(
                        f"no attention record of kind '{kind}' to resolve; "
                        "call request_attention without resolved to raise one"
                    )
                target["resolved"] = True
                # S0 resolve provenance (#1394): who closed the loop and
                # how. A human reply in a room is recorded as im_reply with
                # the evidence cited, never conflated with a tool flip.
                target["resolvedVia"] = resolve_via
                if resolve_by:
                    target["resolvedBy"] = resolve_by
                if resolve_evidence:
                    target["resolveEvidence"] = resolve_evidence
                target["resolvedAt"] = datetime.datetime.now(datetime.timezone.utc).isoformat()
                _write_task(arguments, task)
                synced = _sync_task(arguments, task_id, exclude=["spec.md", "base/"])
                event_id = str(target.get("eventId") or "") or None
                if not synced:
                    return {
                        "ok": False,
                        "retryable": True,
                        "tool": "taskflow",
                        "action": action,
                        "task": task,
                        "attention": {
                            "kind": kind,
                            "resolved": True,
                            "eventId": event_id,
                        },
                        "synced": False,
                        "statePersisted": True,
                        "error": (
                            "shared storage sync failed after the attention "
                            f"record of kind '{kind}' was resolved locally; "
                            "the shared meta.json may still show it "
                            "unresolved. Retry request_attention with the "
                            "same payload (idempotent: no new ping is sent)."
                        ),
                    }
                return {
                    "ok": True,
                    "tool": "taskflow",
                    "action": action,
                    "task": task,
                    "attention": {
                        "kind": kind,
                        "resolved": True,
                        "eventId": event_id,
                        "notification": {
                            "sent": event_id is not None,
                            "eventId": event_id,
                            "reused": True,
                            "resolved": True,
                        },
                    },
                    "synced": True,
                }
            # One open loop per kind: an unresolved same-kind record is
            # the loop. An already-notified one (has eventId) is a plain
            # reuse — no new ping. A pending one (created but its first
            # sync failed, so it has no eventId yet) is retried: re-sync
            # and then send/persist exactly one event for it (deterministic
            # transaction id); a retry must never append a second record
            # or send a second event.
            pending = None
            for existing in reversed(attention):
                if (
                    not isinstance(existing, dict)
                    or existing.get("resolved")
                    or str(existing.get("kind") or "") != kind
                ):
                    continue
                if existing.get("eventId"):
                    # Sync-first, like the resolved close: when the
                    # original post-send push failed, the recorded
                    # eventId/outcome is still local-only. Push it before
                    # reporting success — a retry of an already-notified
                    # record must never re-send the Matrix event.
                    event_id = str(existing["eventId"])
                    synced = _sync_task(arguments, task_id, exclude=["spec.md", "base/"])
                    if not synced:
                        return {
                            "ok": False,
                            "retryable": True,
                            "tool": "taskflow",
                            "action": action,
                            "task": task,
                            "attention": {
                                "kind": kind,
                                "reused": True,
                                "notification": {
                                    "sent": True,
                                    "eventId": event_id,
                                    "reused": True,
                                },
                            },
                            "synced": False,
                            "statePersisted": True,
                            "error": (
                                "shared storage sync failed while re-syncing "
                                f"the already-notified attention record of kind "
                                f"'{kind}'; the Matrix event ({event_id}) is not "
                                "re-sent. Retry request_attention (idempotent: "
                                "only the sync is retried)."
                            ),
                        }
                    return {
                        "ok": True,
                        "tool": "taskflow",
                        "action": action,
                        "task": task,
                        "attention": {
                            "kind": kind,
                            "reused": True,
                            "notification": {
                                "sent": True,
                                "eventId": event_id,
                                "reused": True,
                            },
                        },
                        "synced": True,
                    }
                if pending is None:
                    pending = existing
            if pending is not None:
                record = pending
                attempt = int(record.get("attempt") or 1)
                question = str(record.get("question") or question)
                synced = _sync_task(arguments, task_id, exclude=["spec.md", "base/"])
                if not synced:
                    return {
                        "ok": False,
                        "retryable": True,
                        "tool": "taskflow",
                        "action": action,
                        "task": task,
                        "error": (
                            "shared storage sync failed while retrying the pending "
                            f"attention record of kind '{kind}'; the attention "
                            "notification was withheld. The pending record is reused "
                            "on retry (idempotent: no second record, no second "
                            "event)."
                        ),
                    }
            else:
                attempt = sum(
                    1 for item in attention if isinstance(item, dict) and item.get("kind") == kind
                ) + 1
                record: dict[str, Any] = {
                    "kind": kind,
                    "question": question[:500],
                    "attempt": attempt,
                    "requestedAt": datetime.datetime.now(datetime.timezone.utc).isoformat(),
                    # explicit resolved=true never reaches the new-record path
                    # (handled above): a freshly raised record is always open.
                    "resolved": False,
                }
                # camelCase to match the existing meta.json record fields;
                # each is written only when provided (backward compatible).
                # The deadline is recorded under the canonical timeoutAt;
                # the expiresAt alias is echoed (same value) only when the
                # caller used it, so the #1449/#1434 readers keep working.
                if attention_options:
                    record["options"] = attention_options
                if suggested:
                    record["suggested"] = suggested
                if deadline_at:
                    record["timeoutAt"] = deadline_at
                if expires_at:
                    record["expiresAt"] = expires_at
                attention.append(record)
                task["attention"] = attention
                _write_task(arguments, task)
                synced = _sync_task(arguments, task_id, exclude=["spec.md", "base/"])
                if not synced:
                    return {
                        "ok": False,
                        "retryable": True,
                        "tool": "taskflow",
                        "action": action,
                        "task": task,
                        "error": (
                            "shared storage sync failed after request_attention; the "
                            "attention notification was withheld. Local attention state "
                            "is recorded — retry request_attention (idempotent)."
                        ),
                    }
            room_id = str(task.get("room_id") or "").strip()
            if not room_id:
                notification = {
                    "sent": False,
                    "skipped": True,
                    "error": "task has no room_id",
                }
            else:
                leader = _team_leader_matrix_id()
                if not leader:
                    notification = {
                        "sent": False,
                        "skipped": True,
                        "error": "team leader Matrix ID not found in runtime config",
                    }
                else:
                    membership = _validate_assignee_membership(room_id, leader)
                    if not membership.get("ok"):
                        notification = {
                            "sent": False,
                            "skipped": True,
                            "error": str(membership.get("error") or "room membership check failed"),
                        }
                    else:
                        notification = _send_attention_notification(
                            arguments,
                            room_id=room_id,
                            task_id=task_id,
                            kind=kind,
                            question=question,
                            leader=leader,
                            worker=str(task.get("assigned_to") or ""),
                            humans=_team_human_matrix_ids(),
                            attempt=attempt,
                        )
                        if notification.get("sent"):
                            record["eventId"] = notification.get("eventId")
            # S4a notification outcome persistence (#1394): one durable
            # entry per logical attempt (one Matrix transaction id).
            # `room_accepted` is Matrix's acceptance of the room event;
            # `channel_accepted` is the per-recipient receipt and stays
            # null unless an adapter reports it — never inferred from room
            # acceptance. A first request appends the entry; a retry of
            # the same pending record (same attempt) re-uses it — the
            # entry's previous physical outcome is demoted into
            # `retries` and the entry is re-pointed at the new outcome,
            # so the log never grows a second entry per attempt.
            outcome_ts = datetime.datetime.now(datetime.timezone.utc).isoformat()
            log_entries = record.setdefault("notification_log", [])
            folded_entry = None
            for entry in reversed(log_entries):
                if isinstance(entry, dict) and int(entry.get("attempt") or 0) == attempt:
                    folded_entry = entry
                    break
            if folded_entry is not None:
                previous_outcome: dict[str, Any] = {
                    "ts": folded_entry.get("ts"),
                    "room_accepted": bool(folded_entry.get("room_accepted")),
                    "channel_accepted": folded_entry.get("channel_accepted"),
                }
                if folded_entry.get("error"):
                    previous_outcome["error"] = str(folded_entry["error"])[:300]
                folded_entry.setdefault("retries", []).append(previous_outcome)
                folded_entry["ts"] = outcome_ts
                folded_entry["room_accepted"] = bool(notification.get("sent"))
                folded_entry["channel_accepted"] = notification.get("channel_accepted")
                folded_entry.pop("error", None)
                if notification.get("error"):
                    folded_entry["error"] = str(notification["error"])[:300]
            else:
                new_entry: dict[str, Any] = {
                    "attempt": attempt,
                    "ts": outcome_ts,
                    "room_accepted": bool(notification.get("sent")),
                    "channel_accepted": notification.get("channel_accepted"),
                    "retries": list(notification.get("retries") or []),
                }
                if notification.get("error"):
                    new_entry["error"] = str(notification["error"])[:300]
                log_entries.append(new_entry)
            _write_task(arguments, task)
            # The outcome write above is local only: push it, so the
            # shared snapshot carries the notification_log and the
            # eventId the send produced. A failed push never rolls the
            # eventId back — a retry takes the reuse path and only
            # re-syncs, never re-sending the Matrix event.
            synced = _sync_task(arguments, task_id, exclude=["spec.md", "base/"])
            if not synced:
                if notification.get("sent"):
                    sync_error = (
                        "shared storage sync failed after the attention "
                        "notification was sent; the outcome and the eventId "
                        "are recorded locally. Retry request_attention "
                        "(idempotent: the recorded Matrix event is not "
                        "re-sent — only the sync is retried)."
                    )
                else:
                    sync_error = (
                        "shared storage sync failed after the attention "
                        "send attempt; the outcome is recorded locally. "
                        "Retry request_attention (idempotent: the pending "
                        "record is reused — no second record, no second "
                        "event)."
                    )
                return {
                    "ok": False,
                    "retryable": True,
                    "tool": "taskflow",
                    "action": action,
                    "task": task,
                    "attention": {
                        "kind": kind,
                        "attempt": attempt,
                        "notification": notification,
                    },
                    "synced": False,
                    "statePersisted": True,
                    "error": sync_error,
                }
            return {
                "ok": True,
                "tool": "taskflow",
                "action": action,
                "task": task,
                "attention": {
                    "kind": kind,
                    "attempt": attempt,
                    "notification": notification,
                },
                "synced": True,
            }

        if action == "cancel_task":
            if role != "leader":
                raise ValueError("cancel_task requires leader role")
            task_id = _safe_id(payload.get("taskId") or payload.get("task_id"), "taskId")
            task = _load_task(arguments, task_id)
            requested_submission_id = _first_text(
                payload.get("submissionId"),
                payload.get("submission_id"),
            )
            persisted_submission_id = _first_text(
                task.get("submission_id"),
                task.get("submissionId"),
            )
            if persisted_submission_id and not requested_submission_id:
                raise ValueError("submissionId is required for the current task submission")
            if requested_submission_id and requested_submission_id != persisted_submission_id:
                raise ValueError("submissionId does not match the current task submission")
            project_id = str(task.get("project_id") or "")
            reason = str(payload.get("reason") or payload.get("cancelReason") or payload.get("cancel_reason") or "").strip()
            if not reason:
                raise ValueError("reason is required")
            replacement_task_id = payload.get("replacementTaskId") or payload.get("replacement_task_id")
            normalized_replacement_task_id = (
                _safe_id(replacement_task_id, "replacementTaskId") if replacement_task_id else ""
            )
            terminal_status = _terminal_task_status(arguments, task, task_id)
            continuation = task.get("continuation") if isinstance(task.get("continuation"), dict) else {}
            cancellation_committed = bool(task.get("cancelled_at")) or continuation.get("status") == "resolved"
            if terminal_status == "cancelled" and cancellation_committed:
                persisted_reason = str(task.get("cancel_reason") or "").strip()
                persisted_replacement_task_id = str(task.get("replacement_task_id") or "").strip()
                if (
                    persisted_reason != reason
                    or persisted_replacement_task_id != normalized_replacement_task_id
                ):
                    raise ValueError("cancel_task conflicts with existing cancellation")
                try:
                    project_synced = _update_project_task(
                        arguments,
                        project_id,
                        task_id,
                        status="cancelled",
                    )
                except OSError as exc:
                    return _persisted_state_failure_result(
                        tool="taskflow",
                        action=action,
                        error=exc,
                        task=task,
                    )
                if not project_synced:
                    return _sync_failure_result({
                        "tool": "taskflow",
                        "action": action,
                        "task": task,
                        "project": _read_json(_project_state_path(arguments, project_id)) if project_id else {},
                        "reused": True,
                    }, "cancel_task project")
                synced = _sync_task(arguments, task_id, exclude=["spec.md", "base/"])
                result = {
                    "ok": True,
                    "tool": "taskflow",
                    "action": action,
                    "task": task,
                    "project": _read_json(_project_state_path(arguments, project_id)) if project_id else {},
                    "reused": True,
                    "synced": synced,
                }
                if not synced:
                    return _sync_failure_result(result, "cancel_task")
                return result
            if terminal_status:
                _reject_task_transition(
                    terminal_status,
                    "cancelled",
                    "cancel_task",
                    (
                        f"cannot cancel terminal task: {terminal_status} "
                        f"(task is in terminal state {terminal_status})"
                    ),
                    arguments=arguments,
                    task_id=task_id,
                )

            _resolve_task_continuation(task, "cancelled")
            if normalized_replacement_task_id:
                replacement_extra = {"replacement_task_id": normalized_replacement_task_id}
            else:
                replacement_extra = {}
                task.pop("replacement_task_id", None)
            try:
                _transition_task(
                    arguments,
                    task,
                    task_id,
                    "cancelled",
                    "cancel_task",
                    note=reason,
                    cancel_reason=reason,
                    cancelled_at=_utc_timestamp(),
                    **replacement_extra,
                )
            except OSError as exc:
                return _uncommitted_state_failure_result(tool="taskflow", action=action, error=exc)

            try:
                project_synced = _update_project_task(
                    arguments,
                    project_id,
                    task_id,
                    status="cancelled",
                )
            except OSError as exc:
                return _persisted_state_failure_result(
                    tool="taskflow",
                    action=action,
                    error=exc,
                    task=task,
                )
            if not project_synced:
                return _sync_failure_result({
                    "tool": "taskflow",
                    "action": action,
                    "task": task,
                    "project": _read_json(_project_state_path(arguments, project_id)) if project_id else {},
                }, "cancel_task project")
            synced = _sync_task(arguments, task_id, exclude=["spec.md", "base/"])
            result = {
                "ok": True,
                "tool": "taskflow",
                "action": action,
                "task": task,
                "project": _read_json(_project_state_path(arguments, project_id)) if project_id else {},
                "synced": synced,
            }
            if not synced:
                return _sync_failure_result(result, "cancel_task")
            return result

        if action == "check_task":
            if role != "leader":
                raise ValueError("check_task requires leader role")
            task_id = _safe_id(payload.get("taskId") or payload.get("task_id"), "taskId")
            pulled = _pull_task(arguments, task_id)
            task = _load_task(arguments, task_id)
            result, validation_errors = _task_result_from_meta(task)
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
    except IllegalTransitionError as exc:
        return {
            "ok": False,
            "tool": "taskflow",
            "action": action,
            "error": "illegal_transition",
            "from": exc.from_status,
            "to": exc.to_status,
            "detail": str(exc),
        }
    except ValueError as exc:
        return {"ok": False, "tool": "taskflow", "action": action, "error": str(exc)}

    return {"ok": False, "tool": "taskflow", "action": action, "error": f"unsupported action: {action}"}


