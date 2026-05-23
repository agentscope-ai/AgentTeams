"""Tests for MatrixChannel Matrix-specific outgoing behavior.

openclaw >= 2026.4.x's mention monitor requires BOTH ``m.mentions.user_ids``
metadata AND a *visible* mention (a ``matrix.to`` link in ``formatted_body``
or a regex match on the agent's identity) — a metadata-only mention is
silently dropped with ``reason: "no-mention"``. These tests pin down the
three-layer invariant that ``_apply_mention`` must uphold so CoPaw-issued
messages actually wake up receiving OpenClaw agents.
"""

import asyncio
from types import SimpleNamespace

from matrix import channel as matrix_channel
from matrix.channel import MatrixChannel


class _TypingClient:
    def __init__(self):
        self.rooms = {}
        self.calls = []

    async def room_typing(self, room_id, *, typing_state, timeout):
        self.calls.append((room_id, typing_state, timeout))


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
        self.sent.append(
            (room_id, event_type, content, ignore_unverified_devices),
        )
        return SimpleNamespace(event_id="$reply")


async def _noop_typing(*_args, **_kwargs):
    return None


def _make_channel(user_id: str = "@bot:hs.local") -> MatrixChannel:
    """Build a bare channel instance without going through __init__.

    ``MatrixChannel.__init__`` wires up BaseChannel/AsyncClient state we do
    not need here; ``_apply_mention`` only touches ``self._user_id`` and
    ``self._client`` (via ``_resolve_display_name``). Setting
    ``_client = None`` forces the display-name resolver down its localpart
    fallback, which keeps these tests deterministic.
    """
    ch = MatrixChannel.__new__(MatrixChannel)
    ch._user_id = user_id
    ch._client = None
    ch._typing_tasks = {}
    return ch


def _make_typing_channel() -> tuple[MatrixChannel, _TypingClient]:
    ch = _make_channel()
    client = _TypingClient()
    ch._client = client
    return ch, client


def test_apply_mention_explicit_user_ids_prefixes_body_and_adds_anchor():
    ch = _make_channel()
    content = {
        "msgtype": "m.text",
        "body": "Please handle this.",
        "format": "org.matrix.custom.html",
        "formatted_body": "<p>Please handle this.</p>",
    }

    ch._apply_mention(
        content,
        "!room:hs.local",
        explicit_user_ids=["@worker-a:hs.local"],
    )

    assert content["m.mentions"] == {"user_ids": ["@worker-a:hs.local"]}
    # body should use display name (localpart fallback), not full MXID
    assert content["body"].startswith("worker-a ")
    assert "@worker-a:hs.local" not in content["body"]
    assert (
        'href="https://matrix.to/#/%40worker-a%3Ahs.local"'
        in content["formatted_body"]
    )
    assert content["format"] == "org.matrix.custom.html"


def test_apply_mention_fallback_sender_id_when_no_explicit_list():
    ch = _make_channel()
    content = {
        "msgtype": "m.text",
        "body": "Got it, thanks!",
        "format": "org.matrix.custom.html",
        "formatted_body": "<p>Got it, thanks!</p>",
    }

    ch._apply_mention(
        content,
        "!room:hs.local",
        fallback_user_id="@alice:hs.local",
    )

    assert content["m.mentions"] == {"user_ids": ["@alice:hs.local"]}
    # body should use display name (localpart fallback), not full MXID
    assert "alice" in content["body"]
    assert "@alice:hs.local" not in content["body"]
    assert (
        'href="https://matrix.to/#/%40alice%3Ahs.local"'
        in content["formatted_body"]
    )


def test_apply_mention_body_scan_rewrites_existing_mxid_to_anchor():
    ch = _make_channel()
    content = {
        "msgtype": "m.text",
        "body": "@worker-b:hs.local hello there",
        "format": "org.matrix.custom.html",
        "formatted_body": "<p>@worker-b:hs.local hello there</p>",
    }

    ch._apply_mention(content, "!room:hs.local")

    assert content["m.mentions"] == {"user_ids": ["@worker-b:hs.local"]}
    # Body MXID should be replaced with display name (localpart fallback).
    assert "worker-b" in content["body"]
    assert "@worker-b:hs.local" not in content["body"]
    # First occurrence in formatted_body is replaced with a matrix.to anchor.
    assert (
        'href="https://matrix.to/#/%40worker-b%3Ahs.local"'
        in content["formatted_body"]
    )


