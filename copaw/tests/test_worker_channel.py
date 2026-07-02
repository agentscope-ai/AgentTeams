"""Tests for copaw_worker.matrix_channel — leads/workers' MatrixChannel.

This mirrors ``test_channel_mention.py`` (the manager overlay's twin test
file). Nothing tested ``copaw_worker.matrix_channel`` before this file. It
covers the three Step 5 sub-changes:

1. Bare ``@localpart`` mention resolution (tier 4) via a room-members
   localpart cache.
2. Immediate ack — a short direct ``room_send`` sent the instant a
   task/mention is accepted, bypassing the enqueue/agent-processing queue.
3. Catch-up replay — startup messages are buffered (not dropped) while the
   initial sync is running, then replayed through the normal handling path
   once the channel is ready.
"""

import asyncio
import os
from types import SimpleNamespace

from copaw_worker import matrix_channel
from copaw_worker.matrix_channel import MatrixChannel


class _SendClient:
    def __init__(self):
        self.rooms = {}
        self.sent = []

    async def room_send(
        self,
        room_id,
        event_type,
        content,
        ignore_unverified_devices=True,
    ):
        self.sent.append((room_id, event_type, content, ignore_unverified_devices))
        return SimpleNamespace(event_id="$reply")


async def _noop_typing(*_args, **_kwargs):
    return None


async def _noop_read_receipt(*_args, **_kwargs):
    return None


async def _false_dm(*_args, **_kwargs):
    return False


class _FakeCfg:
    history_limit = 50


class _FakeRoom:
    room_id = "!room:hs.local"
    users = {"@alice:hs.local": object(), "@worker:hs.local": object()}

    def user_name(self, user_id):
        if user_id == "@worker:hs.local":
            return "worker"
        return user_id


def _make_channel(user_id: str = "@worker:hs.local") -> MatrixChannel:
    """Build a bare channel instance without going through __init__.

    Mirrors ``test_channel_mention.py``'s ``_make_channel`` — sets only the
    attributes exercised by the methods under test.
    """
    ch = MatrixChannel.__new__(MatrixChannel)
    ch._user_id = user_id
    ch._client = None
    ch._typing_tasks = {}
    ch._localpart_cache = {}
    ch._startup_replay_buffer = []
    return ch


def _make_inbound_channel() -> MatrixChannel:
    ch = _make_channel(user_id="@worker:hs.local")
    ch._cfg = _FakeCfg()
    ch._room_histories = {}
    ch._check_allowed = lambda *_args: True
    ch._require_mention = lambda _room_id: True
    ch._send_read_receipt = _noop_read_receipt
    ch._send_typing = _noop_typing
    ch.enqueued = []
    ch._enqueue = ch.enqueued.append
    return ch


def _event(body: str, mentioned: bool = False):
    """A group-room text event. ``room.users`` has 2 entries in _FakeRoom, so
    the worker's ``is_dm = len(room.users) == 2`` would read as DM — tests
    that need group-room behavior pass a room with 3+ users instead."""
    mentions = {"user_ids": ["@worker:hs.local"]} if mentioned else {}
    return SimpleNamespace(
        sender="@alice:hs.local",
        body=body,
        event_id="$event",
        server_timestamp=0,
        source={"content": {"m.mentions": mentions}},
    )


class _GroupRoom(_FakeRoom):
    """A 3-member room so ``is_dm = len(room.users) == 2`` is False."""

    users = {
        "@alice:hs.local": object(),
        "@bob:hs.local": object(),
        "@worker:hs.local": object(),
    }


def _first_text(payload):
    return payload["content_parts"][0]["text"]


# ---------------------------------------------------------------------------
# Basic inbound routing sanity (nothing tested this module before)
# ---------------------------------------------------------------------------


def test_matrix_mentioned_group_message_is_enqueued():
    ch = _make_inbound_channel()

    asyncio.run(
        ch._on_room_event(_GroupRoom(), _event("worker: please help", mentioned=True)),
    )

    assert len(ch.enqueued) == 1
    assert "please help" in _first_text(ch.enqueued[0])


