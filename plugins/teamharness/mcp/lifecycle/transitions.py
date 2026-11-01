# task state transitions, history append and transition errors (W1-a2: real module)
from __future__ import annotations

from typing import Any

# NOTE: cross-group imports are top-level absolutes (`common.*`), not
# `..common.*`: mcp/ sits on sys.path (server.py inserts it), so `common`
# resolves to the SAME module object in both `import server` (top-level)
# and `import mcp.server` (package) modes. Level-2 relatives would fail in
# the top-level mode the in-repo tests use.
from mcp.common import refs
from mcp.common.constants import TASK_HISTORY_LIMIT, TERMINAL_TASK_STATUSES, TRANSITIONS
from mcp.common.io import _read_json, _safe_read_bytes, _utc_timestamp
from mcp.common.paths import _project_dir, _project_state_path, _task_state_path
from .signal_events import _emit_signal_event

# Call-time lookups (common.refs): these names resolve against the bound
# mcp/server.py namespace, so test monkeypatches of server attributes stay
# visible to this module.
_sync_project = refs.proxy("_sync_project")
_write_json = refs.proxy("_write_json")
_write_project_plan = refs.proxy("_write_project_plan")
_role = refs.proxy("_role")


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


def _require_task_mutable(
    arguments: dict[str, Any],
    task: dict[str, Any],
    task_id: str,
    action: str,
    to_status: str = "",
) -> None:
    """Terminal-state guard for taskflow actions.

    Rejection uses the same structured surface as the table check
    (IllegalTransitionError + SIG-MISMATCH) so the public taskflow entry
    returns ``illegal_transition`` with from/to instead of a bare error
    string.  ``to_status`` is the status the action would have written
    ("" for status-preserving actions — the reported to equals from).
    """
    terminal_status = _terminal_task_status(arguments, task, task_id)
    if terminal_status:
        _reject_task_transition(
            terminal_status,
            to_status or terminal_status,
            action,
            (
                f"{action} cannot update terminal task: {terminal_status} "
                f"(task is in terminal state {terminal_status})"
            ),
            arguments=arguments,
            task_id=task_id,
        )


def _update_project_task(arguments: dict[str, Any], project_id: str, task_id: str, **updates: Any) -> bool:
    path = _project_state_path(arguments, project_id)
    project = _read_json(path)
    if not project:
        return False
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
    return _sync_project(arguments, project_id)


refs.define("_update_project_task", _update_project_task)


def _transition_actor(arguments: dict[str, Any]) -> str:
    """history actor: ``role:account_id`` (precedent: ``_ack_`` role stamping)."""
    role = _role(arguments) or "unknown"
    account = str(arguments.get("account_id") or "default").strip() or "default"
    return f"{role}:{account}"


def _transition_guidance(action: str, from_status: str) -> str:
    if action in ("ack_task", "submit_task"):
        if from_status in ("", "planned", "prepared"):
            return "delegate_task it first" if action == "ack_task" else "ack_task it first"
        return "wait for a leader decision (accept_task_result)"
    if action == "accept_task_result":
        return "wait for submit_task"
    if action == "delegate_task":
        return "cancel_task it first, or wait for the submission to be decided"
    return "check the task transition table (task-transitions.json)"


class IllegalTransitionError(ValueError):
    """Structured illegal-transition rejection (INT-5).

    Carries the from/to/action triple so the taskflow dispatch layer can
    return a structured error; a ValueError subclass so the internal
    protocol (call sites, OSError-only inner catches) is unchanged.
    """

    def __init__(self, from_status: str, to_status: str, action: str, message: str) -> None:
        super().__init__(message)
        self.from_status = from_status
        self.to_status = to_status
        self.action = action


