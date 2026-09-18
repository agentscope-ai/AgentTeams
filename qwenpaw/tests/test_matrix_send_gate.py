"""Behavioral tests for the send alignment gate (#1244).

The gate lives in ``_on_process_completed``: before the final reply of a
turn is flushed, new room events recorded after the turn's context
snapshot (the per-room history buffer, empty at enqueue time) make the
reply stale.  Stale replies are dropped and the agent is re-triggered
with the fresh context; a bounded budget falls back to "send with note".
"""

import asyncio
from unittest.mock import AsyncMock

from agentteams_matrix.channel import (
    _MATRIX_OWN_THREAD_ROOT_KEY,
    _MATRIX_PENDING_FINAL_MESSAGE_KEY,
    _MATRIX_SEND_GATE_MAX_RETRIGGERS,
    _THREAD_META_ROOT_KEY,
    AgentTeamsMatrixChannel,
    HistoryEntry,
)


def _make_channel(user_id: str = "@worker-a:hs.local") -> AgentTeamsMatrixChannel:
    ch = AgentTeamsMatrixChannel.__new__(AgentTeamsMatrixChannel)
    ch._user_id = user_id
    ch.history_limit = 50
    ch._room_histories = {}
    ch._room_history_gen = {}
    ch._room_history_records = {}
    ch._room_event_log = {}
    ch._send_gate_state = {}
    ch.enqueued = []
    ch._enqueue = ch.enqueued.append
    ch._send_typing = AsyncMock()
    ch._send_plain_text = AsyncMock()
    return ch


def _entry(sender: str = "@carol:hs.local", body: str = "new message") -> HistoryEntry:
    return HistoryEntry(
        sender=sender,
        body=body,
        timestamp=1_750_000_000_000,
        message_id="$m1",
    )


def _turn_meta(event_id: str = "$turn1", **overrides) -> dict:
    meta = {
        "room_id": "!room:hs.local",
        "is_dm": False,
        "is_group": True,
        "event_id": event_id,
        "thread_root_event_id": event_id,
        "sender_id": "@carol:hs.local",
    }
    meta.update(overrides)
    return meta


def test_gate_aligned_when_buffer_empty():
    ch = _make_channel()
    action, count = ch._evaluate_send_gate(_turn_meta(), "!room:hs.local")
    assert (action, count) == ("ok", 0)


def test_gate_skips_dm_and_thread_events():
    ch = _make_channel()
    ch._room_histories["!room:hs.local"] = [_entry()]
    for overrides in ({"is_dm": True, "is_group": False}, {"is_thread_event": True}):
        action, count = ch._evaluate_send_gate(_turn_meta(**overrides), "!room:hs.local")
        assert (action, count) == ("ok", 0)


def test_gate_ok_without_turn_snapshot():
    ch = _make_channel()
    ch._room_histories["!room:hs.local"] = [_entry()]
    # Proactive (cron) turns carry no event_id — not gated.
    action, count = ch._evaluate_send_gate(_turn_meta(event_id=""), "!room:hs.local")
    assert (action, count) == ("ok", 1)


def test_gate_retrigger_on_new_room_events():
    ch = _make_channel()
    ch._room_histories["!room:hs.local"] = [_entry("a"), _entry("b")]
    action, count = ch._evaluate_send_gate(_turn_meta(), "!room:hs.local")
    assert (action, count) == ("retrigger", 2)
    state = ch._send_gate_state["!room:hs.local"]
    assert state["event_id"] == "$turn1"
    assert state["count"] == 0


def test_gate_note_after_retrigger_budget_exhausted():
    ch = _make_channel()
    ch._room_histories["!room:hs.local"] = [_entry()]
    ch._send_gate_state["!room:hs.local"] = {
        "event_id": "$turn1",
        "count": _MATRIX_SEND_GATE_MAX_RETRIGGERS,
        "deadline": 1e18,
    }
    action, count = ch._evaluate_send_gate(_turn_meta(), "!room:hs.local")
    assert (action, count) == ("note", 1)


def test_gate_note_after_deadline():
    ch = _make_channel()
    ch._room_histories["!room:hs.local"] = [_entry()]
    ch._send_gate_state["!room:hs.local"] = {
        "event_id": "$turn1",
        "count": 0,
        "deadline": 0.0,  # long past
    }
    action, count = ch._evaluate_send_gate(_turn_meta(), "!room:hs.local")
    assert (action, count) == ("note", 1)


