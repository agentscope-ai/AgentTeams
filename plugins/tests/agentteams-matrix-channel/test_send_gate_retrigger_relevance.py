"""Send-gate retrigger relevance check (#1386 P1).

A turn's pending reply obligation must only be voided by room events that
can plausibly change it.  Unrelated peer traffic (a parallel worker
answering its own question) must NOT re-trigger the turn: the completed
draft is delivered as-is (plus a "new message(s) arrived" note) instead of
being re-run into a NO_REPLY that silently loses the answer (issue #1386
case A).

Relevant events that still re-trigger:

  * a new message from the turn's trigger sender (the asker follows up),
  * any retained entry that addresses this worker by name.

Identity normalization (review round 2, R1): ``_on_room_event`` stores the
sender's DISPLAY NAME in ``HistoryEntry.sender`` (falling back to the
localpart) while the turn meta keeps the full MXID.  The relevance check
therefore compares the entry's canonical ``sender_id`` (full MXID), never
the display name — otherwise the asker's un-@'d follow-up degrades to
"note".  These tests deliver the follow-ups through the real
``_on_room_event`` callback (room display names included) instead of
constructing idealized ``HistoryEntry`` objects.

Run:  pytest plugins/tests/agentteams-matrix-channel/test_send_gate_retrigger_relevance.py
"""

from __future__ import annotations

import asyncio
import importlib.util
import sys
from pathlib import Path
from unittest.mock import AsyncMock, patch

import pytest

pytest.importorskip("nio")

REPO_ROOT = Path(__file__).resolve().parents[3]
CHANNEL_MODULE_PATH = (
    REPO_ROOT
    / "plugins"
    / "agentteams-matrix-channel"
    / "agentteams_matrix"
    / "channel.py"
)

_spec = importlib.util.spec_from_file_location(
    "agentteams_matrix_channel_1386_relevance", CHANNEL_MODULE_PATH
)
assert _spec is not None and _spec.loader is not None
channel_module = importlib.util.module_from_spec(_spec)
sys.modules["agentteams_matrix_channel_1386_relevance"] = channel_module
_spec.loader.exec_module(channel_module)

AgentTeamsMatrixChannel = channel_module.AgentTeamsMatrixChannel

OWN_ROOT = channel_module._MATRIX_OWN_THREAD_ROOT_KEY
META_ROOT = channel_module._THREAD_META_ROOT_KEY
PLACEHOLDER = channel_module._MATRIX_PLACEHOLDER_THREAD_ROOT_KEY
STREAMING_FINAL = channel_module._MATRIX_STREAMING_FINAL_TEXT_KEY

ROOM = "!room:matrix.local"
WORKER = "@test1:matrix.local"
PEER = "@test2:matrix.local"
LEADER = "@pi-leader:matrix.local"

# Display names deliberately DIFFERENT from the MXID localparts, the exact
# condition under which the display-name comparison used to break: the
# turn meta keeps "@pi-leader:matrix.local" while the history entry's
# ``sender`` is "PI Leader (human)" (or the "pi-leader" localpart fallback).
LEADER_DISPLAY = "PI Leader (human)"
PEER_DISPLAY = "Peer Worker Two"

ORIGINAL_DRAFT = "AT-R02-test1 complete answer (original draft)"


def _make_channel() -> AgentTeamsMatrixChannel:
    return AgentTeamsMatrixChannel(
        process=lambda *args, **kw: None,
        share_session_in_group=True,
    )


class _FakeRoom:
    """Minimal nio-MatrixRoom stand-in for the display-name lookup."""

    def __init__(self, room_id: str, display_names: dict):
        self.room_id = room_id
        self.users = {}
        self._display_names = display_names

    def user_name(self, user_id: str):
        return self._display_names.get(user_id)


class _FakeTextEvent:
    """Minimal nio-RoomMessageText stand-in (the attributes the handler reads)."""

    def __init__(self, sender: str, body: str, event_id: str = "evt-in"):
        self.sender = sender
        self.body = body
        self.event_id = event_id
        self.server_timestamp = 0
        self.source = {"content": {"body": body}}


