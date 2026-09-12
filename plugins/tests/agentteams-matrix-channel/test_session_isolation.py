#!/usr/bin/env python3
"""Group sender session isolation (``share_session_in_group``) for the
AgentTeams Matrix channel.

Mirrors upstream QwenPaw #7001 semantics on the AgentTeams-owned channel:
QwenPaw keys session state on both ``session_id`` AND ``user_id``, so group
rooms isolate per sender only when the AgentRequest ``user_id`` switches from
``room_id`` to the real sender.

    share_session_in_group=True  (default, legacy) -> user_id = room_id
    share_session_in_group=False                   -> user_id = real sender
    DMs                                            -> user_id = room_id (unchanged)
"""

from __future__ import annotations

import importlib.util
import sys
from pathlib import Path

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
    "agentteams_matrix_channel", CHANNEL_MODULE_PATH
)
assert _spec is not None and _spec.loader is not None
channel_module = importlib.util.module_from_spec(_spec)
# Register before exec: channel.py uses @dataclass with string annotations,
# which resolve the owning module via sys.modules.
sys.modules["agentteams_matrix_channel"] = channel_module
_spec.loader.exec_module(channel_module)

AgentTeamsMatrixChannel = channel_module.AgentTeamsMatrixChannel

ROOM = "!roomid:matrix.local"
LEADER = "@sysdev-lead:matrix.local"
HUMAN = "@luo:matrix.local"


def _make_channel(**overrides):
    kwargs = {
        "process": lambda *args, **kw: None,
        "share_session_in_group": True,
    }
    kwargs.update(overrides)
    return AgentTeamsMatrixChannel(**kwargs)


def _payload(sender: str, *, is_dm: bool = False, room: str = ROOM) -> dict:
    """Native payload in the exact shape built by _on_room_event."""
    return {
        "channel_id": "agentteams_matrix",
        "sender_id": sender,
        "content_parts": [],
        "acl_sender_id": sender,
        "meta": {
            "room_id": room,
            "is_dm": is_dm,
            "is_group": not is_dm,
            "worker_name": "worker-a",
            "sender_id": sender,
        },
    }


def test_group_shared_mode_uses_room_id():
    channel = _make_channel(share_session_in_group=True)
    req = channel.build_agent_request_from_native(_payload(LEADER))
    assert req.user_id == ROOM
    assert req.session_id == f"matrix:{ROOM}"


def test_group_isolated_mode_uses_real_sender():
    channel = _make_channel(share_session_in_group=False)
    req = channel.build_agent_request_from_native(_payload(LEADER))
    assert req.user_id == LEADER
    assert req.session_id == f"matrix:{ROOM}"


def test_group_isolated_different_senders_get_different_sessions():
    """The AgentTeams collaboration risk case: Leader dispatch and a human
    question in the same room must land in separate sessions."""
    channel = _make_channel(share_session_in_group=False)
    leader_req = channel.build_agent_request_from_native(_payload(LEADER))
    human_req = channel.build_agent_request_from_native(_payload(HUMAN))
    assert (leader_req.session_id, leader_req.user_id) != (
        human_req.session_id,
        human_req.user_id,
    )
    assert leader_req.user_id == LEADER
    assert human_req.user_id == HUMAN
    # Same room, same session key prefix — isolation comes from user_id.
    assert leader_req.session_id == human_req.session_id


def test_dm_unchanged_under_isolation():
    channel = _make_channel(share_session_in_group=False)
    req = channel.build_agent_request_from_native(_payload(HUMAN, is_dm=True))
    assert req.user_id == ROOM
    assert req.session_id == f"matrix:{ROOM}"


def test_from_config_parses_share_session_flag():
    channel = AgentTeamsMatrixChannel.from_config(
        process=lambda *args, **kw: None,
        config={
            "homeserver": "http://matrix.example.com",
            "user_id": "@worker-a:matrix.local",
            "access_token": "token",
            "share_session_in_group": False,
        },
    )
    assert channel.share_session_in_group is False


def test_from_config_defaults_to_shared_for_compatibility():
    """Older configs (field absent) must keep the legacy room-wide session."""
    channel = AgentTeamsMatrixChannel.from_config(
        process=lambda *args, **kw: None,
        config={"homeserver": "http://matrix.example.com"},
    )
    assert channel.share_session_in_group is True
