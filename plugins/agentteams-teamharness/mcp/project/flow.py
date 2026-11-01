# projectflow tool implementation and task-result acceptance (W1-a2: real module)
from __future__ import annotations

import hashlib

import json

import re

import uuid

from typing import Any

# Cross-group imports are top-level absolutes (see lifecycle/transitions.py
# note): mcp/ on sys.path keeps one module identity in both load modes.
from mcp.common import refs
from mcp.common.config import _runtime_team_name
from mcp.common.constants import (
    LOOP_BUDGET_BY_TIER,
    LOOP_PROVENANCE_LIMIT,
    TERMINAL_TASK_STATUSES,
)
from mcp.common.errors import (
    _persisted_state_failure_result,
    _pull_failed_response,
    _sync_failure_result,
    _uncommitted_state_failure_result,
)
from mcp.common.identity import _canonical_room_id
from mcp.common.io import _read_json, _safe_read_bytes, _utc_timestamp
from mcp.common.payload import (
    _first_text,
    _non_negative_int,
    _payload,
    _payload_bool,
    _payload_bool_field,
    _positive_int,
    _safe_id,
)
from mcp.common.paths import _project_dir, _project_state_path, _task_dir, _task_state_path
from mcp.artifact.publish import _attachment_parent_event_id
from mcp.artifact.sync import _load_authoritative_task_optional
from mcp.common.config import _protocol_enforce_enabled
from mcp.lifecycle.review import ProtocolGuardError, _protocol_guard_for_accept
from mcp.lifecycle.signal_events import _emit_signal_event, _list_events, _project_audit_seq
# taskflow keeps NO module-level import of this module (its 9 flow names
# are refs proxies), so this direction of the pair is cycle-free.
from mcp.lifecycle.taskflow import _task_result_from_meta, _validate_task_result_status
from mcp.lifecycle.transitions import _append_transition_history, _transition_actor
from mcp.room.notify import _send_project_completion_notification
from mcp.room.rooms import _runtime_team_room_id
from mcp.transport.route import _normalize_reply_route, _reply_route_from_requester
from .state import (
    _mark_requester_report_sent,
    _normalize_tasks,
    _project_id_from_payload,
    _project_task_status,
    _ready_loop_nodes,
    _ready_nodes,
    _resolve_project,
    _safe_loop_decision,
    _safe_loop_status,
    _validate_loop_id_mapping,
    _validate_task_graph,
)

# Call-time lookups (common.refs): names that still live in exec'd
# fragments (artifact / room / transport groups) or at server level
# (_role) resolve against the bound mcp/server.py namespace at call
# time, so test monkeypatches of server attributes stay visible.
_publish_project_artifacts = refs.proxy("_publish_project_artifacts")
_pull_project = refs.proxy("_pull_project")
_role = refs.proxy("_role")
_sync_project = refs.proxy("_sync_project")
_sync_task = refs.proxy("_sync_task")
_write_json = refs.proxy("_write_json")
_write_project_plan = refs.proxy("_write_project_plan")
_write_task = refs.proxy("_write_task")


# Loop budget ledger (project["loop"]["budget"]). No budget by default:
# iteration_budget stays None unless the project explicitly declares a
# recognized tier or iteration_budget - an explicit project
# iteration_budget wins over the tier mapping. Over budget the loop is
# flagged (over_budget + provenance), not truncated. iterations_used is
# a view of len(loop["iterations"]) recomputed on every record, so it is
# never initialized from the previous budget here (tokens_used is the
# only field carried over).
def _loop_budget(project: dict[str, Any], previous_budget: Any) -> dict[str, Any]:
    tier = str(project.get("tier") or "").strip().lower()
    tier = tier if tier in LOOP_BUDGET_BY_TIER else None
    budget = {
        "tier": tier,
        "iteration_budget": LOOP_BUDGET_BY_TIER[tier] if tier else None,
        "iterations_used": 0,
        "tokens_used": 0,
        "over_budget": False,
    }
    explicit = project.get("iteration_budget")
    if explicit is not None:
        budget["iteration_budget"] = _non_negative_int(explicit, "iteration_budget")
    if isinstance(previous_budget, dict):
        budget["tokens_used"] = _non_negative_int(previous_budget.get("tokens_used") or 0, "tokens_used")
    budget["over_budget"] = (
        budget["iteration_budget"] is not None
        and budget["iterations_used"] > budget["iteration_budget"]
    )
    return budget


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


def _external_requester_channel(project: dict[str, Any]) -> str:
    reply_route = project.get("reply_route") if isinstance(project.get("reply_route"), dict) else {}
    channel = str(reply_route.get("channel") or project.get("source") or "").strip().lower()
    if channel and channel != "matrix":
        return channel
    requester_route = _reply_route_from_requester(project.get("requester"))
    channel = str(requester_route.get("channel") or "").strip().lower()
    return channel if channel and channel != "matrix" else ""


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


def _accepted_node_status(result_status: Any) -> str:
    status = _validate_task_result_status(result_status)
    if status in {"SUCCESS", "SUCCESS_WITH_NOTES"}:
        return "completed"
    if status == "REVISION_NEEDED":
        return "revision"
    if status in {"BLOCKED", "INTERRUPTED"}:
        return "blocked"
    raise ValueError(f"unsupported result status: {status}")


