# signal contract emission and event listing (W1-a2: real module)
from __future__ import annotations

import base64

import hashlib

import json

from pathlib import Path

from typing import Any

# Cross-group imports are top-level absolutes (`common.*`) — see the note in
# transitions.py: mcp/ on sys.path makes the identity identical in both
# `import server` and `import mcp.server` modes.
from mcp.common import refs
from mcp.common.config import _runtime_team_name
from mcp.common.io import _utc_timestamp
from mcp.common.paths import _workspace_dir

_filesync = refs.proxy("_filesync")

# S2 signal events (INT-3, #1394 aligned). The 14-event list is single-sourced
# at plugins/teamharness/contracts/signal-mapping.json (cross-language golden
# fixture; F8 precedent — the Go emitter table is cross-checked against it in
# its tests, this table must match the contract's py:* emitters, asserted by
# plugins/tests/teamharness/test_signal_emission.py).
SIGNAL_CONTRACT_PATH = (
    Path(__file__).resolve().parents[2] / "contracts" / "signal-mapping.json"
)


_SIGNAL_EMITTERS: dict[str, tuple[str, str, str]] = {
    # emitter -> (source_event, event_type, origin); must match the contract
    # rows' source_events / event_type / the schema origin enum.
    "submit_task": ("task_submitted", "SIG-COMPLETE", "submit"),
    "accept_task_result": ("task_accepted", "SIG-COMPLETE", "submit"),
    "record_loop_iteration": ("loop_iteration_finished", "SIG-DECISION", "decision_lock"),
    "record_loop_iteration:review": ("loop_review_verdict", "SIG-VERDICT", "critic_verdict"),
    "illegal_transition_rejected": ("mismatch_recorded", "SIG-MISMATCH", "mismatch_recorded"),
}


def _signal_event_id(
    team_id: str,
    project_id: str,
    task_id: str,
    loop_id: str,
    event_type: str,
    source_event: str,
    iteration: int | None,
    fingerprint: str,
    attempt: int = 0,
    audit_seq: int = 0,
    discriminator: str = "",
) -> str:
    # Contract id_rule (v1.2): 'ev_' + sha256(team_id|project_id|task_id|
    # loop_id|event_type|source_event|iteration|fingerprint|attempt|
    # audit_seq)[:16]. fingerprint is the hex sha256 of the ref bytes after
    # the main write — the stable identity of the logical operation's
    # result; audit_seq is the project's persisted committed-operation
    # revision (the operation INSTANCE identity), so two distinct
    # operations whose post-write state bytes are byte-identical (e.g. a
    # second pause within the same wall-clock second) still hash distinct
    # inputs, while a retry of the same operation (same committed revision
    # + same state bytes) re-lands on the same id (consumer dedupes).
    # Must stay byte-identical to the Go signalEventID (both test suites
    # pin the same fixed vector).
    #
    # Rejection events (SIG-MISMATCH) append an extra final component: a
    # rejection commits no state, so the ref bytes — and therefore the
    # fingerprint — are identical across *different* rejected operations
    # on the same task state. The rejection discriminator
    # ``action|from|to`` is the logical-operation identity for this
    # family: appending it keeps distinct rejections on distinct ids
    # while a retry of the same rejection re-hashes identically. Events
    # without a discriminator hash the fixed ten-field v1.2 input
    # unchanged (the cross-language fixed vector is untouched).
    parts = [
        team_id,
        project_id,
        task_id,
        loop_id,
        event_type,
        source_event,
        str(iteration) if iteration is not None else "",
        fingerprint,
        str(attempt),
        str(audit_seq),
    ]
    if discriminator:
        parts.append(discriminator)
    digest = hashlib.sha256("|".join(parts).encode("utf-8")).hexdigest()
    return "ev_" + digest[:16]


def _project_audit_seq(project: dict[str, Any] | Any) -> int:
    # id_rule v1.2: the project's committed-operation revision as persisted
    # by the Go intervention endpoints (markAuditFields advances it per
    # committed write). This runtime only ever READS it — Python writes
    # always change state bytes, so no increment is needed here. 0 when
    # absent (projects written before v1.2).
    if not isinstance(project, dict):
        return 0
    seq = project.get("audit_seq")
    if isinstance(seq, bool) or not isinstance(seq, int):
        return 0
    return seq


