# delegate/completion/attention notification sending (W1-a2: real module)
from __future__ import annotations

import json

import os

import uuid

from typing import Any

import urllib.error

import urllib.parse

import urllib.request

# Cross-group imports are top-level absolutes (see lifecycle/transitions.py
# note): mcp/ on sys.path keeps one module identity in both load modes.
from mcp.common import refs
from mcp.common.identity import _canonical_room_id
from mcp.common.io import _read_json
from mcp.common.paths import _task_state_path
from .matrix import _matrix_room_member_user_ids, _team_human_matrix_ids, _team_leader_matrix_id
from .mention import MATRIX_ROOM_RE, _matrix_content

# Monkeypatch-sensitive (15-name set): resolve at call time so a test patch
# of server._write_task stays visible to this module's call sites.
_write_task = refs.proxy("_write_task")


def _validate_assignee_membership(room_id: str, assignee: str) -> dict[str, Any]:
    """Validate that the assignee is a joined member of the target Matrix room.

    Strict when the Matrix environment is configured and the assignee is a
    Matrix mxid (``@user:server``): a missing membership is a retryable
    failure so delegation never marks a task assigned when the Worker cannot
    receive the automatic notification. Skipped when the Matrix environment is
    unavailable (the notification would be skipped anyway) or the assignee is a
    display name (there is no mxid to verify).
    """
    homeserver = os.getenv("AGENTTEAMS_MATRIX_URL", "").strip()
    token = os.getenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "").strip()
    if not homeserver or not token:
        return {"ok": True, "skipped": "matrix env unavailable"}
    matrix_room_id = _canonical_room_id(room_id)
    if not matrix_room_id.startswith("!"):
        return {"ok": True, "skipped": f"non-Matrix room target: {room_id}"}
    assignee_mxid = str(assignee or "").strip()
    if not assignee_mxid.startswith("@"):
        return {"ok": True, "skipped": f"assignee is a display name, not an mxid: {assignee}"}
    try:
        members = _matrix_room_member_user_ids(matrix_room_id, {"join"})
    except (ValueError, urllib.error.HTTPError, urllib.error.URLError, TimeoutError, OSError, json.JSONDecodeError) as exc:
        return {"ok": False, "error": f"cannot verify room membership: {exc}"}
    if assignee_mxid in members:
        return {"ok": True, "member": True}
    return {
        "ok": False,
        "error": f"assignee {assignee_mxid} is not a joined member of room {matrix_room_id}",
    }


# Monkeypatch-sensitive (15-name set): register the real object, then rebind
# the module-level name to a proxy — _task_completion_notification and
# _send_project_completion_notification call it from module scope, so the
# rebind is what makes a test patch of server._validate_assignee_membership
# visible at those call sites.
refs.define("_validate_assignee_membership", _validate_assignee_membership)
_validate_assignee_membership = refs.proxy("_validate_assignee_membership")