def _submission_result(task: dict[str, Any]) -> dict[str, Any]:
    deliverables = task.get("deliverables")
    if not isinstance(deliverables, list):
        deliverables = []
    return {
        "status": str(task.get("result_status") or task.get("resultStatus") or ""),
        "summary": str(task.get("summary") or ""),
        "deliverables": [str(item) for item in deliverables],
    }


def _task_result_digest(result: dict[str, Any]) -> str:
    """Return the cross-runtime identity of a structured task result."""
    deliverables = result.get("deliverables")
    if not isinstance(deliverables, list):
        deliverables = []
    canonical_result = {
        "status": str(result.get("status") or "").strip(),
        "summary": re.sub(r"\s+", " ", str(result.get("summary") or "")).strip(),
        "deliverables": [str(item) for item in deliverables],
    }
    canonical_json = json.dumps(
        canonical_result,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")
    return hashlib.sha256(
        b"teamharness.task-result.v1\0" + canonical_json,
    ).hexdigest()


def _continuation_delivery_id(project_id: str, task_id: str, submission_id: str) -> str:
    identity = "\0".join((project_id, task_id, submission_id, "result-submitted:v1"))
    return hashlib.sha256(identity.encode("utf-8")).hexdigest()


def _resolve_task_continuation(task: dict[str, Any], resolution: str) -> None:
    continuation = task.get("continuation") if isinstance(task.get("continuation"), dict) else {}
    if not continuation:
        return
    continuation["status"] = "resolved"
    continuation["resolution"] = resolution
    continuation["resolved_at"] = continuation.get("resolved_at") or _utc_timestamp()
    task["continuation"] = continuation


def _accept_task_result(arguments: dict[str, Any], payload: dict[str, Any]) -> dict[str, Any]:
    if _role(arguments) != "leader":
        raise ValueError("accept_task_result requires leader role")
    project_id = _safe_id(payload.get("projectId") or payload.get("project_id"), "projectId")
    task_id = _safe_id(payload.get("taskId") or payload.get("task_id"), "taskId")
    project = _read_json(_project_state_path(arguments, project_id))
    if not project:
        raise ValueError("project not found")
    task_meta = _read_json(_task_state_path(arguments, task_id))
    # W2 round 10 (#1467): resolve the authoritative task INDEPENDENTLY of
    # the local cache. Round 9 pulled only when a local copy existed, so a
    # fresh Leader workspace skipped the pull — and every acceptance gate
    # below — while the task existed in shared storage: an r1 task with no
    # review and no human approval was accepted with nodeStatus completed
    # while the Task stayed submitted (the maintainer's round-10 repro).
    # A failed pull still fails closed; a genuinely absent task keeps the
    # legacy no-task path ONLY while protocol enforcement is off — under
    # enforcement the acceptance must not bypass the gates.
    authoritative = _load_authoritative_task_optional(arguments, task_id)
    if authoritative is not None:
        task_meta = authoritative
    elif _protocol_enforce_enabled():
        raise ProtocolGuardError(
            "task_not_found",
            "accept_task_result requires an authoritative task record while "
            "protocol enforcement is enabled",
        )
    task_project_id = _first_text(task_meta.get("project_id"), task_meta.get("projectId"))
    if task_project_id and task_project_id != project_id:
        raise ValueError("task does not belong to project")
    if task_meta:
        task_status = str(task_meta.get("status") or "")
        if task_status not in {"submitted", *TERMINAL_TASK_STATUSES}:
            raise ValueError(f"accept_task_result requires submitted task state, got {task_status or 'missing'}")
    requested_submission_id = _first_text(payload.get("submissionId"), payload.get("submission_id"))
    persisted_submission_id = _first_text(task_meta.get("submission_id"), task_meta.get("submissionId"))
    legacy_identity_persisted = False
    if persisted_submission_id and not requested_submission_id:
        raise ValueError("submissionId is required for the current task submission")
    if task_meta and not persisted_submission_id and requested_submission_id:
        raise ValueError("accept_task_result requires a submission identity")
    if requested_submission_id and requested_submission_id != persisted_submission_id:
        raise ValueError("submissionId does not match the current task submission")
    if task_meta:
        persisted_result, validation_errors = _task_result_from_meta(task_meta)
        if validation_errors:
            raise ValueError(f"persisted task result is invalid: {'; '.join(validation_errors)}")
        persisted_result_digest = _first_text(
            task_meta.get("result_digest"),
            task_meta.get("resultDigest"),
        )
        if persisted_result_digest and persisted_result_digest != _task_result_digest(persisted_result):
            raise ValueError("persisted task result digest does not match its structured result")
        if not persisted_submission_id:
            persisted_submission_id = uuid.uuid4().hex
            task_meta["submission_id"] = persisted_submission_id
            task_meta["submitted_at"] = _utc_timestamp()
            task_meta["result_digest"] = _task_result_digest(persisted_result)
            task_meta["continuation"] = {
                "status": "pending",
                "delivery_id": _continuation_delivery_id(
                    project_id,
                    task_id,
                    persisted_submission_id,
                ),
            }
            try:
                _write_task(arguments, task_meta)
            except OSError as exc:
                return _uncommitted_state_failure_result(
                    tool="projectflow",
                    action="accept_task_result",
                    error=exc,
                )
            legacy_identity_persisted = True
    result_status_value = payload.get("resultStatus") or payload.get("result_status")
    accepted = _payload_bool(payload.get("accepted"), True)
    if task_meta:
        persisted_result_status = str(task_meta.get("result_status") or "")
        if str(result_status_value or "SUCCESS").strip() != persisted_result_status:
            raise ValueError("resultStatus does not match the submitted task result")
        # W2 protocol gates (protocol.enforce switch, default off): a
        # missing / non-passing review record or a missing r1 approval
        # record refuses a POSITIVE acceptance (error_code surface); a
        # rejection / return-for-revision (accepted=False) is never gated.
        _protocol_guard_for_accept(task_meta, accepted)
    node_status = _accepted_node_status(result_status_value)
    if not accepted and node_status == "completed":
        result_status_value = "REVISION_NEEDED"
        node_status = "revision"
    current_node_status = _project_task_status(project, task_id)
    if current_node_status in TERMINAL_TASK_STATUSES:
        if current_node_status != node_status:
            raise ValueError(f"task result already decided as {current_node_status}")
        try:
            _write_project_plan(_project_dir(arguments, project_id), project)
        except OSError as exc:
            return _persisted_state_failure_result(
                tool="projectflow",
                action="accept_task_result",
                error=exc,
                task=task_meta or None,
                project=project,
            )
        if not _sync_project(arguments, project_id):
            return _sync_failure_result({
                "tool": "projectflow",
                "action": "accept_task_result",
                "project": project,
                "task": task_meta or None,
                "taskId": task_id,
                "submissionId": persisted_submission_id or None,
                "nodeStatus": current_node_status,
                "accepted": current_node_status == "completed",
                "reused": True,
                "publishedArtifacts": [],
                "notificationNeeded": {},
            }, "accept_task_result project")
        repaired_task_fence = bool(task_meta) and (
            str(task_meta.get("status") or "") != current_node_status
            or (
                isinstance(task_meta.get("continuation"), dict)
                and task_meta["continuation"].get("status") != "resolved"
            )
        )
        synced: bool | None = None
        if repaired_task_fence:
            task_meta["status"] = current_node_status
            _resolve_task_continuation(task_meta, current_node_status)
            try:
                _write_task(arguments, task_meta)
            except OSError as exc:
                return _persisted_state_failure_result(
                    tool="projectflow",
                    action="accept_task_result",
                    error=exc,
                    task=task_meta,
                    project=project,
                )
        if task_meta:
            # A previous acceptance may have committed locally while its
            # shared-storage push failed. Retrying the same decision must
            # repair that external side effect without rewriting project
            # state or reopening the requester report.
            synced = _sync_task(arguments, task_id, exclude=["spec.md", "base/"])
        reused_result = {
            "ok": True,
            "tool": "projectflow",
            "action": "accept_task_result",
            "project": project,
            "taskId": task_id,
            "submissionId": persisted_submission_id or None,
            "nodeStatus": current_node_status,
            "accepted": current_node_status == "completed",
            "reused": True,
            "repairedTaskFence": repaired_task_fence,
            "publishedArtifacts": [],
            "notificationNeeded": {},
        }
        if task_meta:
            reused_result["task"] = task_meta
        if synced is not None:
            reused_result["synced"] = synced
            if not synced:
                return _sync_failure_result(reused_result, "accept_task_result")
        return reused_result
    changed = False
    for project_task in project.get("tasks", []):
        if project_task.get("task_id") == task_id:
            project_task["status"] = node_status
            changed = True
            break
    loop = project.get("loop") if isinstance(project.get("loop"), dict) else {}
    loop_tasks = loop.get("tasks", []) if isinstance(loop.get("tasks"), list) else []
    for project_task in loop_tasks:
        if project_task.get("task_id") == task_id:
            project_task["status"] = node_status
            project["loop"] = loop
            changed = True
            break
    if not changed:
        raise ValueError("task not found in project plan")
    # Accepting the result resolves ALL outstanding attention requests
    # on the task: the leader's decision closed the loop, so no room
    # ping on this task counts as an open loop anymore.  Resolve in place
    # on the task_meta dict — the terminal status write below persists it
    # in the same write (a separate read-modify-write here would be
    # clobbered by that later write of the older dict).
    if isinstance(task_meta, dict) and task_meta:
        attention = task_meta.get("attention")
        if isinstance(attention, list):
            for item in attention:
                if isinstance(item, dict) and not item.get("resolved"):
                    item["resolved"] = True
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
    try:
        _write_json(_project_state_path(arguments, project_id), project)
    except OSError as exc:
        if legacy_identity_persisted:
            return _persisted_state_failure_result(
                tool="projectflow",
                action="accept_task_result",
                error=exc,
                task=task_meta,
            )
        return _uncommitted_state_failure_result(
            tool="projectflow",
            action="accept_task_result",
            error=exc,
        )
    try:
        _write_project_plan(_project_dir(arguments, project_id), project)
    except OSError as exc:
        return _persisted_state_failure_result(
            tool="projectflow",
            action="accept_task_result",
            error=exc,
            task=task_meta or None,
            project=project,
        )
    if not _sync_project(arguments, project_id):
        return _sync_failure_result({
            "tool": "projectflow",
            "action": "accept_task_result",
            "project": project,
            "task": task_meta or None,
            "taskId": task_id,
            "submissionId": persisted_submission_id or None,
            "nodeStatus": node_status,
            "accepted": node_status == "completed",
            "publishedArtifacts": [],
            "notificationNeeded": {},
        }, "accept_task_result project")
    synced: bool | None = None
    if task_meta:
        # Gap fix (design G2): the task meta moves with the node.
        # (Idempotent re-accepts returned earlier; from == submitted in the
        # normal path — #1183's guard rejects anything else known.)
        _append_transition_history(
            task_meta,
            str(task_meta.get("status") or ""),
            node_status,
            "accept_task_result",
            _transition_actor(arguments),
        )
        task_meta["status"] = node_status
        _resolve_task_continuation(task_meta, node_status)
        try:
            _write_task(arguments, task_meta)
        except OSError as exc:
            return _persisted_state_failure_result(
                tool="projectflow",
                action="accept_task_result",
                error=exc,
                task=task_meta,
                project=project,
            )
        synced = _sync_task(arguments, task_id, exclude=["spec.md", "base/"])
    publish_artifacts = _payload_bool_field(payload, ("publishArtifacts", "publish_artifacts"), False)
    published_artifacts = (
        _publish_project_artifacts(
            arguments,
            project,
            project_id,
            task_id,
            _attachment_parent_event_id(payload, arguments),
        )
        if node_status == "completed" and publish_artifacts and synced is not False else []
    )
    requester_report = project.get("requester_report") if isinstance(project.get("requester_report"), dict) else {}
    requester_report_pending = requester_report.get("pending") is True and requester_report.get("task_id") == task_id
    result = {
        "ok": True,
        "tool": "projectflow",
        "action": "accept_task_result",
        "project": project,
        "task": task_meta or None,
        "taskId": task_id,
        "submissionId": persisted_submission_id or None,
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
    if synced is not None:
        result["synced"] = synced
        if not synced:
            return _sync_failure_result(result, "accept_task_result")
    # S2 signal emission (INT-3): SIG-COMPLETE after the main write succeeded.
    _emit_signal_event(
        arguments,
        "accept_task_result",
        task_id=task_id,
        project_id=str(project.get("project_id") or ""),
        ref=f"shared/tasks/{task_id}/meta.json",
        ref_bytes=_safe_read_bytes(_task_state_path(arguments, task_id)),
        payload={
            "project_id": str(project.get("project_id") or ""),
            "task_id": task_id,
            "node_status": node_status,
            "submission_id": persisted_submission_id or None,
        },
        audit_seq=_project_audit_seq(project),
    )
    return result


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


def _project_id_for_pull(arguments: dict[str, Any], payload: dict[str, Any]) -> str:
    """Resolve the project id for a projectflow/taskflow payload before the
    read-before-read pull.

    create_project/create_quick_project generate/validate a unique id, so the
    project does not yet exist — pull is meaningless (and would fail on the
    remote). Read-type actions carry projectId/project_id directly, except
    resolve_project which may carry only taskId (the task belongs to a
    project). Returns "" when the payload does not identify a project.
    """
    action = str(payload.get("action") or "").strip()
    if action in {"create_project", "create_quick_project"}:
        return ""
    project_id = _first_text(payload.get("projectId"), payload.get("project_id"))
    if project_id:
        return project_id
    task_id = _first_text(payload.get("taskId"), payload.get("task_id"))
    if task_id:
        task = _read_json(_task_state_path(arguments, task_id))
        if task:
            return _first_text(task.get("project_id"), task.get("projectId"))
    return ""


# Actions that write project/task state back to shared storage. When the
# authoritative pull fails, these must NOT continue from stale local state
# (their _sync would clobber a Controller pause/resume/replan) — they fail
# with a retryable error instead. Read-only actions tolerate a failed pull
# and serve local state.
_PROJECTFLOW_MUTATING_ACTIONS = frozenset({
    "accept_task_result",
    "record_loop_iteration",
    "pause_project",
    "resume_project",
    "complete_project",
    "replan_project",
})


def _projectflow(arguments: dict[str, Any]) -> dict[str, Any]:
    action = str(arguments.get("action") or "").strip()
    payload = _payload(arguments)
    try:
        # the lifecycle write API read-before-read: pull the authoritative meta.json from
        # shared storage before any read-type action so a Controller write
        # (pause/resume/replan) takes effect on the next read. One call at
        # the entry covers every read path (resolve_project, ready_nodes,
        # ready_loop_nodes, accept_task_result, record_loop_iteration,
        # pause/resume/complete, ...) instead of adding a pull per action.
        pid = _project_id_for_pull(arguments, payload)
        if pid and not _pull_project(arguments, pid):
            # Mutating actions must not continue from stale local state:
            # their write-back (_sync_project) could clobber a Controller
            # pause/resume/replan that this worker has not seen. Fail with a
            # retryable error; read actions tolerate the stale state.
            if action in _PROJECTFLOW_MUTATING_ACTIONS:
                return _pull_failed_response("projectflow", action)
        if action == "list_events":
            # Team-scoped read: no project id, so the pull block above is a
            # no-op for it (no projectId in the payload).
            return _list_events(arguments, payload)
        if action == "create_project":
            project_id = _project_id_from_payload(arguments, payload)
            project = {
                "project_id": project_id,
                "title": str(payload.get("title") or project_id),
                "source": str(payload.get("source") or ""),
                "requester": str(payload.get("requester") or ""),
                "team_id": str(payload.get("teamId") or payload.get("team_id") or _runtime_team_name()).strip(),
                "status": "active",
                "tasks": [],
            }
            reply_route = _normalize_reply_route(payload.get("replyRoute") or payload.get("reply_route"))
            if not reply_route:
                reply_route = _reply_route_from_requester(project["requester"])
            if reply_route:
                project["reply_route"] = reply_route
            source_room_id = _source_room_id_from_payload(payload, reply_route)
            if source_room_id:
                project["source_room_id"] = source_room_id
            project_dir = _project_dir(arguments, project_id)
            _write_json(_project_state_path(arguments, project_id), project)
            _write_project_plan(project_dir, project)
            _sync_project(arguments, project_id)
            return {
                "ok": True,
                "tool": "projectflow",
                "action": action,
                "project": project,
                "notificationNeeded": _notification_needed(action, project),
            }

        if action == "create_quick_project":
            project_id = _project_id_from_payload(arguments, payload)
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
            task_id = _safe_id(payload.get("taskId") or payload.get("task_id") or f"{project_id}-01", "taskId")
            if not task_id.startswith(f"{project_id}-"):
                raise ValueError("taskId must belong to projectId")
            if _task_state_path(arguments, task_id).exists():
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
                "team_id": str(payload.get("teamId") or payload.get("team_id") or _runtime_team_name()).strip(),
                "status": "active",
                "mode": "quick",
                "plan_type": "dag",
                "tasks": [task_node],
            }
            reply_route = _normalize_reply_route(payload.get("replyRoute") or payload.get("reply_route"))
            if not reply_route:
                reply_route = _reply_route_from_requester(project["requester"])
            if reply_route:
                project["reply_route"] = reply_route
            source_room_id = _source_room_id_from_payload(payload, reply_route)
            if source_room_id:
                project["source_room_id"] = source_room_id
                task_node["source_room_id"] = source_room_id
            _validate_assignment_room(project, room_id)
            project_dir = _project_dir(arguments, project_id)
            _write_json(_project_state_path(arguments, project_id), project)
            _write_project_plan(project_dir, project)
            _sync_project(arguments, project_id)

            task_dir = _task_dir(arguments, task_id)
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
            _write_task(arguments, task)
            return {
                "ok": True,
                "tool": "projectflow",
                "action": action,
                "project": project,
                "task": task,
                "synced": _sync_task(arguments, task_id),
                "notificationNeeded": _notification_needed(action, project, task),
            }

        if action == "resolve_project":
            return _resolve_project(arguments, payload)

        if action == "accept_task_result":
            return _accept_task_result(arguments, payload)

        if action == "mark_requester_report_sent":
            return _mark_requester_report_sent(arguments, payload)

        if action == "plan_dag":
            project_id = _safe_id(payload.get("projectId") or payload.get("project_id"), "projectId")
            state_path = _project_state_path(arguments, project_id)
            project = _read_json(state_path, {"project_id": project_id, "title": project_id, "status": "active", "tasks": []})
            previous = {task.get("task_id"): task for task in project.get("tasks", [])}
            raw_tasks = payload.get("tasks")
            if not isinstance(raw_tasks, list):
                raise ValueError("tasks must be a list")
            planned_tasks = _normalize_tasks(raw_tasks, previous)
            _validate_task_graph(planned_tasks)
            project["tasks"] = planned_tasks
            project["plan_type"] = "dag"
            project_dir = _project_dir(arguments, project_id)
            _write_json(state_path, project)
            _write_project_plan(project_dir, project)
            _sync_project(arguments, project_id)
            return {
                "ok": True,
                "tool": "projectflow",
                "action": action,
                "project": project,
                "readyNodes": _ready_nodes(project),
                "notificationNeeded": _notification_needed(action, project),
            }

        if action == "plan_loop":
            project_id = _safe_id(payload.get("projectId") or payload.get("project_id"), "projectId")
            state_path = _project_state_path(arguments, project_id)
            project = _read_json(state_path, {"project_id": project_id, "title": project_id, "status": "active", "tasks": []})
            previous_loop = project.get("loop") if isinstance(project.get("loop"), dict) else {}
            previous_tasks = {
                task.get("task_id"): task
                for task in (previous_loop.get("tasks", []) if isinstance(previous_loop.get("tasks"), list) else [])
            }
            raw_tasks = payload.get("tasks") or []
            if not isinstance(raw_tasks, list):
                raise ValueError("tasks must be a list")
            max_iterations = _positive_int(payload.get("maxIterations") or payload.get("max_iterations"), "maxIterations")
            current_iteration = _non_negative_int(
                payload.get("currentIteration") or payload.get("current_iteration") or previous_loop.get("current_iteration") or 0,
                "currentIteration",
            )
            if current_iteration > max_iterations:
                raise ValueError("currentIteration cannot exceed maxIterations")
            raw_task_id = payload.get("taskId") or payload.get("task_id") or previous_loop.get("task_id") or None
            task_id = _safe_id(raw_task_id, "taskId") if raw_task_id else None
            # Omitted fields keep the existing loop anchors, symmetric with
            # task_id above: a re-plan that does not re-declare the Loop CRD
            # identity must not silently drop the loop_id anchor that the
            # later reconciler (A<->B check) will compare against.
            raw_loop_id = (
                payload.get("loopId")
                or payload.get("loop_id")
                or previous_loop.get("loop_id")
                or None
            )
            loop_id = _safe_id(raw_loop_id, "loopId") if raw_loop_id else None
            planned_tasks = _normalize_tasks(raw_tasks, previous_tasks)
            _validate_task_graph(planned_tasks)
            loop = {
                "goal": str(payload.get("goal") or previous_loop.get("goal") or "").strip(),
                "stop_condition": str(payload.get("stopCondition") or payload.get("stop_condition") or previous_loop.get("stop_condition") or "").strip(),
                "iteration_template": str(payload.get("iterationTemplate") or payload.get("iteration_template") or previous_loop.get("iteration_template") or "").strip(),
                "max_iterations": max_iterations,
                "current_iteration": current_iteration,
                "status": _safe_loop_status(payload.get("status") or previous_loop.get("status") or "running"),
                "tasks": planned_tasks,
                "history": previous_loop.get("history", []) if isinstance(previous_loop.get("history"), list) else [],
            }
            if task_id:
                loop["task_id"] = task_id
            if loop_id:
                loop["loop_id"] = loop_id
            # Budget + iteration ledger start fresh on (re)plan:
            # iterations_used is len(loop["iterations"]) recomputed on
            # every record, so no stale counter survives a re-plan
            # (pre-ledger history kept above is re-absorbed by
            # record_loop_iteration).
            loop["budget"] = _loop_budget(project, None)
            loop["iterations"] = {}
            loop["provenance"] = []
            if not loop["goal"]:
                raise ValueError("goal is required")
            if not loop["stop_condition"]:
                raise ValueError("stopCondition is required")
            if not loop["iteration_template"]:
                raise ValueError("iterationTemplate is required")
            mismatch = _validate_loop_id_mapping(arguments, project_id, loop, payload)
            if mismatch:
                return mismatch
            project["plan_type"] = "loop"
            project["loop"] = loop
            project_dir = _project_dir(arguments, project_id)
            _write_json(state_path, project)
            _write_project_plan(project_dir, project)
            _sync_project(arguments, project_id)
            return {
                "ok": True,
                "tool": "projectflow",
                "action": action,
                "project": project,
                "loop": loop,
                "readyLoopNodes": _ready_loop_nodes(project),
                "notificationNeeded": _notification_needed(action, project),
            }

        if action == "ready_nodes":
            project_id = _safe_id(payload.get("projectId") or payload.get("project_id"), "projectId")
            project = _read_json(_project_state_path(arguments, project_id))
            if not project:
                raise ValueError("project not found")
            return {
                "ok": True,
                "tool": "projectflow",
                "action": action,
                "project": project,
                "readyNodes": _ready_nodes(project),
            }

        if action == "ready_loop_nodes":
            project_id = _safe_id(payload.get("projectId") or payload.get("project_id"), "projectId")
            project = _read_json(_project_state_path(arguments, project_id))
            if not project:
                raise ValueError("project not found")
            return {
                "ok": True,
                "tool": "projectflow",
                "action": action,
                "project": project,
                "loop": project.get("loop", {}),
                "readyLoopNodes": _ready_loop_nodes(project),
            }

        if action == "record_loop_iteration":
            project_id = _safe_id(payload.get("projectId") or payload.get("project_id"), "projectId")
            project = _read_json(_project_state_path(arguments, project_id))
            if not project:
                raise ValueError("project not found")
            loop = project.get("loop") if isinstance(project.get("loop"), dict) else {}
            if not loop:
                raise ValueError(f"project has no loop plan: {project_id}")
            iteration = _positive_int(payload.get("iteration"), "iteration")
            max_iterations = _positive_int(loop.get("max_iterations"), "maxIterations")
            if iteration > max_iterations:
                raise ValueError("iteration cannot exceed maxIterations")
            decision = _safe_loop_decision(payload.get("decision"))
            summary = str(payload.get("summary") or "").strip()
            next_action = str(payload.get("nextAction") or payload.get("next_action") or "").strip()
            # Iteration ledger: one entry per distinct iteration id.
            # Loops recorded before the ledger existed (or re-planned with
            # the ledger reset while history was kept) rebuild it from
            # history: same id collapses to the last entry, revision
            # counts the occurrences. iterations_used is
            # len(iterations) recomputed on every write below, which
            # self-heals the historical double count.
            history = loop.get("history") if isinstance(loop.get("history"), list) else []
            iterations = loop.get("iterations")
            if not isinstance(iterations, dict) or (not iterations and history):
                counts: dict[str, int] = {}
                iterations = {}
                for entry in history:
                    if not isinstance(entry, dict):
                        continue
                    entry_iteration = entry.get("iteration")
                    if not isinstance(entry_iteration, int) or isinstance(entry_iteration, bool):
                        continue
                    key = str(entry_iteration)
                    counts[key] = counts.get(key, 0) + 1
                    iterations[key] = {
                        "decision": str(entry.get("decision") or ""),
                        "summary": str(entry.get("summary") or ""),
                        "next_action": str(entry.get("next_action") or ""),
                        "revision": counts[key] - 1,
                        "first_at": None,
                        "updated_at": None,
                    }
                loop["iterations"] = iterations
            current_iteration = _non_negative_int(loop.get("current_iteration") or 0, "currentIteration")
            if iteration < current_iteration and str(iteration) not in iterations:
                raise ValueError("iteration_below_current")
            rec = iterations.get(str(iteration))
            if rec is None:
                now = _utc_timestamp()
                rec = {
                    "decision": decision,
                    "summary": summary,
                    "next_action": next_action,
                    "revision": 0,
                    "first_at": now,
                    "updated_at": now,
                }
                iterations[str(iteration)] = rec
                loop["current_iteration"] = max(current_iteration, iteration)
            elif rec["decision"] == decision and rec["summary"] == summary and rec["next_action"] == next_action:
                # Idempotent retry of the same operation: no state change,
                # no history/provenance entry, no signal, no disk write.
                return {
                    "ok": True,
                    "idempotent": True,
                    "revision": rec["revision"],
                    "project": project,
                    "loop": loop,
                    "readyLoopNodes": _ready_loop_nodes(project),
                }
            else:
                # Revision: the same iteration was re-decided. The ledger
                # entry (and only it) advances; the distinct-iteration
                # count stays len(iterations).
                rec["decision"] = decision
                rec["summary"] = summary
                rec["next_action"] = next_action
                rec["revision"] = _non_negative_int(rec.get("revision") or 0, "revision") + 1
                rec["updated_at"] = _utc_timestamp()
            loop["status"] = {
                "continue": "running",
                "replan": "running",
                "ask_user": "waiting_user",
                "stop_success": "completed",
                "stop_blocked": "blocked",
            }[decision]
            # Budget ledger: iterations_used is a view of the distinct
            # iteration ledger, recomputed on every write. No budget
            # (iteration_budget None): usage still accumulates and
            # over_budget stays False.
            budget = loop.get("budget")
            if not isinstance(budget, dict):
                budget = _loop_budget(project, budget)
            iterations_used = len(iterations)
            tokens_used = _non_negative_int(budget.get("tokens_used") or 0, "tokens_used")
            iteration_budget = budget.get("iteration_budget")
            if iteration_budget is not None:
                iteration_budget = _non_negative_int(iteration_budget, "iteration_budget")
            budget["iterations_used"] = iterations_used
            budget["tokens_used"] = tokens_used
            budget["over_budget"] = iteration_budget is not None and iterations_used > iteration_budget
            loop["budget"] = budget
            history.append({
                "iteration": iteration,
                "decision": decision,
                "summary": summary,
                "next_action": next_action,
                "revision": rec["revision"],
            })
            loop["history"] = history
            provenance = loop.get("provenance") if isinstance(loop.get("provenance"), list) else []
            provenance.append({
                "iteration": iteration,
                "revision": rec["revision"],
                "iterations_used": iterations_used,
                "iteration_budget": iteration_budget,
                "over_budget": budget["over_budget"],
            })
            loop["provenance"] = provenance[-LOOP_PROVENANCE_LIMIT:]
            project["plan_type"] = "loop"
            project["loop"] = loop
            project_dir = _project_dir(arguments, project_id)
            _write_json(_project_state_path(arguments, project_id), project)
            _write_project_plan(project_dir, project)
            _sync_project(arguments, project_id)
            # S2 signal emission (INT-3): after the main write succeeded.
            # loop events are project-level (task_id=""); loop_id is null on
            # the main baseline (no loop id field yet — schema allows null).
            loop_ref_bytes = _safe_read_bytes(_project_state_path(arguments, project_id))
            _emit_signal_event(
                arguments,
                "record_loop_iteration",
                task_id="",
                project_id=project_id,
                loop_id=None,
                iteration_index=iteration,
                ref=f"shared/projects/{project_id}/meta.json",
                ref_bytes=loop_ref_bytes,
                payload={
                    "project_id": project_id,
                    "iteration": iteration,
                    "revision": rec["revision"],
                    "decision": decision,
                    "loop_status": loop.get("status"),
                },
                audit_seq=_project_audit_seq(project),
            )
            verdict = payload.get("verdict")
            evidence_ref = str(payload.get("evidenceRef") or payload.get("evidence_ref") or "").strip()
            if (isinstance(verdict, dict) and verdict) or evidence_ref:
                _emit_signal_event(
                    arguments,
                    "record_loop_iteration:review",
                    task_id="",
                    project_id=project_id,
                    loop_id=None,
                    iteration_index=iteration,
                    ref=f"shared/projects/{project_id}/meta.json",
                    ref_bytes=loop_ref_bytes,
                    payload={
                        "project_id": project_id,
                        "iteration": iteration,
                        "verdict": verdict if isinstance(verdict, dict) else {},
                        "evidence_ref": evidence_ref,
                    },
                    audit_seq=_project_audit_seq(project),
                )
            return {
                "ok": True,
                "tool": "projectflow",
                "action": action,
                "project": project,
                "loop": loop,
                "readyLoopNodes": _ready_loop_nodes(project),
                "notificationNeeded": _notification_needed(
                    action, project, summary=f"record_loop_iteration: iteration {iteration} -> {decision}",
                ),
            }

        if action in {"pause_project", "resume_project", "complete_project"}:
            project_id = _safe_id(payload.get("projectId") or payload.get("project_id"), "projectId")
            state_path = _project_state_path(arguments, project_id)
            project = _read_json(state_path)
            if not project:
                raise ValueError("project not found")
            if action == "pause_project":
                project["status"] = "paused"
            elif action == "resume_project":
                project["status"] = "active"
            else:
                loop = project.get("loop") if isinstance(project.get("loop"), dict) else {}
                if str(project.get("plan_type") or "dag") == "loop":
                    tasks = loop.get("tasks", []) if isinstance(loop.get("tasks"), list) else []
                else:
                    tasks = project.get("tasks", []) if isinstance(project.get("tasks"), list) else []
                non_terminal = []
                for task in tasks:
                    if not isinstance(task, dict):
                        non_terminal.append("<unknown> (invalid)")
                        continue
                    status = str(task.get("status") or "")
                    if status not in TERMINAL_TASK_STATUSES:
                        non_terminal.append(f"{task.get('task_id') or '<unknown>'} ({status or 'unknown'})")
                if non_terminal:
                    raise ValueError(
                        "complete_project requires every project task to be terminal; "
                        f"not terminal: {', '.join(non_terminal)}"
                    )
                project["status"] = "completed"
                if loop:
                    loop["status"] = "completed"
                    project["loop"] = loop
            project_dir = _project_dir(arguments, project_id)
            _write_json(state_path, project)
            _write_project_plan(project_dir, project)
            # P0 ordering (PR review 2026-09-14): the terminal state is
            # persisted locally first, then pushed to shared storage, and
            # only after a successful sync does the PROJECT_COMPLETED
            # attention event go out. A failed sync withholds the event
            # and returns a retryable failure — the leader is never woken
            # by a completion whose state it cannot read, and no
            # projectCompletionEventId is recorded without durable state.
            synced = _sync_project(arguments, project_id)
            if action == "complete_project" and not synced:
                return {
                    "ok": False,
                    "retryable": True,
                    "tool": "projectflow",
                    "action": action,
                    "project": project,
                    "error": (
                        "shared storage sync failed after complete_project; the "
                        "PROJECT_COMPLETED notification was withheld. Local "
                        "project state is already completed — retry "
                        "complete_project (idempotent) once storage recovers."
                    ),
                }
            if action == "complete_project":
                # Code-level PROJECT_COMPLETED attention event (PR review
                # 2026-09-05): the leader and the human members see a
                # finished project in the room instead of waiting for the
                # next incident. Notification-level failures stay
                # best-effort — they never block the terminal project
                # write, which is already persisted and synced.
                project["projectNotification"] = _send_project_completion_notification(
                    arguments, project, project_id
                )
                if project.get("projectCompletionEventId"):
                    # Persist the recorded event id (and the notification
                    # result) so a retried complete_project reuses the
                    # event. If this write is lost, the stable txn id
                    # (project-<id>-success) makes the retry deduplicate
                    # at the homeserver and re-record the id.
                    _write_json(state_path, project)
            result = {
                "ok": True,
                "tool": "projectflow",
                "action": action,
                "project": project,
                "synced": synced,
            }
            if action == "complete_project":
                result["notification"] = project.get("projectNotification")
            publish_artifacts = _payload_bool_field(payload, ("publishArtifacts", "publish_artifacts"), False)
            if action == "complete_project" and publish_artifacts and (project_dir / "result.md").is_file():
                result["publishedArtifacts"] = _publish_project_artifacts(
                    arguments,
                    project,
                    project_id,
                    "",
                    _attachment_parent_event_id(payload, arguments),
                    completion_event_id=str(project.get("projectCompletionEventId") or ""),
                )
            result["notificationNeeded"] = _notification_needed(action, project)
            return result
    except ProtocolGuardError as exc:
        return {
            "ok": False,
            "tool": "projectflow",
            "action": action,
            "error_code": exc.error_code,
            "message": exc.message,
        }
    except ValueError as exc:
        return {"ok": False, "tool": "projectflow", "action": action, "error": str(exc)}

    return {"ok": False, "tool": "projectflow", "action": action, "error": f"unsupported action: {action}"}


