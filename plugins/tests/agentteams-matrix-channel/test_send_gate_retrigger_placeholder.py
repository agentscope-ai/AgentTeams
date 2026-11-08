"""Send-gate retrigger must preserve the thread-root placeholder state.

Upstream issue #1386, cases A and B: a Worker's in-flight turn creates a
"处理中..." placeholder root on the main timeline. When a new room
event lands after the turn's context snapshot — the legitimate retrigger
case is the trigger sender following up; a follow-up change (scenario A
of #1386, filed separately) will further narrow the retrigger to
relevant events only — the send gate withholds the completed draft and
re-triggers the turn. The
retrigger metadata carried the thread-root ids but NOT the placeholder
flag, so the re-triggered turn completed on the non-placeholder path:

  * Case B: the regenerated final answer was posted as an ``m.thread``
    reply and the main-timeline placeholder was left stranded forever.
  * Case A: the re-triggered turn returned NO_REPLY; the placeholder was
    likewise stranded and the explicit reply obligation silently lost.

These tests pin the contract:

  * the retrigger metadata carries the placeholder flag over, so the
    re-triggered turn finalizes by editing the SAME placeholder root;
  * a NO_REPLY on the re-triggered turn still closes the placeholder
    coherently instead of stranding it.

Run:  pytest plugins/tests/agentteams-matrix-channel/test_send_gate_retrigger_placeholder.py
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
    "agentteams_matrix_channel_1386", CHANNEL_MODULE_PATH
)
assert _spec is not None and _spec.loader is not None
channel_module = importlib.util.module_from_spec(_spec)
sys.modules["agentteams_matrix_channel_1386"] = channel_module
_spec.loader.exec_module(channel_module)

AgentTeamsMatrixChannel = channel_module.AgentTeamsMatrixChannel
HistoryEntry = channel_module.HistoryEntry

OWN_ROOT = channel_module._MATRIX_OWN_THREAD_ROOT_KEY
META_ROOT = channel_module._THREAD_META_ROOT_KEY
PLACEHOLDER = channel_module._MATRIX_PLACEHOLDER_THREAD_ROOT_KEY
STREAMING_FINAL = channel_module._MATRIX_STREAMING_FINAL_TEXT_KEY

ROOM = "!room:matrix.local"
WORKER = "@test1:matrix.local"
PEER = "@test2:matrix.local"
LEADER = "@pi-leader:matrix.local"


def _make_channel() -> AgentTeamsMatrixChannel:
    return AgentTeamsMatrixChannel(
        process=lambda *args, **kw: None,
        share_session_in_group=True,
    )


def _first_turn_meta() -> dict:
    """Meta of the in-flight turn that owns the placeholder root.

    Snapshot taken before the peer message landed: gen 0 / records 0.
    """
    return {
        "room_id": ROOM,
        "is_dm": False,
        "is_group": True,
        "worker_name": "test1",
        "event_id": "evt-turn1",
        "thread_root_event_id": "evt-turn1",
        "sender_id": LEADER,
        "send_gate_snapshot_gen": 0,
        "send_gate_snapshot_records": 0,
        OWN_ROOT: "root-placeholder",
        META_ROOT: "root-placeholder",
        PLACEHOLDER: True,
        STREAMING_FINAL: "first complete draft",
    }


def _relevant_event_lands(channel: AgentTeamsMatrixChannel) -> None:
    """The trigger sender follows up: the legitimate retrigger case.

    (A follow-up change — scenario A of #1386, filed separately — will
    stop *unrelated peer* replies from re-triggering at all.  The
    flag-carry contract is pinned here against the events that still do
    re-trigger under the current behavior.)
    """
    channel._record_history(
        ROOM,
        HistoryEntry(sender=LEADER, body="follow-up: please also cover X"),
    )


class _PatchCtx:
    """Context manager bundle so each test controls its own mocks.

    Patcher objects are started in __enter__ and stopped in __exit__ —
    a nested ``with`` inside __enter__ would undo the patches before the
    test body runs.
    """

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


def test_retrigger_carries_placeholder_flag():
    """The retriggered turn must know it still owns the placeholder."""
    channel = _make_channel()
    channel._user_id = WORKER
    _relevant_event_lands(channel)
    with _PatchCtx(channel) as sinks:
        _complete(channel, _first_turn_meta())

    assert sinks["enqueue"].called, "the gate must retrigger on the peer event"
    retrigger_meta = sinks["enqueue"].call_args.args[0]["meta"]
    # Root ids were already carried (pre-existing behavior, kept):
    assert retrigger_meta.get(OWN_ROOT) == "root-placeholder"
    # THE BUG (#1386 B): the placeholder flag was dropped here.
    assert retrigger_meta.get(PLACEHOLDER) is True


def test_retriggered_turn_edits_same_root_not_thread_reply():
    """Case B: the regenerated final must edit the placeholder in place."""
    channel = _make_channel()
    channel._user_id = WORKER
    _relevant_event_lands(channel)
    with _PatchCtx(channel) as sinks:
        _complete(channel, _first_turn_meta())
        retrigger_meta = sinks["enqueue"].call_args.args[0]["meta"]
        # The re-triggered turn finishes with the regenerated answer.
        retrigger_meta[STREAMING_FINAL] = "AT-R02-test1 final answer"
        _complete(channel, retrigger_meta)

    # Finalized by editing the placeholder root (m.replace), ...
    assert sinks["edit_root"].await_count == 1
    edit_args = sinks["edit_root"].await_args
    assert edit_args.args[2] == "AT-R02-test1 final answer"
    # ... NOT by posting a fresh message/thread reply.
    # (Note: AsyncMock has no real .awaited property in stdlib 3.11 —
    # attribute access auto-generates a truthy child mock.  Use
    # .called / .await_count, which are real attributes.)
    assert not sinks["send"].called
    assert not sinks["send_content"].called


def test_retriggered_no_reply_closes_placeholder_coherently():
    """Case A: NO_REPLY on the retry must not strand the placeholder."""
    channel = _make_channel()
    channel._user_id = WORKER
    _relevant_event_lands(channel)
    with _PatchCtx(channel) as sinks:
        _complete(channel, _first_turn_meta())
        retrigger_meta = sinks["enqueue"].call_args.args[0]["meta"]
        retrigger_meta[STREAMING_FINAL] = "NO_REPLY"
        _complete(channel, retrigger_meta)

    assert sinks["edit_root"].await_count == 1
    assert sinks["edit_root"].await_args.args[2] == "已处理"
    assert not sinks["send"].called


def test_retrigger_without_placeholder_still_threads():
    """Regression: a non-placeholder turn keeps its existing behavior."""
    channel = _make_channel()
    channel._user_id = WORKER
    _relevant_event_lands(channel)
    meta = _first_turn_meta()
    del meta[PLACEHOLDER]  # e.g. long-text fallback root, not a placeholder
    with _PatchCtx(channel) as sinks:
        _complete(channel, meta)
        retrigger_meta = sinks["enqueue"].call_args.args[0]["meta"]
        assert retrigger_meta.get(PLACEHOLDER) is None
        retrigger_meta[STREAMING_FINAL] = "final answer"
        _complete(channel, retrigger_meta)

    # No placeholder -> the normal send path (thread reply to own root).
    assert not sinks["edit_root"].called
    assert sinks["send"].called
    assert sinks["send"].await_count == 1
