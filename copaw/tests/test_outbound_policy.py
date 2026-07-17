"""Tests for matrix.outbound_policy — Team Leader send filtering."""

from __future__ import annotations

import asyncio
from types import SimpleNamespace

from matrix.channel import MatrixChannel
from matrix.outbound_policy import (
    OutboundFilterPolicy,
    ends_with_no_reply_control,
    is_team_leader_internal_preamble_text,
)


def _write_team_leader_runtime(tmp_path):
    working_dir = tmp_path / "leader" / ".copaw"
    runtime_dir = tmp_path / "leader" / "runtime"
    runtime_dir.mkdir(parents=True)
    (runtime_dir / "runtime.yaml").write_text(
        "kind: MemberRuntimeConfig\n"
        "member:\n"
        "  role: team_leader\n"
        "team:\n"
        "  name: dag-team-1\n"
        "  teamRoomId: \"!team-room:hs.local\"\n"
        "  leaderDmRoomId: \"!leader-dm:hs.local\"\n",
        encoding="utf-8",
    )
    return working_dir


def test_ends_with_no_reply_control():
    assert ends_with_no_reply_control("NO_REPLY")
    assert ends_with_no_reply_control("thinking\nNO_REPLY")
    assert not ends_with_no_reply_control("hello")


def test_team_leader_internal_preamble_detection():
    assert is_team_leader_internal_preamble_text(
        "Let me read the relevant skill documentation.",
    )
    assert not is_team_leader_internal_preamble_text(
        "@dev:hs.local Task assigned: implement the API.",
    )


def test_resolve_destination_room_reroutes_assignment(tmp_path, monkeypatch):
    monkeypatch.setenv(
        "COPAW_WORKING_DIR",
        str(_write_team_leader_runtime(tmp_path)),
    )
    policy = OutboundFilterPolicy(user_id="@dag-team-1-lead:hs.local")
    room = policy.resolve_destination_room(
        "!leader-dm:hs.local",
        "@dag-team-1-dev:hs.local Task assigned: implement the API.",
    )
    assert room == "!team-room:hs.local"


class _FakeClient:
    def __init__(self):
        self.sent = []

    async def room_send(self, room_id, message_type, content, **kwargs):
        self.sent.append((room_id, message_type, content, kwargs))
        return SimpleNamespace(event_id="$sent")


async def _noop_typing(_room_id, _typing):
    return None


def _make_channel(user_id: str = "@dag-team-1-lead:hs.local") -> MatrixChannel:
    ch = MatrixChannel.__new__(MatrixChannel)
    ch._user_id = user_id
    ch._client = _FakeClient()
    ch._send_typing = _noop_typing
    ch._outbound_policy = OutboundFilterPolicy(user_id=user_id)
    return ch


def test_unified_channel_suppresses_team_leader_dm_preamble(
    tmp_path,
    monkeypatch,
):
    monkeypatch.setenv(
        "COPAW_WORKING_DIR",
        str(_write_team_leader_runtime(tmp_path)),
    )
    ch = _make_channel()

    asyncio.run(
        MatrixChannel.send(
            ch,
            "!leader-dm:hs.local",
            "I'll coordinate the team. Let me first check the team organization.",
        ),
    )

    assert ch._client.sent == []


def test_unified_channel_keeps_team_assignment_reroute(tmp_path, monkeypatch):
    monkeypatch.setenv(
        "COPAW_WORKING_DIR",
        str(_write_team_leader_runtime(tmp_path)),
    )
    policy = OutboundFilterPolicy(user_id="@dag-team-1-lead:hs.local")
    room = policy.resolve_destination_room(
        "!leader-dm:hs.local",
        "@dag-team-1-dev:hs.local Task assigned: implement the API.",
    )
    assert room == "!team-room:hs.local"
