# Approval lifecycle: bounded wait, asked/decided audit pair, and callId
# resolve dedup (TH-20, the three approval items).
#
# Layering (DSH comparison audit, 11-team-coordination line 05): our
# approval gate is the TASK-DECISION layer — the r1-style gate that asks
# a human before a task result can be decided — not the tool-call layer.
# These three are that layer's natural extensions, and they close the
# gaps the audit named:
#
# ① bounded wait — an approval ask carries a deadline (explicit
#    ``deadline_at``, a ``wait_seconds`` duration, or the 30-minute
#    default). When the deadline lapses without a human decision the ask
#    transitions to ``unavailable``. The wait is bounded — the task is
#    never permanently frozen on a silent approver — and the lapse is
#    never an approval: no auto-release, fail closed, no late decision
#    after the deadline. Asking again is a NEW attempt with a fresh
#    callId and a fresh deadline.
#
# ② asked/decided audit pair — every ask appends an ``asked`` event and
#    every terminal outcome (approved / denied / unavailable) appends
#    the ``decided`` event carrying the same callId, so the pair is
#    correlatable and every asked has exactly one decided. The journal
#    is a SEPARATE append-only file (``approval_audit.jsonl`` in the
#    task directory), stored apart from the task meta attention list
#    (the loop record's home) and the conversation transcript.
#
# ③ callId dedup — the callId is the approval attempt identity:
#    assigned at ask time (or caller-supplied for an idempotent re-ask),
#    carried by both audit events, and the dedup key of resolve. A
#    second resolve with the same callId is an idempotent replay (same
#    decision) or a rejection (conflicting decision) — never a second
#    decision, never a second journal entry.
#
# Contract boundaries (kept deliberately narrow): this module emits no
# SIG-* signal events (the signal-mapping vocabulary is untouched),
# touches no NOTIFICATION_GAP_REASONS field, and writes ``resolvedVia``
# only from the pre-existing five-value taskflow vocabulary.
# Pre-TH-20 attention records (no callId) are out of scope: they keep
# their existing behaviour and are never force-migrated.
#
# #1467 integration (th-batch-b merge plan): this module is the TH-20
# approval lifecycle, renamed from review.py so the #1467 protocol
# guard can own review.py. A human-channel resolve (im_reply /
# im_command / dashboard) from a human runtime additionally writes the
# trust markers (resolvedRole + resolvedActor) that the guard's
# _has_approval_record requires — the guard's own canonical identity
# primitives (one implementation, no fork), identity sourced from the
# trusted runtime only, never from the payload's decided_by.
from __future__ import annotations

import datetime

import json

import uuid

from pathlib import Path

from typing import Any

from mcp.common import refs

# #1467 integration: the trust markers reuse the guard's canonical
# identity primitives (same package, one implementation — no fork).
# Before the merge the guard is not in the tree yet, so the import
# fails and no markers are written (fail closed).
try:
    from .review import (
        _HUMAN_RESOLVE_CHANNELS,
        _canonical_account,
        _decider_identity,
        _runtime_role,
    )
except ImportError:
    _HUMAN_RESOLVE_CHANNELS = frozenset()
    _canonical_account = None
    _decider_identity = None
    _runtime_role = None

_runtime_identity = refs.proxy("_runtime_identity")

# The approval wait bound: the default deadline when an ask carries no
# explicit one — 30 minutes, the reversible-decision bound (MF-3 R6
# keeps the irreversible bound at 2h).
DEFAULT_APPROVAL_WAIT_SECONDS = 1800

# The separate audit journal, one JSONL line per event, in the task
# directory NEXT TO meta.json — the pair is stored apart from the
# attention list (the loop record's home) and the transcript.
APPROVAL_AUDIT_FILENAME = "approval_audit.jsonl"

# Approval record states (approvalStatus on the attention record).
APPROVAL_PENDING = "pending"
APPROVAL_APPROVED = "approved"
APPROVAL_DENIED = "denied"
APPROVAL_UNAVAILABLE = "unavailable"

# Decision verbs a human (or a human-authorized integration) submits.
DECISION_APPROVE = "approve"
DECISION_DENY = "deny"