def _send_delegate_notification(
    arguments: dict[str, Any],
    *,
    room_id: str,
    task_id: str,
    title: str,
    assignee: str,
    spec: str,
    txn: str | None = None,
) -> dict[str, Any]:
    """Send the automatic Worker assignment notification for delegate_task.

    Publishes the assignment to the Task room with ``m.mentions`` using the
    same Matrix HTTP send path as the message tool.

    Transaction id: the caller passes the **persisted** transaction id for
    this logical delegation attempt (``delegate-{task_id}-{nonce}``, stored
    on the task BEFORE the send). A retry of the same attempt reuses it, so
    a send that reached the room but whose response was lost is deduplicated
    server-side instead of emitting a second assignment event. A genuinely
    new delegation attempt (a different assignee, including A->B->A) rotates
    the nonce, so its notification is never swallowed by a stale id. When
    ``txn`` is omitted, a fresh id is generated for this send only.
    Returns the Matrix ``eventId`` on success.
    """
    homeserver = os.getenv("AGENTTEAMS_MATRIX_URL", "").rstrip("/")
    token = os.getenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "")
    if not homeserver or not token:
        return {
            "sent": False,
            "error": "AGENTTEAMS_MATRIX_URL and AGENTTEAMS_WORKER_MATRIX_TOKEN are required",
        }

    matrix_room_id = str(room_id or "").strip()
    if matrix_room_id.startswith("room:"):
        matrix_room_id = matrix_room_id[len("room:") :].strip()
    if not matrix_room_id.startswith("!"):
        return {"sent": False, "error": f"invalid Matrix room target: {room_id}"}

    spec_preview = (spec or "")[:500]
    if len(spec or "") > 500:
        spec_preview += "..."
    notification_text = (
        f"{assignee} You are assigned task **{task_id}**: {title}\n\n"
        f"{spec_preview}"
    )
    mentions = [assignee]
    content = _matrix_content(notification_text, mentions)

    room_enc = urllib.parse.quote(matrix_room_id, safe="")
    # Persisted transaction id for this delegation attempt (see docstring):
    # the caller pre-mints it so a retry after a lost response reuses the
    # same id and the homeserver deduplicates instead of double-sending.
    if not txn:
        txn = f"delegate-{task_id}-{uuid.uuid4().hex[:12]}"
    txn = urllib.parse.quote(txn, safe="")
    url = f"{homeserver}/_matrix/client/v3/rooms/{room_enc}/send/m.room.message/{txn}"
    request = urllib.request.Request(
        url,
        data=json.dumps(content).encode("utf-8"),
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
        },
        method="PUT",
    )
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            data = json.loads(response.read().decode("utf-8") or "{}")
        event_id = str(data.get("event_id") or "").strip()
        if not event_id:
            return {"sent": False, "error": "Matrix send returned no event_id"}
        return {
            "sent": True,
            "eventId": event_id,
            "roomId": matrix_room_id,
            "assignee": assignee,
        }
    except urllib.error.HTTPError as exc:
        body = exc.read().decode("utf-8", errors="replace")[:200]
        return {
            "sent": False,
            "error": f"Matrix API error: HTTP {exc.code}: {body}",
        }
    except (urllib.error.URLError, TimeoutError, OSError) as exc:
        return {"sent": False, "error": f"Matrix API error: {exc}"}


# Monkeypatch-sensitive (15-name set): no self-calls in this module, so
# define without rebind (call sites elsewhere keep their own proxies).
refs.define("_send_delegate_notification", _send_delegate_notification)


_TASK_COMPLETION_EVENT_TOKENS = {
    "SUCCESS": "TASK_COMPLETED",
    "SUCCESS_WITH_NOTES": "TASK_COMPLETED",
    "REVISION_NEEDED": "TASK_REVISION_NEEDED",
    "BLOCKED": "TASK_BLOCKED",
    "INTERRUPTED": "TASK_INTERRUPTED",
    # Defensive only — #1183 removed PARTIAL / FAILED from the accepted
    # result-status set (no acceptance mapping). The entries stay so a
    # future re-adding of those statuses emits the right first line.
    "PARTIAL": "TASK_PARTIAL",
    "FAILED": "TASK_FAILED",
}


