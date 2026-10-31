"""Task state transitions, history append and transition errors
(W1-a2: real module), plus the W2 protocol sub-state (issue #1451 W2):
decision_status overlays the nine runtime states and is written only by
the protocol layer, through the single writer _set_decision_status.

Frozen is an absorbing state (issue #1467): it is left only by an
authorized approval close (frozen -> open through the kind=approval,
trusted-human path — the only call that carries
unfreeze_authority='approval'). Every other writer — a stall
escalation, a non-approval resolve, a tool close, any future writer —
is refused at that single writer: the state stays frozen and the
attempt is audited as a refused decision_status_history entry
(from='frozen', to='frozen', refused=<attempted status>). The refusal
is a state invariant, not a policy gate: it holds with the protocol
switch off too."""
from __future__ import annotations

from typing import Any

# NOTE: cross-group imports are top-level absolutes (`common.*`), not
# `..common.*`: mcp/ sits on sys.path (server.py inserts it), so `common`
# resolves to the SAME module object in both `import server` (top-level)
# and `import mcp.server` (package) modes. Level-2 relatives would fail in
# the top-level mode the in-repo tests use.
from mcp.common import refs
from mcp.common.constants import (
    ATTENTION_STALL_BLOCKED,
    ATTENTION_STALL_FROZEN,
    DECISION_STATUS_VALUES,
    DEFAULT_DECISION_STATUS,
    TASK_HISTORY_LIMIT,
    TERMINAL_TASK_STATUSES,
    TRANSITIONS,
)
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
# The trusted runtime member identity (server.py): it reads the
# operator-set environment / runtime config, never the tool payload, so
# a caller cannot spoof it. Call-time proxy so tests can monkeypatch it.
_runtime_identity = refs.proxy("_runtime_identity")


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
    """history actor: ``role:account`` (precedent: ``_ack_`` role stamping).

    The account is the trusted runtime member identity
    (server._runtime_identity: the operator-set AGENTTEAMS_WORKER_NAME
    env / runtime config member block), never a payload field. The
    payload's ``account_id`` is provenance only: it is used when no
    trusted identity exists (local / test runs) and can never override
    one (#1467 round 6: the identity used for an authorization decision
    comes from the runtime, and two different members must never
    collapse into one identity)."""
    role = _role(arguments) or "unknown"
    account = _runtime_identity()
    if not account:
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


# W2 protocol sub-state (issue #1451 W2): decision_status overlays the
# nine runtime states; only the protocol layer writes it. The attention
# stall chain is the convergence point that writes "blocked"; "frozen"
# is an absorbing state (issue #1467) — released only through the
# approval channel (the resolved kind=approval attention record, the
# trusted-human path), and every other writer is refused at the single
# writer below, with the refusal audited in decision_status_history.
def _decision_status_of(task: dict[str, Any]) -> str:
    """Read the decision_status sub-state, defaulting to 'open' for tasks
    written before the W2 protocol layer existed."""
    status = str(task.get("decision_status") or "").strip()
    return status if status in DECISION_STATUS_VALUES else DEFAULT_DECISION_STATUS