# Terminal outcomes (decided events / record states).
OUTCOME_APPROVED = "approved"
OUTCOME_DENIED = "denied"
OUTCOME_UNAVAILABLE = "unavailable"

# The resolve-channel vocabulary the taskflow close path already
# validates (request_attention resolvedVia) — reused, never extended.
RESOLVE_VIA_CHANNELS = {"tool", "dashboard", "im_reply", "im_command", "api"}


class ApprovalGuardError(Exception):
    """Structured approval rejection: a stable error_code plus the
    stable message. Deliberately NOT a ValueError subclass so the
    dispatch layers can return the {ok, error_code, message} surface
    instead of a bare error string (same shape as the W2
    ProtocolGuardError vocabulary this branch predates)."""

    def __init__(self, error_code: str, message: str) -> None:
        super().__init__(message)
        self.error_code = error_code
        self.message = message


def _parse_iso8601(value: str, field: str) -> datetime.datetime:
    """Parse an ISO-8601 timestamp (Z tolerated); naive reads as UTC."""
    try:
        parsed = datetime.datetime.fromisoformat(str(value).strip().replace("Z", "+00:00"))
    except ValueError as exc:
        raise ValueError(f"{field} must be an ISO-8601 timestamp") from exc
    return parsed if parsed.tzinfo else parsed.replace(tzinfo=datetime.timezone.utc)


def _as_datetime(now: Any) -> datetime.datetime:
    """The evaluation clock: an explicit ISO string / datetime (tests,
    replay) or the real UTC clock. Naive datetimes read as UTC."""
    if now is None:
        return datetime.datetime.now(datetime.timezone.utc)
    if isinstance(now, datetime.datetime):
        return now if now.tzinfo else now.replace(tzinfo=datetime.timezone.utc)
    return _parse_iso8601(now, "now")


def _iso(moment: datetime.datetime) -> str:
    """The canonical timestamp form (second precision, Z suffix) — the
    same shape _utc_timestamp emits."""
    return (
        moment.astimezone(datetime.timezone.utc)
        .replace(microsecond=0)
        .isoformat()
        .replace("+00:00", "Z")
    )


def _new_call_id() -> str:
    return "call_" + uuid.uuid4().hex[:16]


def approval_audit_path(workspace: Path | str, task_id: str) -> Path:
    """The separate audit journal for one task (task directory, next to
    meta.json — apart from the attention list and the transcript)."""
    return (
        Path(workspace).expanduser()
        / "shared"
        / "tasks"
        / str(task_id)
        / APPROVAL_AUDIT_FILENAME
    )


def _is_approval_record(record: Any) -> bool:
    return (
        isinstance(record, dict)
        and str(record.get("kind") or "") == "approval"
    )


def _approval_records(task: dict[str, Any]) -> list[dict[str, Any]]:
    attention = task.get("attention")
    if not isinstance(attention, list):
        return []
    return [r for r in attention if _is_approval_record(r)]


def _find_record(task: dict[str, Any], call_id: str) -> dict[str, Any] | None:
    """The approval record carrying this callId (the last one — an
    idempotent ask makes duplicates impossible)."""
    found = None
    for record in _approval_records(task):
        if str(record.get("callId") or "") == call_id:
            found = record
    return found


def _read_journal(path: Path) -> list[dict[str, Any]]:
    """The journal entries, in append order. Malformed lines (a torn
    write) are skipped, never fatal — the journal is an audit mirror,
    and the record state on the task meta stays authoritative."""
    if not path.is_file():
        return []
    entries: list[dict[str, Any]] = []
    try:
        text = path.read_text(encoding="utf-8")
    except OSError:
        return []
    for line in text.splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            entry = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(entry, dict):
            entries.append(entry)
    return entries


def _load_journal(workspace: Path | str, task_id: str) -> list[dict[str, Any]]:
    return _read_journal(approval_audit_path(workspace, task_id))


def _append_event(
    workspace: Path | str,
    task_id: str,
    event: dict[str, Any],
) -> dict[str, Any]:
    """Append one journal line with its monotonically increasing seq."""
    path = approval_audit_path(workspace, task_id)
    entries = _read_journal(path)
    event = dict(event)
    event["seq"] = max((int(e.get("seq") or 0) for e in entries), default=0) + 1
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a", encoding="utf-8") as handle:
        handle.write(json.dumps(event, ensure_ascii=False) + "\n")
    return event