def test_gate_fresh_turn_resets_budget():
    ch = _make_channel()
    ch._room_histories["!room:hs.local"] = [_entry()]
    ch._send_gate_state["!room:hs.local"] = {
        "event_id": "$old-turn",
        "count": _MATRIX_SEND_GATE_MAX_RETRIGGERS,
        "deadline": 1e18,
    }
    # A new real mention (different event id) starts a fresh budget.
    action, count = ch._evaluate_send_gate(_turn_meta(event_id="$turn2"), "!room:hs.local")
    assert (action, count) == ("retrigger", 1)


def test_retrigger_enqueues_fresh_context_and_preserves_buffer():
    """The re-trigger input is recovered from the retained event log, not
    the shared buffer — the buffer keeps its first-pass contract and is
    left untouched by the retrigger path."""
    ch = _make_channel()
    room = "!room:hs.local"
    ch._record_history(
        room, _entry(sender="@carol:hs.local", body="correction: do it differently"),
    )
    ch._send_gate_state[room] = {
        "event_id": "$turn1",
        "count": 0,
        "deadline": 1e18,
    }

    asyncio.run(ch._retrigger_for_new_context(_turn_meta(), room, 1))

    assert len(ch.enqueued) == 1
    payload = ch.enqueued[0]
    assert payload["meta"]["send_gate_retrigger"] is True
    assert payload["meta"]["event_id"] == "$turn1"
    assert payload["meta"]["sender_id"] == "@carol:hs.local"
    # Missed context baked in: nudge + recovered history prepend.
    text = payload["content_parts"][0].text
    assert "draft reply was NOT sent" in text
    assert "correction: do it differently" in text
    # The shared buffer is NOT consumed by the retrigger (it still serves
    # first-pass turns).
    assert len(ch._room_histories.get(room) or []) == 1
    assert ch._send_gate_state[room]["count"] == 1
    ch._send_typing.assert_awaited_once_with(room, True)


def test_process_completed_drops_stale_reply_and_retriggers():
    ch = _make_channel()
    ch._room_histories["!room:hs.local"] = [_entry(body="too late for that draft")]
    send_meta = _turn_meta()
    send_meta[_MATRIX_PENDING_FINAL_MESSAGE_KEY] = "stale final reply"

    asyncio.run(ch._on_process_completed(None, "!room:hs.local", send_meta))

    # Re-trigger enqueued; the stale reply was NOT flushed.
    assert len(ch.enqueued) == 1
    assert ch.enqueued[0]["meta"]["send_gate_retrigger"] is True
    # The stale pending message was dropped, not sent.
    ch._send_plain_text.assert_not_called()
    assert "stale final reply" not in str(ch.enqueued)


def test_retrigger_carries_inflight_placeholder_root():
    """The stale turn's in-flight thread-root placeholder must be carried
    over: the retriggered turn edits that same message instead of leaving
    an orphaned "处理中..." and posting a second placeholder."""
    ch = _make_channel()
    ch._room_histories["!room:hs.local"] = [_entry()]
    meta = _turn_meta()
    meta[_MATRIX_OWN_THREAD_ROOT_KEY] = "$root-placeholder"
    meta[_THREAD_META_ROOT_KEY] = "$root-placeholder"

    asyncio.run(ch._retrigger_for_new_context(meta, "!room:hs.local", 1))

    payload = ch.enqueued[0]
    assert payload["meta"][_MATRIX_OWN_THREAD_ROOT_KEY] == "$root-placeholder"
    assert payload["meta"][_THREAD_META_ROOT_KEY] == "$root-placeholder"


def test_retrigger_without_placeholder_root_stays_clean():
    """A turn that never created a placeholder (e.g. gate before streaming
    start) must not invent root keys: the retriggered turn creates its own."""
    ch = _make_channel()
    ch._room_histories["!room:hs.local"] = [_entry()]

    asyncio.run(ch._retrigger_for_new_context(_turn_meta(), "!room:hs.local", 1))

    payload = ch.enqueued[0]
    assert _MATRIX_OWN_THREAD_ROOT_KEY not in payload["meta"]
    # The fallback thread key still points at the turn event (normal path).
    assert payload["meta"][_THREAD_META_ROOT_KEY] == "$turn1"


