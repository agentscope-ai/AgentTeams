"""Unit tests for the per-room history buffer (P9, PR1 M2).

Covers the semantics the channel implementation is pinned to: per-room
injection order, limit truncation (tail retained), per-room isolation,
the gate counters (generation / record count), the "since last mention"
window from the retained event log, and the ``apply_history_to_parts``
prefix/separator format.

The parity section asserts that the channel's original methods return
identical results on the same inputs (the channel is constructed cheaply;
its content-part types are the real qwenpaw ``TextContent``).
"""

from __future__ import annotations

import importlib.util
import sys
from pathlib import Path
from types import SimpleNamespace

import pytest

from core import (
    CURRENT_MESSAGE_MARKER,
    DEFAULT_HISTORY_LIMIT,
    HISTORY_CONTEXT_MARKER,
    HistoryEntry,
    RoomHistory,
)

ROOM_A = "!a:hs.example"
ROOM_B = "!b:hs.example"

REPO_ROOT = Path(__file__).resolve().parents[3]
CHANNEL_MODULE_PATH = (
    REPO_ROOT
    / "plugins"
    / "agentteams-matrix"
    / "adapters"
    / "qwenpaw"
    / "agentteams_matrix"
    / "channel.py"
)


def _entry(sender, body, message_id=None, media_parts=None, sender_id=None):
    return HistoryEntry(
        sender=sender,
        body=body,
        message_id=message_id,
        media_parts=media_parts,
        sender_id=sender_id,
    )


def _text_part(text):
    return SimpleNamespace(type="text", text=text)


def _is_text_part(part):
    return getattr(part, "type", None) == "text"


def _make_text_part(text):
    return SimpleNamespace(type="text", text=text)


# ---------------------------------------------------------------------------
# record / format
# ---------------------------------------------------------------------------


def test_injection_order_and_format():
    h = RoomHistory(limit=5)
    h.record_history(ROOM_A, _entry("alice", "first", message_id="$1"))
    h.record_history(ROOM_A, _entry("bob", "second"))
    h.record_history(ROOM_A, _entry("carol", "third", message_id="$3"))
    assert h.build_history_prefix(ROOM_A) == (
        "alice: first [id:$1]\n"
        "bob: second\n"
        "carol: third [id:$3]"
    )


def test_unknown_room_prefix_empty():
    assert RoomHistory().build_history_prefix(ROOM_A) == ""


def test_limit_truncation_keeps_tail():
    h = RoomHistory(limit=3)
    for i in range(5):
        h.record_history(ROOM_A, _entry(f"s{i}", f"m{i}", message_id=f"${i}"))
    assert h.build_history_prefix(ROOM_A) == (
        "s2: m2 [id:$2]\ns3: m3 [id:$3]\ns4: m4 [id:$4]"
    )


def test_room_isolation():
    h = RoomHistory(limit=10)
    h.record_history(ROOM_A, _entry("a1", "in A"))
    h.record_history(ROOM_B, _entry("b1", "in B", message_id="$b1"))
    assert h.build_history_prefix(ROOM_A) == "a1: in A"
    assert h.build_history_prefix(ROOM_B) == "b1: in B [id:$b1]"
    assert h.generation(ROOM_A) == 1
    assert h.generation(ROOM_B) == 1
    assert h.record_count(ROOM_A) == 1
    assert h.record_count(ROOM_B) == 1


def test_zero_limit_is_noop():
    h = RoomHistory(limit=0)
    h.record_history(ROOM_A, _entry("a", "b"))
    assert h.build_history_prefix(ROOM_A) == ""
    assert h.generation(ROOM_A) == 0
    assert h.record_count(ROOM_A) == 0


def test_negative_limit_clamped_to_zero():
    assert RoomHistory(limit=-5).limit == 0
    assert RoomHistory().limit == DEFAULT_HISTORY_LIMIT == 50


def test_counters_increment_and_clear_keeps_counters():
    h = RoomHistory(limit=10)
    for i in range(3):
        h.record_history(ROOM_A, _entry("s", f"m{i}"))
    assert h.generation(ROOM_A) == 3
    assert h.record_count(ROOM_A) == 3
    h.clear_history(ROOM_A)
    assert h.build_history_prefix(ROOM_A) == ""
    # Clear drops the buffer only; the gate counters keep counting
    # (channel behaviour — the per-turn snapshot is compared against them
    # later, so clearing must not reset them).
    assert h.generation(ROOM_A) == 3
    assert h.record_count(ROOM_A) == 3


# ---------------------------------------------------------------------------
# build_missed_history — "since last mention" window
# ---------------------------------------------------------------------------


def test_missed_history_since_records_window():
    h = RoomHistory(limit=10)
    for i in range(4):
        h.record_history(ROOM_A, _entry(f"s{i}", f"m{i}", message_id=f"${i}"))
    text, _ = h.build_missed_history(ROOM_A, 2)
    assert text == "s2: m2 [id:$2]\ns3: m3 [id:$3]"
    text_all, _ = h.build_missed_history(ROOM_A, 0)
    assert text_all == (
        "s0: m0 [id:$0]\ns1: m1 [id:$1]\ns2: m2 [id:$2]\ns3: m3 [id:$3]"
    )
    assert h.build_missed_history(ROOM_A, 4) == ("", [])
    assert h.build_missed_history(ROOM_A, 100) == ("", [])