def _find_event(
    entries: list[dict[str, Any]],
    kind: str,
    call_id: str,
) -> dict[str, Any] | None:
    for entry in entries:
        if (
            str(entry.get("event") or "") == kind
            and str(entry.get("callId") or "") == call_id
        ):
            return entry
    return None


def ask_approval(
    task: dict[str, Any],
    workspace: Path | str,
    task_id: str,
    *,
    question: str = "",
    deadline_at: str | None = None,
    wait_seconds: float | int | None = None,
    call_id: str | None = None,
    now: str | datetime.datetime | None = None,
) -> dict[str, Any]:
    """Ask a human for a bounded approval decision (②: the ``asked``
    half of the audit pair).

    Appends a kind='approval' attention record (the loop) carrying the
    TH-20 fields — callId, approvalStatus 'pending', approvalDeadlineAt
    — and appends the ``asked`` event to the separate journal. The
    deadline is the explicit ``deadline_at`` (ISO-8601), else
    ``wait_seconds`` from now, else the 30-minute default.

    Idempotent per callId: an OPEN (pending) approval with the same
    callId is reused as-is — no second record, no second asked event
    (a retry of a failed ask must not ping twice). A callId whose
    previous ask already reached a terminal state is NOT reused: a
    re-ask after a decision or a lapse must come with a fresh callId.
    """
    if not isinstance(task, dict):
        raise ValueError("task must be the task meta dict")
    attention = task.get("attention")
    if attention is None:
        attention = []
        task["attention"] = attention
    if not isinstance(attention, list):
        raise ValueError("task['attention'] must be a list")
    now_dt = _as_datetime(now)
    now_iso = _iso(now_dt)

    call_id = str(call_id or "").strip() or _new_call_id()
    existing = _find_record(task, call_id)
    if (
        existing is not None
        and str(existing.get("approvalStatus") or APPROVAL_PENDING) == APPROVAL_PENDING
    ):
        asked = _find_event(_load_journal(workspace, task_id), "asked", call_id)
        return {
            "ok": True,
            "duplicate": True,
            "call_id": call_id,
            "record": existing,
            "asked": asked,
            "deadline_at": str(existing.get("approvalDeadlineAt") or ""),
        }

    if deadline_at is not None:
        deadline_dt = _parse_iso8601(deadline_at, "deadline_at")
    elif wait_seconds is not None:
        try:
            seconds = float(wait_seconds)
        except (TypeError, ValueError) as exc:
            raise ValueError("wait_seconds must be a number") from exc
        if seconds <= 0:
            raise ValueError("wait_seconds must be > 0")
        deadline_dt = now_dt + datetime.timedelta(seconds=seconds)
    else:
        deadline_dt = now_dt + datetime.timedelta(seconds=DEFAULT_APPROVAL_WAIT_SECONDS)
    deadline_iso = _iso(deadline_dt)

    record: dict[str, Any] = {
        "kind": "approval",
        "question": str(question or "").strip()[:500],
        # Same attempt counting as the taskflow loop: the 1-based
        # position among ALL approval records, resolved or not.
        "attempt": len(_approval_records(task)) + 1,
        "requestedAt": now_iso,
        # The loop is open until a decision closes it — a lapse never
        # resolves it (①: the unavailable state is not a resolution).
        "resolved": False,
        "callId": call_id,
        "approvalStatus": APPROVAL_PENDING,
        "approvalDeadlineAt": deadline_iso,
    }
    attention.append(record)
    asked_event = _append_event(
        workspace,
        task_id,
        {
            "event": "asked",
            "callId": call_id,
            "taskId": str(task_id),
            "attempt": record["attempt"],
            "question": record["question"],
            "deadlineAt": deadline_iso,
            "at": now_iso,
        },
    )
    return {
        "ok": True,
        "duplicate": False,
        "call_id": call_id,
        "record": record,
        "asked": asked_event,
        "deadline_at": deadline_iso,
    }


