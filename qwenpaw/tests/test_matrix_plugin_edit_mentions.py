"""Direct tests for _edit_matrix_event m.mentions propagation.

The QwenPaw 2.0 runtime loads ``plugins/agentteams-matrix-channel`` (key
``agentteams_matrix``) as its Matrix channel. Its edit path
``_edit_matrix_event`` must attach ``m.mentions`` to BOTH the outer
replacement event and ``m.new_content`` per MSC3952 so notifications
still route after a thread-root edit.
"""

import asyncio
from types import SimpleNamespace
from unittest.mock import MagicMock

import pytest

from agentteams_matrix.channel import (
    AgentTeamsMatrixChannel,
    _MATRIX_OWN_THREAD_ROOT_KEY,
)


class _FakeClient:
    def __init__(self):
        self.sent = []
        self.rooms = {}

    async def room_send(self, room_id, message_type, content, **kwargs):
        self.sent.append((room_id, message_type, content, kwargs))
        return SimpleNamespace(event_id=f"$sent{len(self.sent)}")


def _make_channel(user_id: str = "@lead:hs.local") -> AgentTeamsMatrixChannel:
    ch = AgentTeamsMatrixChannel.__new__(AgentTeamsMatrixChannel)
    ch._user_id = user_id
    ch._client = _FakeClient()
    ch._send_typing = MagicMock()
    ch._room_send_with_retry = ch._client.room_send
    return ch


def test_edit_matrix_event_propagates_mentions_to_outer_and_new_content():
    ch = _make_channel()
    send_meta = {_MATRIX_OWN_THREAD_ROOT_KEY: "$thread-root"}

    asyncio.run(
        ch._edit_matrix_event(
            "!team-room:hs.local",
            "$thread-root",
            "@worker-a:hs.local TASK_ASSIGNED: t-01 - start now.",
        ),
    )

    assert len(ch._client.sent) == 1
    _, msgtype, content, _ = ch._client.sent[0]
    assert msgtype == "m.room.message"
    assert content["m.relates_to"]["rel_type"] == "m.replace"
    assert content["m.relates_to"]["event_id"] == "$thread-root"
    # Outer event carries m.mentions (MSC3952).
    assert content["m.mentions"] == {
        "user_ids": ["@worker-a:hs.local"],
    }
    # m.new_content also carries m.mentions.
    assert content["m.new_content"]["m.mentions"] == {
        "user_ids": ["@worker-a:hs.local"],
    }
    # Both bodies mention the user (via display-name replacement; the
    # structured m.mentions keeps the raw MXID).
    assert "worker-a" in content["body"]
    assert "worker-a" in content["m.new_content"]["body"]


def test_edit_matrix_event_no_mention_no_m_mentions():
    ch = _make_channel()

    asyncio.run(
        ch._edit_matrix_event(
            "!team-room:hs.local",
            "$thread-root",
            "Status update: task t-01 is in progress.",
        ),
    )

    assert len(ch._client.sent) == 1
    content = ch._client.sent[0][2]
    assert "m.mentions" not in content
    assert "m.mentions" not in content["m.new_content"]