def test_missed_history_media_rides_along():
    h = RoomHistory(limit=10)
    h.record_history(ROOM_A, _entry("s0", "m0"))
    h.record_history(ROOM_A, _entry("s1", "m1", media_parts=["img1"]))
    h.record_history(ROOM_A, _entry("s2", "m2", media_parts=["img2a", "img2b"]))
    _, media = h.build_missed_history(ROOM_A, 0)
    assert media == ["img1", "img2a", "img2b"]
    # since_records=1 keeps records after the first one (s1, s2).
    text, media2 = h.build_missed_history(ROOM_A, 1)
    assert text == "s1: m1\ns2: m2"
    assert media2 == ["img1", "img2a", "img2b"]
    text3, media3 = h.build_missed_history(ROOM_A, 2)
    assert text3 == "s2: m2"
    assert media3 == ["img2a", "img2b"]


def test_event_log_capped_like_buffer():
    h = RoomHistory(limit=3)
    for i in range(5):
        h.record_history(ROOM_A, _entry(f"s{i}", f"m{i}", message_id=f"${i}"))
    # A turn in flight longer than the cap loses the head of its window.
    text, _ = h.build_missed_history(ROOM_A, 0)
    assert text == "s2: m2 [id:$2]\ns3: m3 [id:$3]\ns4: m4 [id:$4]"


# ---------------------------------------------------------------------------
# apply_history_to_parts — prefix/separator format
# ---------------------------------------------------------------------------


def test_apply_history_merges_into_leading_text_part():
    h = RoomHistory(limit=10)
    h.record_history(ROOM_A, _entry("alice", "hi there", message_id="$1"))
    parts = [_text_part("now"), _text_part("tail")]
    out = h.apply_history_to_parts(ROOM_A, parts, _is_text_part, _make_text_part)
    expected_combined = (
        f"{HISTORY_CONTEXT_MARKER}\n"
        "alice: hi there [id:$1]\n\n"
        f"{CURRENT_MESSAGE_MARKER}\nnow"
    )
    # The leading text part is REPLACED by the merged part; the rest of the
    # original parts follow (no history media here).
    assert len(out) == 2
    assert out[0].type == "text"
    assert out[0].text == expected_combined
    assert out[1] is parts[1]
    # The original list is not mutated.
    assert len(parts) == 2
    assert parts[0].text == "now"


def test_apply_history_prepend_without_leading_text_part():
    h = RoomHistory(limit=10)
    h.record_history(ROOM_A, _entry("alice", "hi"))
    media = SimpleNamespace(type="image", image_url="mxc://x")
    out = h.apply_history_to_parts(ROOM_A, [media], _is_text_part, _make_text_part)
    assert out[0].text == (
        f"{HISTORY_CONTEXT_MARKER}\nalice: hi\n\n{CURRENT_MESSAGE_MARKER}"
    )
    assert out[1] is media


def test_apply_history_empty_parts():
    h = RoomHistory(limit=10)
    h.record_history(ROOM_A, _entry("alice", "hi"))
    out = h.apply_history_to_parts(ROOM_A, [], _is_text_part, _make_text_part)
    assert len(out) == 1
    assert out[0].text == (
        f"{HISTORY_CONTEXT_MARKER}\nalice: hi\n\n{CURRENT_MESSAGE_MARKER}"
    )


def test_apply_history_inserts_history_media_between_block_and_parts():
    h = RoomHistory(limit=10)
    h.record_history(ROOM_A, _entry("alice", "pic", media_parts=["img1"]))
    h.record_history(ROOM_A, _entry("bob", "pic2", media_parts=["img2"]))
    now = _text_part("now")
    tail = _text_part("tail")
    out = h.apply_history_to_parts(ROOM_A, [now, tail], _is_text_part, _make_text_part)
    # [merged text] + [history media] + [remaining parts]
    assert out[0].text.startswith(HISTORY_CONTEXT_MARKER)
    assert out[1] == "img1"
    assert out[2] == "img2"
    assert out[3] is tail


def test_apply_history_noop_when_empty_history():
    h = RoomHistory(limit=10)
    parts = [_text_part("now")]
    assert (
        h.apply_history_to_parts(ROOM_A, parts, _is_text_part, _make_text_part)
        is parts
    )


def test_apply_history_noop_when_limit_zero():
    h = RoomHistory(limit=0)
    parts = [_text_part("now")]
    assert (
        h.apply_history_to_parts(ROOM_A, parts, _is_text_part, _make_text_part)
        is parts
    )


# ---------------------------------------------------------------------------
# Parity with the QwenPaw channel (original implementation)
# ---------------------------------------------------------------------------