def test_retrigger_meta_carries_fresh_snapshot():
    """The retriggered turn needs its own freshness marker captured at
    retrigger time — otherwise the buffer pop at the end of the retrigger
    path would leave the re-run turn without a marker and the shared-buffer
    fallback would decide for it."""
    ch = _make_channel()
    room = "!room:hs.local"
    ch._room_histories[room] = [_entry()]
    ch._room_history_gen[room] = 5
    ch._room_history_records[room] = 17
    ch._send_gate_state[room] = {"event_id": "$turn1", "count": 0, "deadline": 1e18}

    asyncio.run(ch._retrigger_for_new_context(_turn_meta(), room, 1))

    meta = ch.enqueued[0]["meta"]
    assert meta["send_gate_snapshot_gen"] == 5
    assert meta["send_gate_snapshot_records"] == 17


def test_two_sender_retrigger_recovers_missed_context_and_second_completion():
    """Maintainer's exact repro through the real methods: record
    NEW_CRITICAL_CONTEXT after A's snapshot, let B's enqueue clear the
    shared history, then retrigger A.  The retrigger payload must contain
    NEW_CRITICAL_CONTEXT (not just the generic nudge), and A's second
    completion gates aligned (ok) only because the context was actually
    supplied — in sender-isolated sessions B's consumed context is
    otherwise unreachable from A."""
    ch = _make_channel()
    room = "!room:hs.local"

    # A's mention turn is enqueued: A's context event recorded, snapshot
    # captured, buffer cleared at enqueue.
    ch._record_history(room, _entry(body="task: ship it"))
    meta_a = _turn_meta(event_id="$turn-a")
    ch._capture_send_gate_snapshot(meta_a, room)
    ch._clear_history(room)

    # A is still running: B's mention lands (the context that makes A
    # stale) and B's enqueue snapshots + clears the shared buffer.
    ch._record_history(room, _entry(body="NEW_CRITICAL_CONTEXT"))
    meta_b = _turn_meta(event_id="$turn-b")
    ch._capture_send_gate_snapshot(meta_b, room)
    ch._clear_history(room)

    # A finishes first: stale -> retrigger.
    action_a, new_count_a = ch._evaluate_send_gate(meta_a, room)
    assert (action_a, new_count_a) == ("retrigger", 1)
    asyncio.run(ch._retrigger_for_new_context(meta_a, room, new_count_a))

    assert len(ch.enqueued) == 1
    payload = ch.enqueued[0]
    text = payload["content_parts"][0].text
    assert "draft reply was NOT sent" in text
    assert "NEW_CRITICAL_CONTEXT" in text, (
        "the retrigger must recover the events that made A stale — "
        "the shared buffer was consumed by B's enqueue"
    )
    # The re-triggered turn's snapshot covers exactly the recovered
    # context (current gen/records).
    meta_a2 = payload["meta"]
    assert meta_a2["send_gate_snapshot_gen"] == 2
    assert meta_a2["send_gate_snapshot_records"] == 2

    # A's second completion: nothing new since the recovery -> aligned,
    # the reply may flush.  B remains aligned as well.
    action_a2, count_a2 = ch._evaluate_send_gate(meta_a2, room)
    assert (action_a2, count_a2) == ("ok", 0)
    action_b, count_b = ch._evaluate_send_gate(meta_b, room)
    assert (action_b, count_b) == ("ok", 0)


def test_retrigger_recovers_media_from_missed_window():
    """Media parts carried by the missed events ride along into the
    retrigger input (vision models must see them, not just the text)."""
    ch = _make_channel()
    room = "!room:hs.local"
    fake_media = object()
    ch._record_history(room, _entry(body="before the snapshot"))
    meta = _turn_meta()
    ch._capture_send_gate_snapshot(meta, room)
    ch._record_history(
        room,
        HistoryEntry(
            sender="@carol:hs.local",
            body="with image",
            timestamp=1,
            message_id="$m2",
            media_parts=[fake_media],
        ),
    )

    asyncio.run(ch._retrigger_for_new_context(meta, room, 1))

    payload = ch.enqueued[0]
    assert "with image" in payload["content_parts"][0].text
    assert any(p is fake_media for p in payload["content_parts"])