def _send_project_completion_notification(
    arguments: dict[str, Any],
    project: dict[str, Any],
    project_id: str,
) -> dict[str, Any]:
    """Send the automatic PROJECT_COMPLETED event for complete_project.

    The caller (complete_project) invokes this only after the terminal
    state has been persisted locally and synced to shared storage — a
    failed sync withholds the event at the caller and returns a
    retryable failure instead, so this function never sends an event
    for state that shared storage does not have. Notification-level
    failures (no room, no leader, missing Matrix env, membership check,
    HTTP error) return a ``skipped`` result and never block the
    terminal project write, which is already persisted and synced. The
    event mentions the team leader and the human members in the task
    room so the requester sees project completion with the same
    salience as a task completion. On a successful send the
    projectCompletionEventId is recorded on the project object; the
    caller persists it, and a retried complete_project reuses the
    recorded event instead of pinging the room again.
    """
    leader = _team_leader_matrix_id()
    if not leader:
        return {
            "sent": False,
            "skipped": True,
            "error": "team leader Matrix ID not found in runtime config",
        }
    if project.get("projectCompletionEventId"):
        return {
            "sent": True,
            "eventId": str(project["projectCompletionEventId"]),
            "leader": leader,
            "reused": True,
        }
    room_id = ""
    tasks = project.get("tasks")
    if isinstance(tasks, list):
        for task in tasks:
            if not isinstance(task, dict):
                continue
            room_id = str(task.get("room_id") or "").strip()
            if not room_id:
                task_id = str(task.get("task_id") or task.get("taskId") or "").strip()
                if task_id:
                    task_state = _read_json(_task_state_path(arguments, task_id), {})
                    room_id = str(task_state.get("room_id") or "").strip()
            if room_id:
                break
    if not room_id:
        source_room_id = str(project.get("source_room_id") or "").strip()
        if MATRIX_ROOM_RE.fullmatch(_canonical_room_id(source_room_id)):
            room_id = source_room_id
    if not room_id:
        return {"sent": False, "skipped": True, "error": "project has no room_id"}
    membership = _validate_assignee_membership(room_id, leader)
    if not membership.get("ok"):
        return {
            "sent": False,
            "skipped": True,
            "error": str(membership.get("error") or "room membership check failed"),
        }
    notification = _send_task_completion_notification(
        room_id=room_id,
        task_id=project_id,
        status="SUCCESS",
        summary=f"Project completed: {str(project.get('title') or project_id)[:200]}",
        leader=leader,
        humans=_team_human_matrix_ids(),
        event_token="PROJECT_COMPLETED",
        txn_prefix="project",
    )
    if notification.get("sent"):
        # Recorded only after a successful send (the P0 ordering in
        # complete_project guarantees the state is durable first); the
        # caller persists it on the project state.
        project["projectCompletionEventId"] = notification.get("eventId")
    return notification