def test_matrix_unmentioned_group_message_buffered_not_enqueued():
    ch = _make_inbound_channel()

    asyncio.run(ch._on_room_event(_GroupRoom(), _event("just chatting")))

    assert ch.enqueued == []
    assert ch._room_histories  # buffered as history


# ---------------------------------------------------------------------------
# Bare-@mention resolution (tier 4) — room-members localpart cache
# ---------------------------------------------------------------------------


class _RoomMember:
    def __init__(self, user_id):
        self.user_id = user_id


class _JoinedMembersResponse:
    """Stand-in for nio.responses.JoinedMembersResponse."""

    def __init__(self, members):
        self.members = members


class _JoinedMembersClient:
    def __init__(self, members_by_room):
        self._members_by_room = members_by_room
        self.calls = []

    async def joined_members(self, room_id):
        self.calls.append(room_id)
        members = self._members_by_room.get(room_id, [])
        return _JoinedMembersResponse([_RoomMember(m) for m in members])


def _bare_mention_event(body: str):
    """A message event with no m.mentions / formatted_body / full MXID —
    only a bare ``@localpart`` in the plain-text body."""
    return SimpleNamespace(
        sender="@alice:hs.local",
        body=body,
        event_id="$event",
        server_timestamp=0,
        source={"content": {"m.mentions": {}}},
    )


def test_was_mentioned_resolves_bare_localpart_via_room_members():
    ch = _make_channel(user_id="@worker:hs.local")
    ch._client = _JoinedMembersClient(
        {"!room:hs.local": ["@alice:hs.local", "@worker:hs.local"]},
    )
    matrix_channel.JoinedMembersResponse = _JoinedMembersResponse

    event = _bare_mention_event("@worker can you take this?")
    mentioned = asyncio.run(ch._was_mentioned(event, event.body, "!room:hs.local"))

    assert mentioned is True


def test_was_mentioned_bare_localpart_cache_is_ttl_bounded():
    ch = _make_channel(user_id="@worker:hs.local")
    client = _JoinedMembersClient(
        {"!room:hs.local": ["@alice:hs.local", "@worker:hs.local"]},
    )
    ch._client = client
    matrix_channel.JoinedMembersResponse = _JoinedMembersResponse

    event = _bare_mention_event("@worker ping")
    asyncio.run(ch._was_mentioned(event, event.body, "!room:hs.local"))
    asyncio.run(ch._was_mentioned(event, event.body, "!room:hs.local"))

    assert len(client.calls) == 1


def test_was_mentioned_bare_localpart_not_in_room_returns_false():
    ch = _make_channel(user_id="@worker:hs.local")
    ch._client = _JoinedMembersClient(
        {"!room:hs.local": ["@alice:hs.local", "@bob:hs.local"]},
    )
    matrix_channel.JoinedMembersResponse = _JoinedMembersResponse

    event = _bare_mention_event("@someone-else can you take this?")
    mentioned = asyncio.run(ch._was_mentioned(event, event.body, "!room:hs.local"))

    assert mentioned is False


def test_matrix_bare_mention_wakes_handler_in_group_room():
    """End-to-end: a bare ``@localpart`` (no homeserver suffix) in a group
    room wakes the handler and enqueues the message."""
    ch = _make_inbound_channel()
    ch._client = _JoinedMembersClient(
        {
            "!room:hs.local": [
                "@alice:hs.local",
                "@bob:hs.local",
                "@worker:hs.local",
            ],
        },
    )
    matrix_channel.JoinedMembersResponse = _JoinedMembersResponse

    asyncio.run(
        ch._on_room_event(_GroupRoom(), _bare_mention_event("@worker please help")),
    )

    assert len(ch.enqueued) == 1


# ---------------------------------------------------------------------------
# Immediate ack — direct room_send bypassing the queue
# ---------------------------------------------------------------------------