def test_retrigger_window_head_dropped_under_cap_still_recovers_tail():
    """A turn in flight for longer than history_limit room events loses
    the head of its missed window (cap drop); the tail through the newest
    event is still recovered and the snapshot advances only to the
    context actually supplied."""
    ch = _make_channel()
    ch.history_limit = 2
    room = "!room:hs.local"
    for body in ("e1", "e2", "e3", "e4"):
        ch._record_history(room, _entry(body=body))
    meta = _turn_meta()
    meta["send_gate_snapshot_gen"] = 1
    meta["send_gate_snapshot_records"] = 1
    ch._send_gate_state[room] = {"event_id": "$turn1", "count": 0, "deadline": 1e18}

    asyncio.run(ch._retrigger_for_new_context(meta, room, 3))

    text = ch.enqueued[0]["content_parts"][0].text
    # e2 (record 2) was dropped from the retained log by the cap; the
    # tail (e3, e4) is recovered.
    assert "e2" not in text
    assert "e3" in text and "e4" in text
    # Snapshot advances to the context actually supplied (record 4).
    meta_a2 = ch.enqueued[0]["meta"]
    assert meta_a2["send_gate_snapshot_records"] == 4
    action, count = ch._evaluate_send_gate(meta_a2, room)
    assert (action, count) == ("ok", 0)


def test_gate_marker_not_consumed_by_different_mention():
    """Maintainer's exact sequence: A is running, B is mentioned, A finishes
    with the same prompt text, B finishes.  B's enqueue clears the shared
    room buffer — with the old buffer-as-marker design that consumed A's
    staleness signal.  With the per-turn generation snapshot, A must be
    blocked (retriggered) while B passes."""
    ch = _make_channel()
    room = "!room:hs.local"

    # A's mention turn is enqueued: snapshot captured, buffer cleared.
    meta_a = _turn_meta(event_id="$turn-a")
    ch._capture_send_gate_snapshot(meta_a, room)
    ch._clear_history(room)

    # A is still running: room event X lands after A's snapshot.
    ch._record_history(room, _entry(sender="@c:hs.local", body="X"))

    # B's mention turn is enqueued: B's snapshot includes X; the buffer
    # clear here used to consume A's staleness marker.
    meta_b = _turn_meta(event_id="$turn-b")
    ch._capture_send_gate_snapshot(meta_b, room)
    ch._clear_history(room)

    # A finishes first — the room changed after A's snapshot -> stale.
    action_a, _ = ch._evaluate_send_gate(meta_a, room)
    assert action_a == "retrigger", (
        "A's draft is stale: B's mention arrived after A's snapshot, "
        "but the different mention's clear must not consume A's marker"
    )

    # B finishes — nothing new after B's snapshot -> aligned, sends.
    action_b, count_b = ch._evaluate_send_gate(meta_b, room)
    assert (action_b, count_b) == ("ok", 0)


def test_process_completed_blocks_stale_reply_after_other_mention_cleared_buffer():
    """End-to-end through _on_process_completed: A's buffer was consumed by
    B's enqueue, so the shared buffer is empty at A's completion — the gate
    must still retrigger A (drop the stale draft) instead of flushing it."""
    ch = _make_channel()
    room = "!room:hs.local"
    meta_a = _turn_meta(event_id="$turn-a")
    meta_a[_MATRIX_PENDING_FINAL_MESSAGE_KEY] = "stale final reply"
    ch._capture_send_gate_snapshot(meta_a, room)
    ch._record_history(room, _entry(body="B's mention"))
    ch._clear_history(room)  # B's enqueue consumed the buffer

    asyncio.run(ch._on_process_completed(None, room, meta_a))

    # Re-trigger enqueued; the stale reply was NOT flushed.
    assert len(ch.enqueued) == 1
    assert ch.enqueued[0]["meta"]["send_gate_retrigger"] is True
    ch._send_plain_text.assert_not_called()
    assert "stale final reply" not in str(ch.enqueued)

    # The retrigger input recovered the missed context (B's mention), not
    # just the generic nudge.
    assert "B's mention" in ch.enqueued[0]["content_parts"][0].text

    # Second completion of A: aligned now (the context was actually
    # supplied) and the final reply flushes.
    meta_a2 = ch.enqueued[0]["meta"]
    meta_a2[_MATRIX_PENDING_FINAL_MESSAGE_KEY] = "aligned second reply"
    ch.send_message_content = AsyncMock()
    asyncio.run(ch._on_process_completed(None, room, meta_a2))
    ch.send_message_content.assert_awaited_once_with(
        room, "aligned second reply", meta_a2,
    )