def _lapse_record(
    task: dict[str, Any],
    workspace: Path | str,
    task_id: str,
    record: dict[str, Any],
    now_dt: datetime.datetime,
) -> dict[str, Any]:
    """Move ONE expired pending approval record to ``unavailable`` and
    close its audit pair with the lapse outcome (① + ②). Never sets
    resolved — the lapse is not a decision (no auto-release)."""
    now_iso = _iso(now_dt)
    deadline_iso = str(record.get("approvalDeadlineAt") or "")
    record["approvalStatus"] = APPROVAL_UNAVAILABLE
    record["unavailableAt"] = now_iso
    event = _append_event(
        workspace,
        task_id,
        {
            "event": "decided",
            "callId": str(record.get("callId") or ""),
            "taskId": str(task_id),
            "outcome": OUTCOME_UNAVAILABLE,
            "reason": "bounded_wait_expired",
            "deadlineAt": deadline_iso,
            "at": now_iso,
        },
    )
    return {
        "call_id": str(record.get("callId") or ""),
        "deadline_at": deadline_iso,
        "unavailable_at": now_iso,
        "decided": event,
    }


def check_approval_bounded_wait(
    task: dict[str, Any],
    workspace: Path | str,
    task_id: str,
    *,
    now: str | datetime.datetime | None = None,
) -> list[dict[str, Any]]:
    """Evaluate the bounded waits (①): every pending approval record
    whose deadline has passed (``now >= deadline``, inclusive) lapses to
    ``unavailable`` and its audit pair closes with the lapse outcome.

    The check is lazy — the MCP runtime has no timer thread, so the
    wait is evaluated on the next approval-facing operation (this
    function, or resolve_approval, which runs it itself). Idempotent: a
    record already in a terminal state is never re-lapsed, and the
    journal gains at most one decided event per ask. Returns the
    transitions (one per record that lapsed on this call).

    Legacy records without a callId are skipped: the bounded wait and
    the audit pair apply to callId-carrying records only.
    """
    if not isinstance(task, dict):
        raise ValueError("task must be the task meta dict")
    now_dt = _as_datetime(now)
    transitions: list[dict[str, Any]] = []
    for record in _approval_records(task):
        if str(record.get("approvalStatus") or APPROVAL_PENDING) != APPROVAL_PENDING:
            continue
        deadline_iso = str(record.get("approvalDeadlineAt") or "")
        if not deadline_iso or not str(record.get("callId") or ""):
            continue
        deadline_dt = _parse_iso8601(deadline_iso, "approvalDeadlineAt")
        if now_dt < deadline_dt:
            continue
        transitions.append(
            _lapse_record(task, workspace, task_id, record, now_dt)
        )
    return transitions


