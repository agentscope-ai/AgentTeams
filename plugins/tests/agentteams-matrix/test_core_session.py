"""Unit tests for session-id derivation and handle conversion (P9, PR1 M2).

Matrix keys sessions on the room: DMs, group rooms and thread replies
all resolve to ``matrix:<room_id>`` (a thread stays in its parent
room's session; the thread root travels in the channel meta, not in the
session key).  The parity section asserts identical results from the
channel's original methods on the same inputs.
"""

from __future__ import annotations

import importlib.util
import sys
from pathlib import Path

import pytest

from core import resolve_session_id, to_handle_from_target

SENDER = "@bob:hs.example"
DM_ROOM = "!dm:hs.example"
GROUP_ROOM = "!team:hs.example"

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


# ---------------------------------------------------------------------------
# resolve_session_id — table driven
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    ("sender_id", "meta", "expected"),
    [
        # DM: the DM room id is the session key.
        (SENDER, {"room_id": DM_ROOM, "is_dm": True}, f"matrix:{DM_ROOM}"),
        # Group room: room-wide shared session.
        (SENDER, {"room_id": GROUP_ROOM, "is_group": True}, f"matrix:{GROUP_ROOM}"),
        # Thread reply: the session key stays the parent room's; the
        # thread root travels in meta, not in the session id.
        (
            SENDER,
            {
                "room_id": GROUP_ROOM,
                "is_thread_event": True,
                "thread_root_event_id": "$root123",
            },
            f"matrix:{GROUP_ROOM}",
        ),
        # Fallback: no room id in the meta — the sender id is the key.
        (SENDER, {"is_group": False}, f"matrix:{SENDER}"),
        (SENDER, {}, f"matrix:{SENDER}"),
        (SENDER, None, f"matrix:{SENDER}"),
    ],
    ids=["dm", "group-room", "thread", "no-room-id", "empty-meta", "none-meta"],
)
def test_resolve_session_id_table(sender_id, meta, expected):
    assert resolve_session_id(sender_id, meta) == expected


def test_resolve_session_id_explicit_none_room_id():
    # Verbatim channel semantics: ``.get("room_id", sender_id)`` applies
    # the fallback only when the key is ABSENT; an explicit None value is
    # returned as-is (str(None) in the f-string).
    assert resolve_session_id(SENDER, {"room_id": None}) == "matrix:None"


# ---------------------------------------------------------------------------
# to_handle_from_target — table driven
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    ("user_id", "session_id", "expected"),
    [
        (SENDER, f"matrix:{DM_ROOM}", DM_ROOM),
        (SENDER, f"matrix:{GROUP_ROOM}", GROUP_ROOM),
        (SENDER, "plain-session", "plain-session"),
        ("", "matrix:!x:hs", "!x:hs"),
    ],
    ids=["dm-roundtrip", "group-roundtrip", "no-prefix", "empty-user"],
)
def test_to_handle_from_target_table(user_id, session_id, expected):
    assert to_handle_from_target(user_id=user_id, session_id=session_id) == expected


@pytest.mark.parametrize(
    ("meta", "room"),
    [
        ({"room_id": DM_ROOM, "is_dm": True}, DM_ROOM),
        ({"room_id": GROUP_ROOM, "is_group": True}, GROUP_ROOM),
        (
            {
                "room_id": GROUP_ROOM,
                "is_thread_event": True,
                "thread_root_event_id": "$r",
            },
            GROUP_ROOM,
        ),
    ],
    ids=["dm", "group-room", "thread"],
)
def test_session_handle_roundtrip(meta, room):
    session_id = resolve_session_id(SENDER, meta)
    assert to_handle_from_target(user_id=SENDER, session_id=session_id) == room


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
def channel():
    pytest.importorskip("nio")
    pytest.importorskip("qwenpaw")
    mod = _load_channel_module("p9_parity_channel_session")
    return mod.AgentTeamsMatrixChannel(process=lambda *a, **k: None)


def test_parity_resolve_session_id(channel):
    metas = [
        {"room_id": DM_ROOM, "is_dm": True},
        {"room_id": GROUP_ROOM, "is_group": True},
        {
            "room_id": GROUP_ROOM,
            "is_thread_event": True,
            "thread_root_event_id": "$root123",
        },
        {"is_group": False},
        {},
        None,
    ]
    for meta in metas:
        assert channel.resolve_session_id(SENDER, meta) == resolve_session_id(
            SENDER, meta
        )


def test_parity_to_handle_from_target(channel):
    session_ids = [
        f"matrix:{DM_ROOM}",
        f"matrix:{GROUP_ROOM}",
        "plain-session",
        "matrix:None",
    ]
    for session_id in session_ids:
        assert channel.to_handle_from_target(
            user_id=SENDER, session_id=session_id
        ) == to_handle_from_target(user_id=SENDER, session_id=session_id)