def _set_decision_status(
    task: dict[str, Any],
    new_status: str,
    *,
    reason: str,
    source: str,
    actor: str = "",
    unfreeze_authority: str = "",
) -> str:
    """Write the decision_status sub-state; 'frozen' is absorbing.

    A history entry is appended when the task enters blocked/frozen, or
    leaves them back to open (decision_status_history: ts / from / to /
    reason / source, plus actor when known).

    Frozen is an absorbing state (#1467): the only exit is an authorized
    approval close — new_status 'open' with unfreeze_authority
    'approval'. Every other attempt to leave or demote a frozen task
    (a stall re-escalation, a non-approval resolve, a tool close, any
    future writer) is refused here, at the single writer: the state
    stays 'frozen' and the attempt is audited as a refused history
    entry (from='frozen', to='frozen', refused=<the attempted status>).
    The refusal is a state invariant, not a policy gate — it holds with
    the protocol switch off too.

    Returns the effective status ('frozen' on a refusal) so callers can
    report what actually happened. Mutates in place; the caller
    persists the task."""
    if new_status not in DECISION_STATUS_VALUES:
        raise ValueError(f"unknown decision_status: {new_status}")
    current = _decision_status_of(task)
    if current == new_status:
        return current
    if current == "frozen" and (
        new_status != "open" or unfreeze_authority != "approval"
    ):
        # Refuse the exit/demotion: keep the state and audit the
        # attempt — no phantom transition enters the history.
        history = task.get("decision_status_history")
        if not isinstance(history, list):
            history = []
        entry: dict[str, Any] = {
            "ts": _utc_timestamp(),
            "from": current,
            "to": current,
            "refused": new_status,
            "reason": reason,
            "source": source,
        }
        if actor:
            entry["actor"] = actor
        history.append(entry)
        task["decision_status_history"] = history
        return current
    task["decision_status"] = new_status
    if new_status in ("blocked", "frozen") or (
        current in ("blocked", "frozen") and new_status == "open"
    ):
        history = task.get("decision_status_history")
        if not isinstance(history, list):
            history = []
        entry: dict[str, Any] = {
            "ts": _utc_timestamp(),
            "from": current,
            "to": new_status,
            "reason": reason,
            "source": source,
        }
        if actor:
            entry["actor"] = actor
        history.append(entry)
        task["decision_status_history"] = history
    return _decision_status_of(task)


def _attention_stall_escalate(task: dict[str, Any], record: dict[str, Any]) -> str:
    """Escalate decision_status after a stall increment: >=
    ATTENTION_STALL_FROZEN -> frozen, >= ATTENTION_STALL_BLOCKED ->
    blocked (monotonic: frozen never demotes to blocked). Only the
    protocol layer's stall chain writes these values; sub-states set by
    other writers (paused / spec_conflict) are left alone.

    Frozen is absorbing (#1467): a stalled record on a frozen task
    short-circuits — it can neither demote it (frozen -> blocked) nor
    change anything else. The choke point in _set_decision_status is
    the real guard for any other writer; the short-circuit keeps the
    intent explicit and the history clean (no refusal entry from the
    stall path, which is expected to fire on every retry)."""
    current = _decision_status_of(task)
    if current not in ("open", "blocked", "frozen"):
        return current
    if current == "frozen":
        return "frozen"
    stall = int(record.get("stall_count") or 0)
    if stall >= ATTENTION_STALL_FROZEN:
        target = "frozen"
    elif stall >= ATTENTION_STALL_BLOCKED:
        target = "blocked"
    else:
        return current
    if current == target:
        return current
    _set_decision_status(
        task,
        target,
        reason=f"attention stalled {stall} times without an answer",
        source=f"attention:{str(record.get('kind') or 'unknown')}",
    )
    return _decision_status_of(task)


def _attention_resolved_status(
    task: dict[str, Any],
    record: dict[str, Any],
    *,
    actor: str = "",
    unfreeze_authority: str = "",
) -> None:
    """Zero the stall counter when the attention answer arrives and
    return a stall-driven blocked/frozen sub-state to open (with a
    history entry, so an unfreeze is audited). Non-stall sub-states are
    left alone.

    Frozen is absorbing (#1467): unfreeze_authority='approval' is
    carried only by the authorized kind=approval close; any other
    resolve on a frozen task is refused and audited by the choke point
    instead of opening the task — belt and braces with the
    frozen_unfreeze_requires_approval policy guard, and it also holds
    with the protocol switch off (the state invariant is not a policy
    gate)."""
    record["stall_count"] = 0
    current = _decision_status_of(task)
    if current not in ("blocked", "frozen"):
        return
    reason = "unfrozen by approval record" if current == "frozen" else "attention answer received"
    _set_decision_status(
        task,
        "open",
        reason=reason,
        source=f"attention:{str(record.get('kind') or 'unknown')}",
        actor=actor,
        unfreeze_authority=unfreeze_authority,
    )