def resolve_approval(
    task: dict[str, Any],
    workspace: Path | str,
    task_id: str,
    call_id: str,
    decision: str,
    *,
    decided_by: str = "",
    via: str = "tool",
    evidence: dict[str, Any] | None = None,
    now: str | datetime.datetime | None = None,
) -> dict[str, Any]:
    """Record a human decision on a bounded approval (②: the
    ``decided`` half of the pair; ③: the callId-deduped resolve).

    ``decision`` is 'approve' or 'deny'. The outcome closes the loop
    the same way the taskflow close path does (resolved +
    resolvedVia/resolvedBy/resolvedAt, plus resolveEvidence when
    given) and appends exactly one ``decided`` event carrying the
    callId.

    Dedup (③): a second resolve with the SAME callId and the same
    decision is an idempotent replay — the original outcome is
    returned with ``duplicate: True``, no new journal entry, no state
    change (a retry after a lost response must not decide twice). A
    same-callId CONFLICT (a different decision) is rejected — the
    first decision wins. A callId that never asked is rejected.

    Bound (①): the deadline is evaluated HERE as well as in
    check_approval_bounded_wait — a decision after the deadline is
    rejected (approval_unavailable): the wait ended, the approver was
    unavailable, and neither auto-release nor a late approval is
    honored. Re-ask with a fresh callId instead.
    """
    if not isinstance(task, dict):
        raise ValueError("task must be the task meta dict")
    call_id = str(call_id or "").strip()
    if not call_id:
        raise ApprovalGuardError(
            "invalid_call_id", "callId is required to resolve an approval"
        )
    decision = str(decision or "").strip().lower()
    if decision not in {DECISION_APPROVE, DECISION_DENY}:
        raise ApprovalGuardError(
            "invalid_decision", "decision must be one of approve, deny"
        )
    via = str(via or "").strip().lower() or "tool"
    if via not in RESOLVE_VIA_CHANNELS:
        raise ValueError(
            "resolvedVia must be one of tool, dashboard, im_reply, im_command, api"
        )
    now_dt = _as_datetime(now)
    now_iso = _iso(now_dt)

    entries = _load_journal(workspace, task_id)
    record = _find_record(task, call_id)

    # ③ first-decision-wins: the journal is the dedup authority.
    prior = _find_event(entries, "decided", call_id)
    if prior is not None:
        prior_outcome = str(prior.get("outcome") or "")
        if prior_outcome == OUTCOME_UNAVAILABLE:
            # ① the wait already ended with the approver unavailable —
            # a late decision is not honored under any decision verb.
            raise ApprovalGuardError(
                "approval_unavailable",
                "the bounded wait expired (approver unavailable); "
                "re-ask with a fresh callId",
            )
        expected = OUTCOME_APPROVED if decision == DECISION_APPROVE else OUTCOME_DENIED
        if prior_outcome == expected:
            return {
                "ok": True,
                "duplicate": True,
                "outcome": prior_outcome,
                "call_id": call_id,
                "record": record,
                "decided": prior,
            }
        raise ApprovalGuardError(
            "approval_already_decided",
            f"callId '{call_id}' was already decided as '{prior_outcome}' "
            "(first decision wins)",
        )

    if record is None:
        raise ApprovalGuardError(
            "approval_not_found",
            f"no approval request with callId '{call_id}'",
        )

    # ① the bound, evaluated on every path: a late decision is not
    # honored — the wait already ended with the approver unavailable.
    if str(record.get("approvalStatus") or APPROVAL_PENDING) == APPROVAL_UNAVAILABLE:
        raise ApprovalGuardError(
            "approval_unavailable",
            "the bounded wait expired (approver unavailable); "
            "re-ask with a fresh callId",
        )
    if str(record.get("approvalStatus") or APPROVAL_PENDING) in {
        OUTCOME_APPROVED,
        OUTCOME_DENIED,
    }:
        # Decided outside this module's journal (legacy close): the
        # record state is still authoritative — never decide twice.
        raise ApprovalGuardError(
            "approval_already_decided",
            "the approval record is already decided",
        )
    deadline_iso = str(record.get("approvalDeadlineAt") or "")
    if deadline_iso:
        deadline_dt = _parse_iso8601(deadline_iso, "approvalDeadlineAt")
        if now_dt >= deadline_dt:
            _lapse_record(task, workspace, task_id, record, now_dt)
            raise ApprovalGuardError(
                "approval_unavailable",
                "the bounded wait expired (approver unavailable); "
                "re-ask with a fresh callId",
            )

    outcome = OUTCOME_APPROVED if decision == DECISION_APPROVE else OUTCOME_DENIED
    record["approvalStatus"] = outcome
    record["decidedAt"] = now_iso
    # Close the loop the way the taskflow close path does (S0 resolve
    # provenance): the decision IS the resolution of the loop.
    record["resolved"] = True
    record["resolvedVia"] = via
    decided_by = str(decided_by or "").strip()[:200]
    if decided_by:
        record["resolvedBy"] = decided_by
    record["resolvedAt"] = now_iso
    # #1467 integration: a human-channel approval from a human runtime
    # is a valid approval record for the protocol guard. The markers
    # use the guard's canonical identity, sourced from the trusted
    # runtime identity only — never from the payload's decided_by (a
    # forged payload value must not open the delegate path). Denied
    # decisions, non-human channels, and non-human runtimes write
    # nothing (fail closed).
    if (
        outcome == OUTCOME_APPROVED
        and _HUMAN_RESOLVE_CHANNELS
        and via in _HUMAN_RESOLVE_CHANNELS
        and _runtime_role is not None
        and _runtime_role() == "human"
    ):
        trusted_account = _runtime_identity()
        if trusted_account:
            actor = f"human:{_canonical_account(trusted_account)}"
            marker_role, marker_account = _decider_identity(actor)
            if marker_role == "human" and marker_account:
                record["resolvedRole"] = "human"
                record["resolvedActor"] = actor
    if isinstance(evidence, dict) and evidence:
        record["resolveEvidence"] = evidence
    decided_event = _append_event(
        workspace,
        task_id,
        {
            "event": "decided",
            "callId": call_id,
            "taskId": str(task_id),
            "outcome": outcome,
            "decision": decision,
            "decidedBy": decided_by,
            "via": via,
            "at": now_iso,
        },
    )
    return {
        "ok": True,
        "duplicate": False,
        "outcome": outcome,
        "call_id": call_id,
        "record": record,
        "decided": decided_event,
    }