def _send_task_completion_notification(
    *,
    room_id: str,
    task_id: str,
    status: str,
    summary: str,
    leader: str,
    worker: str = "",
    result_path: str = "",
    humans: list[str] | None = None,
    event_token: str = "",
    txn_prefix: str = "submit",
) -> dict[str, Any]:
    """Send the automatic Worker completion notification for submit_task.

    Publishes the completion event to the Task room with ``m.mentions``
    using the same Matrix HTTP send path as the message tool. Every
    accepted result status gets its own first-line contract token so
    leader-side prompts can branch on the line itself (task-execution
    skill contract; #1183's accepted set):
        @leader TASK_COMPLETED: <task-id> - Result: shared/tasks/<task-id>/result.md
        @leader TASK_REVISION_NEEDED: <task-id> - <summary>
        @leader TASK_BLOCKED: <task-id> - <short blocker summary>
        @leader TASK_INTERRUPTED: <task-id> - <summary>
    PARTIAL / FAILED keep defensive map entries (TASK_PARTIAL /
    TASK_FAILED): #1183 removed them from the accepted set, so a
    re-adding there emits the right first line without a follow-up
    change here.
    Humans (task initiator and other non-agent members) are mentioned
    alongside the leader so the requester is routed with the same
    salience the leader is. The transaction ID is stable per task and
    status, and the recorded event id is reused on an exact retry, so
    the event is sent exactly once (the durable-continuation digest
    fence rejects a resubmission with a changed result).
    """
    homeserver = os.getenv("AGENTTEAMS_MATRIX_URL", "").rstrip("/")
    token = os.getenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "")
    if not homeserver or not token:
        return {
            "sent": False,
            "error": "AGENTTEAMS_MATRIX_URL and AGENTTEAMS_WORKER_MATRIX_TOKEN are required",
        }

    matrix_room_id = str(room_id or "").strip()
    if matrix_room_id.startswith("room:"):
        matrix_room_id = matrix_room_id[len("room:") :].strip()
    if not matrix_room_id.startswith("!"):
        return {"sent": False, "error": f"invalid Matrix room target: {room_id}"}

    summary_preview = (summary or "")[:500]
    if len(summary or "") > 500:
        summary_preview += "..."
    line_token = event_token or _TASK_COMPLETION_EVENT_TOKENS.get(status, f"TASK_{status}")
    if event_token:
        notification_text = f"{leader} {event_token}: {task_id} - {summary_preview}".rstrip()
    elif line_token == "TASK_COMPLETED":
        if result_path:
            notification_text = f"{leader} TASK_COMPLETED: {task_id} - Result: {result_path}"
        elif summary_preview:
            notification_text = f"{leader} TASK_COMPLETED: {task_id} - {summary_preview}"
        else:
            notification_text = f"{leader} TASK_COMPLETED: {task_id}"
    else:
        notification_text = f"{leader} {line_token}: {task_id} - {summary_preview}".rstrip()
    if worker:
        notification_text += f"\n- Worker: {worker}"
    if not event_token and line_token != "TASK_COMPLETED":
        notification_text += f"\n- Status: {status}"
    elif (
        line_token == "TASK_COMPLETED"
        and not event_token
        and result_path
        and summary_preview
    ):
        # The Result line names the file; keep the short summary preview
        # as the detail line (legacy contract, leader-side prompts read it).
        notification_text += f"\n{summary_preview}"
    mentions = [leader]
    for human in humans or []:
        if human and human != leader and human not in mentions:
            mentions.append(human)
    content = _matrix_content(notification_text, mentions)

    room_enc = urllib.parse.quote(matrix_room_id, safe="")
    txn = urllib.parse.quote(f"{txn_prefix}-{task_id}-{status.lower()}", safe="")
    url = f"{homeserver}/_matrix/client/v3/rooms/{room_enc}/send/m.room.message/{txn}"
    request = urllib.request.Request(
        url,
        data=json.dumps(content).encode("utf-8"),
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
        },
        method="PUT",
    )
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            data = json.loads(response.read().decode("utf-8") or "{}")
        event_id = str(data.get("event_id") or "").strip()
        if not event_id:
            return {"sent": False, "error": "Matrix send returned no event_id"}
        return {
            "sent": True,
            "eventId": event_id,
            "roomId": matrix_room_id,
            "leader": leader,
            "humans": mentions[1:],
        }
    except urllib.error.HTTPError as exc:
        body = exc.read().decode("utf-8", errors="replace")[:200]
        return {
            "sent": False,
            "error": f"Matrix API error: HTTP {exc.code}: {body}",
        }
    except (urllib.error.URLError, TimeoutError, OSError) as exc:
        return {"sent": False, "error": f"Matrix API error: {exc}"}