def _load_channel_module(name: str):
    spec = importlib.util.spec_from_file_location(name, CHANNEL_MODULE_PATH)
    assert spec is not None and spec.loader is not None
    mod = importlib.util.module_from_spec(spec)
    sys.modules[name] = mod
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture(scope="module")
def channel_history():
    """(module, channel) with history_limit=3."""
    pytest.importorskip("nio")
    pytest.importorskip("qwenpaw")
    mod = _load_channel_module("p9_parity_channel_history")
    ch = mod.AgentTeamsMatrixChannel(
        process=lambda *a, **k: None, history_limit=3
    )
    return mod, ch


def _feed(mod, ch, core, fields):
    for sender, body, mid in fields:
        ch._record_history(
            ROOM_A,
            mod.HistoryEntry(sender=sender, body=body, message_id=mid),
        )
        core.record_history(
            ROOM_A, HistoryEntry(sender=sender, body=body, message_id=mid)
        )


def test_parity_record_format_and_counters(channel_history):
    mod, ch = channel_history
    core = RoomHistory(limit=3)
    _feed(
        mod,
        ch,
        core,
        [
            ("alice", "m0", "$e0"),
            ("bob", "m1", None),
            ("carol", "m2", "$e2"),
            ("dave", "m3", "$e3"),
            ("erin", "m4", "$e4"),
        ],
    )
    assert ch._build_history_prefix(ROOM_A) == core.build_history_prefix(ROOM_A)
    for since in (0, 1, 2, 4, 5):
        assert ch._build_missed_history(ROOM_A, since) == core.build_missed_history(
            ROOM_A, since
        )
    assert ch.history_limit == core.limit
    assert ch._room_history_gen.get(ROOM_A) == core.generation(ROOM_A)
    assert ch._room_history_records.get(ROOM_A) == core.record_count(ROOM_A)
    # Event log caps identically (limit 3, 5 records).
    assert ch._build_missed_history(ROOM_A, 0) == core.build_missed_history(ROOM_A, 0)
    # Clear drops the buffer but keeps the counters on both sides.
    ch._clear_history(ROOM_A)
    core.clear_history(ROOM_A)
    assert ch._build_history_prefix(ROOM_A) == core.build_history_prefix(ROOM_A) == ""
    assert ch._room_history_gen.get(ROOM_A) == core.generation(ROOM_A)
    assert ch._room_history_records.get(ROOM_A) == core.record_count(ROOM_A)


def test_parity_apply_history_to_parts(channel_history):
    mod, ch = channel_history
    from qwenpaw.schemas import ContentType, TextContent

    core = RoomHistory(limit=3)
    _feed(
        mod,
        ch,
        core,
        [("alice", "m0", "$e0"), ("bob", "m1", None), ("carol", "m2", "$e2")],
    )

    def is_text_part(part):
        return getattr(part, "type", None) == ContentType.TEXT

    def make_text_part(text):
        return TextContent(type=ContentType.TEXT, text=text)

    cases = [
        [TextContent(type=ContentType.TEXT, text="now")],
        [TextContent(type=ContentType.TEXT, text=""), SimpleNamespace(type="image")],
        [SimpleNamespace(type="image")],
        [],
    ]
    for parts in cases:
        got = ch._apply_history_to_parts(ROOM_A, list(parts))
        want = core.apply_history_to_parts(
            ROOM_A, list(parts), is_text_part, make_text_part
        )
        assert len(got) == len(want)
        for g, w in zip(got, want):
            assert g == w, (g, w)


def test_parity_apply_history_noop_identity(channel_history):
    mod, ch = channel_history
    from qwenpaw.schemas import ContentType, TextContent

    core = RoomHistory(limit=3)
    parts = [TextContent(type=ContentType.TEXT, text="now")]
    # No buffered history for ROOM_B on either side: both return the same
    # list object, unchanged.
    assert ch._apply_history_to_parts(ROOM_B, parts) is parts
    assert (
        core.apply_history_to_parts(
            ROOM_B, parts, lambda p: getattr(p, "type", None) == ContentType.TEXT,
            lambda t: TextContent(type=ContentType.TEXT, text=t),
        )
        is parts
    )


def test_parity_zero_limit(channel_history):
    mod, ch = channel_history
    ch2 = mod.AgentTeamsMatrixChannel(
        process=lambda *a, **k: None, history_limit=0
    )
    core = RoomHistory(limit=0)
    ch2._record_history(
        ROOM_A, mod.HistoryEntry(sender="alice", body="m0", message_id="$e0")
    )
    core.record_history(ROOM_A, HistoryEntry(sender="alice", body="m0", message_id="$e0"))
    assert ch2._build_history_prefix(ROOM_A) == core.build_history_prefix(ROOM_A) == ""
    # The channel's snapshot reads `.get(room_id, 0)` for unrecorded rooms.
    assert ch2._room_history_gen.get(ROOM_A, 0) == core.generation(ROOM_A) == 0
    assert ch2._room_history_records.get(ROOM_A, 0) == core.record_count(ROOM_A) == 0