def test_immediate_ack_sent_before_enqueue_by_default(monkeypatch):
    monkeypatch.delenv("HICLAW_CHAT_ACK", raising=False)
    ch = _make_inbound_channel()
    client = _SendClient()
    ch._client = client

    asyncio.run(
        ch._on_room_event(_GroupRoom(), _event("worker: please help", mentioned=True)),
    )

    assert len(ch.enqueued) == 1
    assert len(client.sent) == 1
    assert client.sent[0][0] == "!room:hs.local"
    assert client.sent[0][2]["body"]
    assert "\n" not in client.sent[0][2]["body"]


def test_immediate_ack_disabled_via_env(monkeypatch):
    monkeypatch.setenv("HICLAW_CHAT_ACK", "0")
    ch = _make_inbound_channel()
    client = _SendClient()
    ch._client = client

    asyncio.run(
        ch._on_room_event(_GroupRoom(), _event("worker: please help", mentioned=True)),
    )

    assert len(ch.enqueued) == 1
    assert client.sent == []


def test_immediate_ack_env_false_string_disables(monkeypatch):
    monkeypatch.setenv("HICLAW_CHAT_ACK", "false")
    ch = _make_inbound_channel()
    client = _SendClient()
    ch._client = client

    asyncio.run(
        ch._on_room_event(_GroupRoom(), _event("worker: please help", mentioned=True)),
    )

    assert client.sent == []


def test_immediate_ack_not_sent_when_no_mention():
    """No ack when the message isn't even accepted (unmentioned group msg)."""
    ch = _make_inbound_channel()
    client = _SendClient()
    ch._client = client

    asyncio.run(ch._on_room_event(_GroupRoom(), _event("just chatting")))

    assert ch.enqueued == []
    assert client.sent == []


# ---------------------------------------------------------------------------
# Catch-up replay — buffer suppressed startup messages, replay after ready
# ---------------------------------------------------------------------------


def test_startup_buffer_skips_own_messages():
    ch = _make_channel(user_id="@worker:hs.local")
    ch._startup_replay_buffer = []

    own_event = SimpleNamespace(sender="@worker:hs.local", event_id="$own")
    ch._buffer_startup_event(_FakeRoom(), own_event)

    assert ch._startup_replay_buffer == []


def test_startup_buffer_caps_at_limit():
    ch = _make_channel(user_id="@worker:hs.local")
    ch._startup_replay_buffer = []

    for i in range(matrix_channel.STARTUP_REPLAY_BUFFER_CAP + 10):
        event = SimpleNamespace(sender="@alice:hs.local", event_id=f"$e{i}")
        ch._buffer_startup_event(_FakeRoom(), event)

    assert len(ch._startup_replay_buffer) == matrix_channel.STARTUP_REPLAY_BUFFER_CAP


def test_startup_replay_processes_buffered_messages_through_normal_path():
    """No first-boot message loss: buffered events are replayed through the
    normal _on_room_event handling path once the channel is ready."""
    ch = _make_inbound_channel()
    ch._startup_replay_buffer = [
        (_GroupRoom(), _event("worker: hello", mentioned=True)),
        (_GroupRoom(), _event("worker: second message", mentioned=True)),
    ]

    asyncio.run(ch._replay_startup_buffer())

    assert ch._startup_replay_buffer == []
    assert len(ch.enqueued) == 2


def test_startup_replay_noop_when_buffer_empty():
    ch = _make_inbound_channel()
    ch._startup_replay_buffer = []

    asyncio.run(ch._replay_startup_buffer())

    assert ch.enqueued == []


# ---------------------------------------------------------------------------
# Configurable sync timeout (drift-fix port)
# ---------------------------------------------------------------------------


def test_config_sync_timeout_defaults_to_30s():
    cfg = matrix_channel.MatrixChannelConfig({})
    assert cfg.sync_timeout_ms == matrix_channel.DEFAULT_SYNC_TIMEOUT_MS


def test_config_sync_timeout_overridable():
    cfg = matrix_channel.MatrixChannelConfig({"sync_timeout_ms": 5000})
    assert cfg.sync_timeout_ms == 5000