def test_apply_mention_explicit_overrides_sender_fallback():
    ch = _make_channel()
    content = {
        "msgtype": "m.text",
        "body": "move to next",
        "format": "org.matrix.custom.html",
        "formatted_body": "<p>move to next</p>",
    }

    ch._apply_mention(
        content,
        "!room:hs.local",
        explicit_user_ids=["@worker-c:hs.local"],
        fallback_user_id="@alice:hs.local",
    )

    assert content["m.mentions"] == {"user_ids": ["@worker-c:hs.local"]}
    assert "@alice:hs.local" not in content["body"]


def test_apply_mention_skips_self_mention():
    """The agent must never mention itself — that would loop on its own reply."""
    ch = _make_channel(user_id="@bot:hs.local")
    content = {
        "msgtype": "m.text",
        "body": "hello",
        "format": "org.matrix.custom.html",
        "formatted_body": "<p>hello</p>",
    }

    ch._apply_mention(
        content,
        "!room:hs.local",
        explicit_user_ids=["@bot:hs.local"],
    )

    assert "m.mentions" not in content


def test_apply_mention_no_targets_leaves_content_untouched():
    ch = _make_channel()
    content = {
        "msgtype": "m.text",
        "body": "plain chatter with no mention",
        "format": "org.matrix.custom.html",
        "formatted_body": "<p>plain chatter with no mention</p>",
    }
    snapshot = dict(content)

    ch._apply_mention(content, "!room:hs.local")

    assert content == snapshot


def test_apply_mention_synthesizes_formatted_body_for_media_events():
    """``send_media`` only sets ``body`` (filename); mention must still land."""
    ch = _make_channel()
    content = {
        "msgtype": "m.image",
        "body": "screenshot.png",
        "url": "mxc://hs.local/abc",
        "info": {"mimetype": "image/png", "size": 0},
    }

    ch._apply_mention(
        content,
        "!room:hs.local",
        explicit_user_ids=["@worker-d:hs.local"],
    )

    assert content["format"] == "org.matrix.custom.html"
    # body should use display name (localpart fallback), not full MXID
    assert content["body"].startswith("worker-d ")
    assert "@worker-d:hs.local" not in content["body"]
    assert (
        'href="https://matrix.to/#/%40worker-d%3Ahs.local"'
        in content["formatted_body"]
    )
    assert content["m.mentions"] == {"user_ids": ["@worker-d:hs.local"]}


def test_apply_mention_multiple_targets_all_get_visible_anchors():
    ch = _make_channel()
    content = {
        "msgtype": "m.text",
        "body": "team syncing",
        "format": "org.matrix.custom.html",
        "formatted_body": "<p>team syncing</p>",
    }

    ch._apply_mention(
        content,
        "!room:hs.local",
        explicit_user_ids=["@worker-a:hs.local", "@worker-b:hs.local"],
    )

    assert content["m.mentions"] == {
        "user_ids": ["@worker-a:hs.local", "@worker-b:hs.local"],
    }
    for uid_enc in (
        "%40worker-a%3Ahs.local",
        "%40worker-b%3Ahs.local",
    ):
        assert (
            f'href="https://matrix.to/#/{uid_enc}"'
            in content["formatted_body"]
        )


def test_process_completed_stops_typing_even_without_reply():
    ch, client = _make_typing_channel()

    asyncio.run(ch._on_process_completed(None, "!room:hs.local", {}))

    assert client.calls[-1][0] == "!room:hs.local"
    assert client.calls[-1][1] is False


def test_cancelled_consume_error_stops_typing_without_matrix_noise():
    ch, client = _make_typing_channel()

    asyncio.run(
        ch._on_consume_error(
            None,
            "!room:hs.local",
            "Task has been cancelled",
        )
    )

    assert client.calls[-1][0] == "!room:hs.local"
    assert client.calls[-1][1] is False


class _FakeCommandRegistry:
    def is_control_command(self, text):
        return text.strip().lower().split(None, 1)[0] in {
            "/stop",
            "/approve",
        }


class _FakeCfg:
    history_limit = 50


class _FakeContentType:
    TEXT = "text"


class _FakeTextContent:
    def __init__(self, type, text):
        self.type = type
        self.text = text