def _assert_transition(
    from_status: str,
    to_status: str,
    action: str,
    *,
    arguments: dict[str, Any] | None = None,
    task_id: str = "",
) -> None:
    """Table check.  Same-state re-entry is a legal no-op (idempotent retry).

    Rejections raise IllegalTransitionError with an ``illegal_transition:``
    message; terminal states and unknown states get distinct messages.  When
    ``arguments`` + ``task_id`` are provided the rejection also emits
    SIG-MISMATCH (best-effort — ``_emit_signal_event`` never raises).
    """
    if from_status == to_status:
        return
    if from_status in TERMINAL_TASK_STATUSES:
        message = (
            f"illegal_transition: {action}: task is in terminal state {from_status} "
            f"(immutable); cannot transition to '{to_status}'"
        )
    else:
        allowed = TRANSITIONS.get(from_status)
        if allowed is None:
            message = (
                f"illegal_transition: {action}: task is in unexpected state "
                f"{from_status or 'missing'}; {_transition_guidance(action, from_status)}"
            )
        elif to_status not in allowed:
            message = (
                f"illegal_transition: {action}: task is '{from_status}'; "
                f"{_transition_guidance(action, from_status)} "
                f"(allowed transitions from '{from_status}': {', '.join(allowed)})"
            )
        else:
            return
    if arguments is not None and task_id:
        _emit_signal_event(
            arguments,
            "illegal_transition_rejected",
            task_id=task_id,
            ref=f"shared/tasks/{task_id}/meta.json",
            ref_bytes=_safe_read_bytes(_task_state_path(arguments, task_id)),
            payload={
                "from_status": from_status,
                "to_status": to_status,
                "action": action,
            },
            discriminator=f"{action}|{from_status}|{to_status}",
        )
    raise IllegalTransitionError(from_status, to_status, action, message)


def _reject_task_transition(
    from_status: str,
    to_status: str,
    action: str,
    message: str,
    *,
    arguments: dict[str, Any] | None = None,
    task_id: str = "",
) -> None:
    """Action-level guard rejection (pre-``_transition_task`` checks:
    terminal immutability, ack/submit/progress ordering, cancel).

    Same structured surface as the table check: raises
    IllegalTransitionError so the taskflow dispatch returns
    ``illegal_transition`` with from/to, and emits SIG-MISMATCH when the
    task is named.  The TRANSITIONS table itself is untouched — these
    guards encode action-level rules stricter than the table (e.g. ack on
    a planned task is rejected even though planned -> in_progress is a
    legal table edge reached via delegate_task).
    """
    if arguments is not None and task_id:
        _emit_signal_event(
            arguments,
            "illegal_transition_rejected",
            task_id=task_id,
            ref=f"shared/tasks/{task_id}/meta.json",
            ref_bytes=_safe_read_bytes(_task_state_path(arguments, task_id)),
            payload={
                "from_status": from_status,
                "to_status": to_status,
                "action": action,
            },
            discriminator=f"{action}|{from_status}|{to_status}",
        )
    raise IllegalTransitionError(from_status, to_status, action, message)


def _append_transition_history(
    task: dict[str, Any],
    from_status: str,
    to_status: str,
    action: str,
    actor: str,
    note: str = "",
    record_noop: bool = False,
) -> None:
    """Append a transition entry to task meta ``history`` (cap
    TASK_HISTORY_LIMIT, dropping the oldest).  Mutates in place; the caller
    persists.  No-op re-entries are not recorded unless ``record_noop``
    (report_progress uses from == to on purpose).

    Each entry carries ``seq``, the per-task monotonic sequence stored in
    the task meta as ``history_seq``: timestamps are second-resolution and
    repeated progress entries are allowed, so content cannot identify an
    event, and the reader's /events cursor needs a stable identity that
    survives cap truncation.  Legacy entries predating the field are
    backfilled in list order on this write (append order is stable)."""
    if from_status == to_status and not record_noop:
        return
    history = task.get("history")
    if not isinstance(history, list):
        history = []
    # Stable event identity for the /events cursor (see docstring): the
    # counter lives on the task meta, so it survives history truncation.
    try:
        seq = int(task.get("history_seq") or 0)
    except (TypeError, ValueError):
        seq = 0
    for existing in history:
        if isinstance(existing, dict) and existing.get("seq") is None:
            seq += 1
            existing["seq"] = seq
    seq += 1
    task["history_seq"] = seq
    entry: dict[str, Any] = {
        "ts": _utc_timestamp(),
        "from": from_status,
        "to": to_status,
        "actor": actor,
        "action": action,
        "seq": seq,
    }
    if note:
        entry["note"] = note
    history.append(entry)
    if len(history) > TASK_HISTORY_LIMIT:
        history = history[-TASK_HISTORY_LIMIT:]
    task["history"] = history