def _land(
    channel: AgentTeamsMatrixChannel,
    sender: str,
    body: str,
    room: _FakeRoom,
    event_id: str = "evt-in",
) -> None:
    """Deliver *body* through the REAL ``_on_room_event`` callback.

    Group room, no mention of the worker -> the handler caches it to
    history exactly as in production: ``sender`` = display name (or
    localpart fallback), ``sender_id`` = the canonical MXID from
    ``event.sender``.
    """
    event = _FakeTextEvent(sender=sender, body=body, event_id=event_id)
    asyncio.run(channel._on_room_event(room, event))


def _placeholder_turn_meta() -> dict:
    return {
        "room_id": ROOM,
        "is_dm": False,
        "is_group": True,
        "worker_name": "test1",
        "event_id": "evt-turn1",
        "thread_root_event_id": "evt-turn1",
        "sender_id": LEADER,  # the asker — full MXID, as the turn meta keeps
        "send_gate_snapshot_gen": 0,
        "send_gate_snapshot_records": 0,
        OWN_ROOT: "root-placeholder",
        META_ROOT: "root-placeholder",
        PLACEHOLDER: True,
        STREAMING_FINAL: ORIGINAL_DRAFT,
    }


class _PatchCtx:
    def __init__(self, channel: AgentTeamsMatrixChannel):
        self.channel = channel
        self._patchers: list = []
        self.sinks: dict = {}

    def __enter__(self):
        c = self.channel
        self.sinks = {
            "enqueue": AsyncMock(),
            "edit_root": AsyncMock(),
            "send": AsyncMock(),
            "send_content": AsyncMock(),
            "plain_text": AsyncMock(),
            "typing": AsyncMock(),
            "base_completed": AsyncMock(),
        }
        targets = [
            (c, "_enqueue", "enqueue"),
            (c, "_edit_thread_root", "edit_root"),
            (c, "send", "send"),
            (c, "send_message_content", "send_content"),
            (c, "_send_plain_text", "plain_text"),
            (c, "_send_typing", "typing"),
            (channel_module.BaseChannel, "_on_process_completed", "base_completed"),
        ]
        for target, name, key in targets:
            p = patch.object(target, name, self.sinks[key])
            p.start()
            self._patchers.append(p)
        return self.sinks

    def __exit__(self, *exc):
        while self._patchers:
            self._patchers.pop().stop()
        return False


def _complete(channel: AgentTeamsMatrixChannel, meta: dict) -> None:
    asyncio.run(channel._on_process_completed(None, ROOM, meta))


def _last_entry(channel: AgentTeamsMatrixChannel):
    log = channel._room_event_log.get(ROOM, [])
    assert log, "no history entry recorded"
    return log[-1][1]


def test_trigger_sender_followup_retriggers_via_real_callback():
    """THE R1 regression, real callback: the asker's un-@'d follow-up
    arrives from a sender whose DISPLAY NAME differs from the MXID
    localpart; the history entry stores the display name, yet the gate
    must still retrigger (canonical sender_id comparison)."""
    channel = _make_channel()
    channel._user_id = WORKER
    room = _FakeRoom(ROOM, {LEADER: LEADER_DISPLAY})
    _land(channel, LEADER, "actually, please also cover edge case X", room)

    # Document the production seam: display name in ``sender`` (different
    # from both the MXID and its localpart), canonical MXID in ``sender_id``.
    entry = _last_entry(channel)
    assert entry.sender == LEADER_DISPLAY
    assert entry.sender_id == LEADER
    assert entry.sender != LEADER  # the comparison the old code used

    with _PatchCtx(channel) as sinks:
        _complete(channel, _placeholder_turn_meta())

    assert sinks["enqueue"].called
    retrigger_meta = sinks["enqueue"].call_args.args[0]["meta"]
    assert retrigger_meta.get(META_ROOT) == "root-placeholder"