def approval_state(
    task: dict[str, Any],
    *,
    now: str | datetime.datetime | None = None,
) -> dict[str, Any]:
    """The live position of the task's most recent callId-carrying
    approval request: its state (pending / approved / denied /
    unavailable / None when none), its callId and deadline, and — for a
    pending record — whether the deadline has already passed without
    the lapse check having run yet (expired=True: past due, not yet
    audited). Legacy records without a callId are out of scope and are
    never reported here."""
    managed = [
        r for r in _approval_records(task) if str(r.get("callId") or "")
    ]
    if not managed:
        return {
            "status": None,
            "call_id": None,
            "deadline_at": None,
            "expired": False,
        }
    record = managed[-1]
    status = str(record.get("approvalStatus") or APPROVAL_PENDING)
    deadline_iso = str(record.get("approvalDeadlineAt") or "")
    expired = False
    if status == APPROVAL_PENDING and deadline_iso and now is not None:
        now_dt = _as_datetime(now)
        deadline_dt = _parse_iso8601(deadline_iso, "approvalDeadlineAt")
        expired = now_dt >= deadline_dt
    return {
        "status": status,
        "call_id": str(record.get("callId") or ""),
        "deadline_at": deadline_iso,
        "expired": expired,
    }


def read_approval_audit(workspace: Path | str, task_id: str) -> list[dict[str, Any]]:
    """The task's audit journal (separate file, append order)."""
    return _load_journal(workspace, task_id)


def audit_pairs(entries: list[dict[str, Any]]) -> dict[str, Any]:
    """Pair the journal into asked/decided pairs (②), by callId.

    Every callId with an asked AND a decided entry is a pair (the first
    of each, in append order). A callId with an asked but no decided is
    an open (or abandoned) wait — reported, not averaged away. A
    decided without an asked is a corrupt or foreign event; TWO decided
    events for one callId is a dedup violation the report surfaces in
    ``duplicate_decided`` instead of hiding.
    """
    asked: dict[str, dict[str, Any]] = {}
    decided: dict[str, list[dict[str, Any]]] = {}
    for entry in entries or []:
        if not isinstance(entry, dict):
            continue
        call_id = str(entry.get("callId") or "")
        if not call_id:
            continue
        kind = str(entry.get("event") or "")
        if kind == "asked" and call_id not in asked:
            asked[call_id] = entry
        elif kind == "decided":
            decided.setdefault(call_id, []).append(entry)
    pairs: dict[str, dict[str, Any]] = {}
    unpaired_asked: list[str] = []
    unpaired_decided: list[str] = []
    duplicate_decided: dict[str, int] = {}
    for call_id in sorted(set(asked) | set(decided)):
        has_asked = call_id in asked
        count_decided = len(decided.get(call_id, ()))
        if has_asked and count_decided >= 1:
            pairs[call_id] = {
                "asked": asked[call_id],
                "decided": decided[call_id][0],
            }
        if not has_asked and count_decided >= 1:
            unpaired_decided.append(call_id)
        if has_asked and count_decided == 0:
            unpaired_asked.append(call_id)
        if count_decided >= 2:
            duplicate_decided[call_id] = count_decided
    return {
        "pairs": pairs,
        "unpaired_asked": unpaired_asked,
        "unpaired_decided": unpaired_decided,
        "duplicate_decided": duplicate_decided,
    }