class _FakeRoom:
    room_id = "!room:hs.local"
    users = {}

    def user_name(self, user_id):
        if user_id == "@copywriting-assistant:hs.local":
            return "copywriting-assistant"
        return user_id


async def _false_dm(_room_id, _sender_id):
    return False


async def _noop_read_receipt(_room_id, _event_id):
    return None


def _make_inbound_channel() -> MatrixChannel:
    if not hasattr(matrix_channel, "TextContent"):
        matrix_channel.TextContent = _FakeTextContent
        matrix_channel.ContentType = _FakeContentType

    ch = _make_channel(user_id="@copywriting-assistant:hs.local")
    ch._cfg = _FakeCfg()
    ch._room_histories = {}
    ch._command_registry = _FakeCommandRegistry()
    ch._is_dm_room = _false_dm
    ch._check_allowed = lambda *_args: True
    ch._require_mention = lambda _room_id: True
    ch._send_read_receipt = _noop_read_receipt
    ch._send_typing = _noop_typing
    ch.enqueued = []
    ch._enqueue = ch.enqueued.append
    return ch


def _event(body: str, mentioned: bool = False, thread: bool = False):
    mentions = (
        {"user_ids": ["@copywriting-assistant:hs.local"]}
        if mentioned
        else {}
    )
    content = {"m.mentions": mentions}
    if thread:
        content["m.relates_to"] = {"rel_type": "m.thread"}
    return SimpleNamespace(
        sender="@alice:hs.local",
        body=body,
        event_id="$event",
        server_timestamp=0,
        source={"content": content},
    )


def _first_text(payload):
    return payload["content_parts"][0].text


def test_matrix_control_command_strips_mention_before_enqueue():
    ch = _make_inbound_channel()

    asyncio.run(
        ch._on_room_event(
            _FakeRoom(),
            _event("copywriting-assistant: /stop", mentioned=True),
        ),
    )

    assert len(ch.enqueued) == 1
    assert _first_text(ch.enqueued[0]) == "/stop"


def test_matrix_bare_stop_not_recognized_without_slash():
    ch = _make_inbound_channel()

    asyncio.run(
        ch._on_room_event(
            _FakeRoom(),
            _event("copywriting-assistant: stop", mentioned=True),
        ),
    )

    assert len(ch.enqueued) == 1
    assert "/stop" not in _first_text(ch.enqueued[0])


def test_matrix_control_command_requires_mention_in_group():
    ch = _make_inbound_channel()

    asyncio.run(ch._on_room_event(_FakeRoom(), _event("/approve")))

    assert len(ch.enqueued) == 0


def test_matrix_unmentioned_thread_message_does_not_pollute_history():
    ch = _make_inbound_channel()

    asyncio.run(ch._on_room_event(_FakeRoom(), _event("side thread", thread=True)))

    assert len(ch.enqueued) == 0
    assert ch._room_histories == {}


def test_matrix_double_slash_stop_normalized_with_mention():
    ch = _make_inbound_channel()

    asyncio.run(
        ch._on_room_event(
            _FakeRoom(),
            _event("copywriting-assistant: //stop", mentioned=True),
        ),
    )

    assert len(ch.enqueued) == 1
    assert _first_text(ch.enqueued[0]) == "/stop"


def test_matrix_readiness_probe_replies_directly_without_enqueue():
    ch = _make_inbound_channel()
    client = _SendClient()
    ch._client = client

    asyncio.run(
        ch._on_room_event(
            _FakeRoom(),
            _event(
                "copywriting-assistant: Readiness check: please reply with "
                "the exact text READY.",
                mentioned=True,
            ),
        ),
    )

    assert ch.enqueued == []
    assert len(client.sent) == 1
    assert client.sent[0][0] == "!room:hs.local"
    assert client.sent[0][2]["body"] == "READY"


def test_matrix_readiness_probe_bypasses_allowlist_when_targeted():
    ch = _make_inbound_channel()
    client = _SendClient()
    ch._client = client
    ch._check_allowed = lambda *_args: False

    asyncio.run(
        ch._on_room_event(
            _FakeRoom(),
            _event(
                "@copywriting-assistant:hs.local Readiness check: please "
                "reply with the exact text READY.",
            ),
        ),
    )

    assert ch.enqueued == []
    assert len(client.sent) == 1
    assert client.sent[0][2]["body"] == "READY"