def test_trigger_sender_followup_localpart_fallback_retriggers():
    """R1 regression, localpart fallback arm: the room has no display name
    for the asker, so ``HistoryEntry.sender`` falls back to the bare
    localpart ("pi-leader") — still not the full MXID the turn meta keeps.
    The gate must still retrigger via the canonical sender_id."""
    channel = _make_channel()
    channel._user_id = WORKER
    room = _FakeRoom(ROOM, {})  # user_name() -> None for everyone
    _land(channel, LEADER, "one more edge case, please", room)

    entry = _last_entry(channel)
    assert entry.sender == "pi-leader"  # localpart fallback, not an MXID
    assert entry.sender_id == LEADER

    with _PatchCtx(channel) as sinks:
        _complete(channel, _placeholder_turn_meta())

    assert sinks["enqueue"].called


def test_irrelevant_peer_reply_delivers_draft_not_retrigger():
    """Case A: peer's parallel reply must not void the pending answer
    (real callback; the peer also has a display name distinct from its
    MXID localpart)."""
    channel = _make_channel()
    channel._user_id = WORKER
    room = _FakeRoom(ROOM, {PEER: PEER_DISPLAY})
    _land(channel, PEER, "AT-R02-test2 peer answer", room)

    with _PatchCtx(channel) as sinks:
        _complete(channel, _placeholder_turn_meta())

    # No re-trigger at all: the original draft is delivered as-is.
    assert not sinks["enqueue"].called
    assert sinks["edit_root"].await_count == 1
    assert sinks["edit_root"].await_args.args[2] == ORIGINAL_DRAFT
    assert not sinks["send"].called
    # ... plus the "new messages arrived" note.
    assert sinks["plain_text"].call_count == 1


def test_mixed_events_one_relevant_retriggers():
    """Irrelevant peer traffic + one relevant event => still retrigger."""
    channel = _make_channel()
    channel._user_id = WORKER
    room = _FakeRoom(ROOM, {LEADER: LEADER_DISPLAY, PEER: PEER_DISPLAY})
    _land(channel, PEER, "unrelated chatter", room)
    _land(channel, LEADER, "follow-up question", room, event_id="evt-in-2")

    with _PatchCtx(channel) as sinks:
        _complete(channel, _placeholder_turn_meta())

    assert sinks["enqueue"].called


def test_irrelevant_reply_without_placeholder_sends_draft():
    """Non-placeholder turn: irrelevant peer traffic => normal send path."""
    channel = _make_channel()
    channel._user_id = WORKER
    room = _FakeRoom(ROOM, {PEER: PEER_DISPLAY})
    _land(channel, PEER, "unrelated chatter", room)
    meta = _placeholder_turn_meta()
    del meta[PLACEHOLDER]

    with _PatchCtx(channel) as sinks:
        _complete(channel, meta)

    assert not sinks["enqueue"].called
    assert sinks["send"].await_count == 1
    assert sinks["send"].await_args.args[1] == ORIGINAL_DRAFT
    assert sinks["plain_text"].call_count == 1


def test_relevance_predicate_mention_arm():
    """Predicate-level test of the mention arm of
    ``_new_events_relevant_to_turn`` (defense-in-depth for retained
    entries that address the worker by name; real mentions of the worker
    are enqueued as their own turns by the callback and never reach the
    history log, so this arm is exercised at the predicate seam)."""
    channel = _make_channel()
    channel._user_id = WORKER
    entry = channel_module.HistoryEntry(
        sender=PEER_DISPLAY, body="hold on @test1 the requirement changed"
    )
    channel._room_event_log[ROOM] = [(1, entry)]

    meta = _placeholder_turn_meta()
    # @worker in the body => relevant.
    assert channel._new_events_relevant_to_turn(ROOM, 0, meta) is True
    # Unrelated body from the same sender => not relevant.
    entry.body = "unrelated chatter"
    assert channel._new_events_relevant_to_turn(ROOM, 0, meta) is False