def _send_attention_notification(
    arguments: dict[str, Any],
    *,
    room_id: str,
    task_id: str,
    kind: str,
    question: str,
    leader: str,
    worker: str = "",
    humans: list[str] | None = None,
    attempt: int = 1,
) -> dict[str, Any]:
    """Send an in-flight attention request to the Task room.

    Workers/leaders use this to pull a human decision (approval,
    decision, escalation) out of the ambient group chat: the event is a
    status-scoped first-line token (``ATTENTION_APPROVAL`` etc.) that
    mentions the leader and the human members of the team. The
    transaction ID is stable per task, kind and attempt so a retry
    cannot duplicate the event while a new attempt produces a new one.
    """
    homeserver = os.getenv("AGENTTEAMS_MATRIX_URL", "").rstrip("/")
    token = os.getenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "")
    if not homeserver or not token:
        return {
            "sent": False,
            "error": "AGENTTEAMS_MATRIX_URL and AGENTTEAMS_WORKER_MATRIX_TOKEN are required",
        }

    matrix_room_id = str(room_id or "").strip()
    if matrix_room_id.startswith("room:"):
        matrix_room_id = matrix_room_id[len("room:") :].strip()
    if not matrix_room_id.startswith("!"):
        return {"sent": False, "error": f"invalid Matrix room target: {room_id}"}

    question_preview = (question or "")[:500]
    if len(question or "") > 500:
        question_preview += "..."
    notification_text = f"{leader} ATTENTION_{kind.upper()}: {task_id} - {question_preview}".rstrip()
    if worker:
        notification_text += f"\n- Worker: {worker}"
    mentions = [leader]
    for human in humans or []:
        if human and human != leader and human not in mentions:
            mentions.append(human)
    content = _matrix_content(notification_text, mentions)

    room_enc = urllib.parse.quote(matrix_room_id, safe="")
    txn = urllib.parse.quote(f"attention-{task_id}-{kind}-{attempt}", safe="")
    url = f"{homeserver}/_matrix/client/v3/rooms/{room_enc}/send/m.room.message/{txn}"
    request = urllib.request.Request(
        url,
        data=json.dumps(content).encode("utf-8"),
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
        },
        method="PUT",
    )
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            data = json.loads(response.read().decode("utf-8") or "{}")
        event_id = str(data.get("event_id") or "").strip()
        if not event_id:
            return {"sent": False, "error": "Matrix send returned no event_id"}
        return {
            "sent": True,
            "eventId": event_id,
            "roomId": matrix_room_id,
            "leader": leader,
            "humans": mentions[1:],
        }
    except urllib.error.HTTPError as exc:
        body = exc.read().decode("utf-8", errors="replace")[:200]
        return {
            "sent": False,
            "error": f"Matrix API error: HTTP {exc.code}: {body}",
        }
    except (urllib.error.URLError, TimeoutError, OSError) as exc:
        return {"sent": False, "error": f"Matrix API error: {exc}"}


def _task_completion_notification(
    arguments: dict[str, Any],
    task: dict[str, Any],
    task_id: str,
    status: str,
    summary: str,
) -> dict[str, Any]:
    """Best-effort completion notification orchestration for submit_task.

    Mirrors the delegate_task notification lifecycle: resolve the leader
    from the runtime config, reuse an already-recorded event on retry,
    validate room membership, send, then persist the event id so the
    notification cannot be duplicated. Any problem returns a skipped
    result and never blocks the terminal submission.
    """
    room_id = str(task.get("room_id") or "").strip()
    if not room_id:
        return {"sent": False, "skipped": True, "error": "task has no room_id"}
    leader = _team_leader_matrix_id()
    if not leader:
        return {
            "sent": False,
            "skipped": True,
            "error": "team leader Matrix ID not found in runtime config",
        }
    recorded_status = str(task.get("completionEventStatus") or "")
    if task.get("completionEventId"):
        if not recorded_status or recorded_status == status:
            return {
                "sent": True,
                "eventId": str(task["completionEventId"]),
                "roomId": _canonical_room_id(room_id),
                "leader": leader,
                "reused": True,
                "completionStatus": recorded_status or status,
            }
        # A recorded completion exists but the task is being re-submitted
        # with a changed result status: the recorded event is stale.
        # Invalidate it so the new status-scoped transaction can send a
        # fresh event instead of silently reusing the old one.
        task.pop("completionEventId", None)
        task.pop("completionEventStatus", None)
        _write_task(arguments, task)
    membership = _validate_assignee_membership(room_id, leader)
    if not membership.get("ok"):
        return {
            "sent": False,
            "skipped": True,
            "error": str(membership.get("error") or "room membership check failed"),
        }
    notification = _send_task_completion_notification(
        room_id=room_id,
        task_id=task_id,
        status=status,
        summary=summary,
        leader=leader,
        worker=str(task.get("assigned_to") or ""),
        result_path=str(task.get("result_path") or ""),
        humans=_team_human_matrix_ids(),
    )
    if notification.get("sent"):
        # If the persist below fails, the event id is lost but the
        # transaction id is stable per task + status, so a retried
        # submission re-puts the same Matrix txn and the homeserver
        # deduplicates it — no duplicate event, the id is re-recorded.
        task["completionEventId"] = notification.get("eventId")
        task["completionEventStatus"] = status
        _write_task(arguments, task)
    return notification