def _emit_signal_event(
    arguments: dict[str, Any],
    emitter: str,
    *,
    task_id: str = "",
    project_id: str = "",
    loop_id: str | None = None,
    iteration_index: int | None = None,
    ref: str = "",
    ref_bytes: bytes = b"",
    payload: dict[str, Any] | None = None,
    audit_seq: int = 0,
    discriminator: str = "",
) -> bool:
    """Append one S2 signal event object under shared/events/{team_id}/.

    Best-effort: never raises and never rolls back the caller's write (the
    main write already happened; the event is a convenience, same policy as
    the Go side). Idempotent: the deterministic event id makes a re-emit
    land on the same object key, which is then skipped. The local write is
    followed by a best-effort single-file mirror push (failure warn-only,
    the local object stays). Returns True when the object was written.

    audit_seq (contract id_rule v1.2) is the project's persisted
    committed-operation revision — the operation instance identity that
    distinguishes two operations whose post-write state bytes would
    otherwise be identical; 0 when the project carries none.

    discriminator (contract id_rule v1.2, rejection family): a rejection
    commits no state, so the ref bytes do not differ between distinct
    rejected operations. The ``action|from|to`` discriminator is appended
    to the identity so distinct rejections get distinct ids while a retry
    of the same rejection stays idempotent.
    """
    try:
        source_event, event_type, origin = _SIGNAL_EMITTERS[emitter]
        team_name = str(_runtime_team_name() or "").strip()
        team_id = team_name or "global"
        # The fingerprint of the post-write ref state plus the committed
        # audit_seq are the stable identity of this logical operation
        # (contract id_rule v1.2) — hashed into the event id so distinct
        # operations never share a key while a retry of the same operation
        # re-lands on the original.
        fingerprint = hashlib.sha256(ref_bytes).hexdigest()
        event_id = _signal_event_id(
            team_id,
            project_id,
            task_id,
            loop_id or "",
            event_type,
            source_event,
            iteration_index,
            fingerprint,
            0,
            audit_seq,
            discriminator,
        )
        path = _workspace_dir(arguments) / "shared" / "events" / team_id / f"{event_id}.json"
        if path.exists():
            return False  # idempotent re-emit: keep the original object
        event = {
            "event_id": event_id,
            "task_id": task_id,
            "loop_id": loop_id,
            "event_type": event_type,
            "is_signal": True,
            "source_event": {"origin": origin, "request_id": None},
            "ref": ref,
            "fingerprint": fingerprint,
            "iteration_index": iteration_index,
            "payload": payload or {},
            "occurred_at": _utc_timestamp(),
        }
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(event, ensure_ascii=False, indent=2), encoding="utf-8")
    except Exception:
        return False  # warn-only: never fail the main flow over an event
    try:
        sync_args = dict(arguments)
        sync_args.update({
            "action": "push",
            "path": f"shared/events/{team_id}/{event_id}.json",
        })
        _filesync(sync_args)
    except Exception:
        pass  # mirror push is warn-only
    return True


def _list_events(arguments: dict[str, Any], payload: dict[str, Any]) -> dict[str, Any]:
    """S2 event stream read (#1394 list_events, read-only, cursor pagination).

    Reads the LOCAL mirror under shared/events/{team}/ — the same read
    pattern as the other projectflow read actions. Controller-side emits
    become visible once the worker's next mirror cycle lands them (the
    consumer dedupes by event_id regardless).

    Cursor contract (#1433 round 2): the cursor is opaque to callers but
    shared ACROSS the two read surfaces — this action and the controller's
    GET /api/v1/projects/{id}/signal-events both emit
    ``base64url("<occurred_at>\x00<event_id>")`` and a page must resume
    across the boundary in either direction. Go emits unpadded
    (RawURLEncoding), Python emits padded (urlsafe_b64encode), so BOTH
    sides must accept BOTH forms (this decoder re-pads before decoding).
    If either side changes its cursor encoding, sync the other side's
    decoder and this note.
    """
    team = str(payload.get("team") or _runtime_team_name() or "").strip() or "global"
    since = str(payload.get("since") or "").strip()
    types = payload.get("types")
    if isinstance(types, list) and types:
        type_set = {str(t) for t in types}
    elif types:
        type_set = {str(types)}
    else:
        type_set = None
    task_id = str(payload.get("taskId") or payload.get("task_id") or "").strip() or None
    try:
        limit = int(payload.get("limit") or 50)
    except (TypeError, ValueError):
        limit = 50
    limit = max(1, min(limit, 200))
    cursor = str(payload.get("cursor") or "").strip()
    after: tuple[str, str] | None = None
    if cursor:
        try:
            # Re-pad to a multiple of 4: the Go read surface (#1433) emits
            # unpadded base64url cursors, and both forms must be accepted.
            padded = cursor + "=" * (-len(cursor) % 4)
            decoded = base64.urlsafe_b64decode(padded.encode("ascii")).decode("utf-8")
            after_ts, after_id = decoded.rsplit("\x00", 1)
            after = (after_ts, after_id)
        except Exception:
            return {
                "ok": False,
                "tool": "projectflow",
                "action": "list_events",
                "error": "invalid cursor",
            }
    events: list[dict[str, Any]] = []
    events_dir = _workspace_dir(arguments) / "shared" / "events" / team
    if events_dir.is_dir():
        for path in sorted(events_dir.glob("*.json")):
            try:
                ev = json.loads(path.read_text(encoding="utf-8"))
            except Exception:
                continue
            if not isinstance(ev, dict) or not isinstance(ev.get("event_id"), str):
                continue
            events.append(ev)
    if type_set is not None:
        events = [e for e in events if e.get("event_type") in type_set]
    if task_id:
        events = [e for e in events if str(e.get("task_id") or "") == task_id]
    if since:
        # RFC3339 UTC 'Z' timestamps: lexicographic == chronological.
        events = [e for e in events if str(e.get("occurred_at") or "") >= since]
    events.sort(key=lambda e: (str(e.get("occurred_at") or ""), str(e.get("event_id") or "")))
    if after is not None:
        events = [
            e for e in events
            if (str(e.get("occurred_at") or ""), str(e.get("event_id") or "")) > after
        ]
    page = events[:limit]
    next_cursor: str | None = None
    if len(events) > limit:
        last_ts = str(events[limit - 1].get("occurred_at") or "")
        last_id = str(events[limit - 1].get("event_id") or "")
        next_cursor = base64.urlsafe_b64encode(f"{last_ts}\x00{last_id}".encode("utf-8")).decode("ascii")
    return {
        "ok": True,
        "tool": "projectflow",
        "action": "list_events",
        "team": team,
        "events": page,
        "count": len(page),
        "next_cursor": next_cursor,
    }


