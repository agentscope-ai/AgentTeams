"""Tests for MatrixChannel outgoing visible mentions.

openclaw >= 2026.4.x's mention monitor requires BOTH ``m.mentions.user_ids``
metadata AND a *visible* mention (a ``matrix.to`` link in ``formatted_body``
or a regex match on the agent's identity) — a metadata-only mention is
silently dropped with ``reason: "no-mention"``. These tests pin down the
three-layer invariant that ``_apply_mention`` must uphold so CoPaw-issued
messages actually wake up receiving OpenClaw agents.
"""

import asyncio
from types import SimpleNamespace

import matrix.channel as matrix_channel
from matrix.channel import MatrixChannel


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
    return ch


class _FakeClient:
    def __init__(self):
        self.rooms = {}
        self.sent = []

    async def room_send(self, room_id, message_type, content, **kwargs):
        self.sent.append((room_id, message_type, content, kwargs))
        return SimpleNamespace(event_id=f"$sent{len(self.sent)}")


async def _noop_typing(_room_id, _typing):
    return None


def test_team_leader_assignment_in_dm_routes_to_team_room(tmp_path, monkeypatch):
    working_dir = tmp_path / "leader" / ".copaw"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
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

    ch = _make_channel("@dag-team-1-lead:hs.local")
    client = _FakeClient()
    ch._client = client
    ch._send_typing = _noop_typing

    asyncio.run(
        ch.send(
            "!leader-dm:hs.local",
            "@dag-team-1-dev:hs.local Task assigned: implement the API.",
            {"sender_id": "@admin:hs.local"},
        ),
    )

    assert client.sent[0][0] == "!team-room:hs.local"
    assert client.sent[0][2]["m.mentions"] == {
        "user_ids": ["@dag-team-1-dev:hs.local"],
    }
    assert client.sent[0][2]["body"].startswith("@dag-team-1-dev:hs.local ")
    assert "admin " not in client.sent[0][2]["body"]


def test_team_leader_dm_internal_preambles_are_suppressed(tmp_path, monkeypatch):
    working_dir = tmp_path / "leader" / ".copaw"
    monkeypatch.setenv("COPAW_WORKING_DIR", str(working_dir))
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

    ch = _make_channel("@dag-team-1-lead:hs.local")
    client = _FakeClient()
    ch._client = client
    ch._send_typing = _noop_typing

    for text in (
        "Now I need to notify the dev worker in the team room.",
        "I'll coordinate the team to build a REST API for a todo-list app. "
        "Let me start by checking the team organization and then plan the project.",
        "Good, I have a thorough understanding of all the skills. "
        "Now let me check the team organization and available workers.",
        "I have 2 workers available: a dev worker and a QA worker. "
        "Now let me design the DAG plan and create the project.",
        "Good. I have the team roster:\n"
        "- **Dev worker**: `dag-team-1-dev` (`@dag-team-1-dev:hs.local`)\n"
        "- **QA worker**: `dag-team-1-qa` (`@dag-team-1-qa:hs.local`)\n\n"
        "Let me plan the project using DAG strategy.",
    ):
        asyncio.run(ch.send("!leader-dm:hs.local", text))

    assert client.sent == []


def test_team_leader_identity_suppresses_preamble_without_runtime_config(
    tmp_path,
    monkeypatch,
):
    monkeypatch.setenv("COPAW_WORKING_DIR", str(tmp_path / "missing" / ".copaw"))

    ch = _make_channel("@dag-team-1-lead:hs.local")
    client = _FakeClient()
    ch._client = client
    ch._send_typing = _noop_typing

    asyncio.run(
        ch.send(
            "!leader-dm:hs.local",
            "I'll coordinate a team to build this REST API. "
            "Let me first check my team's organization and then plan the work properly.",
        ),
    )

    assert client.sent == []


def test_team_leader_thread_root_edit_suppresses_preamble_without_runtime_config(
    tmp_path,
    monkeypatch,
):
    monkeypatch.setenv("COPAW_WORKING_DIR", str(tmp_path / "missing" / ".copaw"))

    ch = _make_channel("@dag-team-1-lead:hs.local")
    client = _FakeClient()
    ch._client = client
    ch._send_typing = _noop_typing

    asyncio.run(
        ch._edit_thread_root(
            "!leader-dm:hs.local",
            {"matrix_own_thread_root_event_id": "$root"},
            "Good. I have two workers available. Now let me plan this project.",
            msgtype="m.text",
        ),
    )

    assert client.sent == []


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
    assert content["body"].startswith("@worker-a:hs.local ")
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
    assert "@alice:hs.local" in content["body"]
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
    # Body already had the MXID — no duplicate prefix.
    assert content["body"].count("@worker-b:hs.local") == 1
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
    assert content["body"].startswith("@worker-d:hs.local ")
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


def test_send_does_not_auto_mention_sender_id():
    ch = _make_channel()
    client = _FakeClient()
    ch._client = client
    ch._send_typing = _noop_typing

    asyncio.run(
        ch.send(
            "!room:hs.local",
            "Got it, thanks!",
            {"sender_id": "@alice:hs.local"},
        ),
    )

    content = client.sent[0][2]
    assert "m.mentions" not in content
    assert "@alice:hs.local" not in content["body"]


def test_send_enriches_visible_body_mentions():
    ch = _make_channel()
    client = _FakeClient()
    ch._client = client
    ch._send_typing = _noop_typing

    asyncio.run(
        ch.send(
            "!room:hs.local",
            "@alice:hs.local please review this.",
            {"sender_id": "@bob:hs.local"},
        ),
    )

    content = client.sent[0][2]
    assert content["m.mentions"] == {"user_ids": ["@alice:hs.local"]}
    assert "@bob:hs.local" not in content["body"]


def test_send_suppresses_no_reply_as_final_control_line():
    ch = _make_channel()
    client = _FakeClient()
    ch._client = client
    ch._send_typing = _noop_typing

    asyncio.run(
        ch.send(
            "!room:hs.local",
            (
                "任务已完成并报告。\n"
                "已通过 @mention 向 coordinator 报告 TASK_COMPLETED。\n\n"
                "NO_REPLY\n"
            ),
        ),
    )

    assert client.sent == []


def test_send_keeps_no_reply_when_not_final_control_line():
    ch = _make_channel()
    client = _FakeClient()
    ch._client = client
    ch._send_typing = _noop_typing

    asyncio.run(
        ch.send(
            "!room:hs.local",
            "The literal token NO_REPLY is documented here, then normal text.",
        ),
    )

    assert len(client.sent) == 1
    assert "NO_REPLY" in client.sent[0][2]["body"]


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
    DATA = "data"
    FILE = "file"


class _FakeTextContent:
    def __init__(self, type, text):
        self.type = type
        self.text = text


class _FakeRoom:
    room_id = "!room:hs.local"
    users = {}

    def user_name(self, user_id):
        if user_id == "@copywriting-assistant:hs.local":
            return "copywriting-assistant 💕"
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


def _event(body: str, mentioned: bool = False):
    mentions = (
        {"user_ids": ["@copywriting-assistant:hs.local"]}
        if mentioned
        else {}
    )
    return SimpleNamespace(
        sender="@alice:hs.local",
        body=body,
        event_id="$event",
        server_timestamp=0,
        source={"content": {"m.mentions": mentions}},
    )


def _first_text(payload):
    return payload["content_parts"][0].text


def test_matrix_control_command_strips_mention_before_enqueue():
    ch = _make_inbound_channel()

    asyncio.run(
        ch._on_room_event(
            _FakeRoom(),
            _event("copywriting-assistant 💕: /stop", mentioned=True),
        ),
    )

    assert len(ch.enqueued) == 1
    assert _first_text(ch.enqueued[0]) == "/stop"


def test_matrix_bare_stop_not_recognized_without_slash():
    """Bare 'stop' (no leading /) is not a control command even with mention."""
    ch = _make_inbound_channel()

    asyncio.run(
        ch._on_room_event(
            _FakeRoom(),
            _event("copywriting-assistant 💕: stop", mentioned=True),
        ),
    )

    assert len(ch.enqueued) == 1
    # Bare 'stop' goes through as normal text, not converted to /stop
    assert "/stop" not in _first_text(ch.enqueued[0])


def test_matrix_control_command_requires_mention_in_group():
    """Control commands in group rooms require @mention (no bypass)."""
    ch = _make_inbound_channel()

    asyncio.run(ch._on_room_event(_FakeRoom(), _event("/approve")))

    # Without mention, /approve is recorded as history, not enqueued
    assert len(ch.enqueued) == 0


def test_matrix_double_slash_stop_normalized_with_mention():
    """Element-style //stop is normalized to /stop when mentioned."""
    ch = _make_inbound_channel()

    asyncio.run(
        ch._on_room_event(
            _FakeRoom(),
            _event("copywriting-assistant 💕: //stop", mentioned=True),
        ),
    )

    assert len(ch.enqueued) == 1
    assert _first_text(ch.enqueued[0]) == "/stop"


def test_matrix_readiness_probe_replies_directly_without_enqueue():
    ch = _make_inbound_channel()
    client = _FakeClient()
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
    client = _FakeClient()
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


def test_matrix_suppresses_cancelled_task_error_message():
    ch = _make_channel()
    client = _FakeClient()
    ch._client = client

    asyncio.run(
        ch._on_consume_error(
            SimpleNamespace(channel_meta={}),
            "!room:hs.local",
            "Error: Task has been cancelled!",
        ),
    )

    assert client.sent == []


def test_send_hides_matrix_session_id_in_stop_response():
    ch = _make_channel()
    client = _FakeClient()
    ch._client = client
    ch._send_typing = _noop_typing

    asyncio.run(
        ch.send(
            "!room:hs.local",
            "**Task Stopped**\n\n"
            "Session `matrix:!room:hs.local`: running task stopped.",
            {},
        ),
    )

    content = client.sent[0][2]
    assert "matrix:!room:hs.local" not in content["body"]
    assert content["body"] == "**Task Stopped**\n\nRunning task stopped."


def test_send_with_matrix_thread_meta_adds_thread_relation():
    ch = _make_channel()
    client = _FakeClient()
    ch._client = client
    ch._send_typing = _noop_typing

    asyncio.run(
        ch.send(
            "!room:hs.local",
            "reading files",
            {"matrix_thread_root_event_id": "$task-root"},
        ),
    )

    content = client.sent[0][2]
    assert content["m.relates_to"] == {
        "rel_type": "m.thread",
        "event_id": "$task-root",
        "is_falling_back": True,
        "m.in_reply_to": {"event_id": "$task-root"},
    }


def test_send_with_plain_thread_root_meta_stays_in_main_timeline():
    ch = _make_channel()
    client = _FakeClient()
    ch._client = client
    ch._send_typing = _noop_typing

    asyncio.run(
        ch.send(
            "!room:hs.local",
            "final result",
            {"thread_root_event_id": "$task-root"},
        ),
    )

    content = client.sent[0][2]
    assert "m.relates_to" not in content


def test_reasoning_completed_message_waits_for_own_first_message_thread():
    ch = _make_channel()
    client = _FakeClient()
    ch._client = client
    ch._send_typing = _noop_typing
    ch._message_to_content_parts = lambda _event: [
        _FakeTextContent(matrix_channel.ContentType.TEXT, "thinking")
    ]

    async def _send_content_parts(to_handle, parts, meta):
        await ch.send(to_handle, parts[0].text, meta)

    ch.send_content_parts = _send_content_parts
    event = SimpleNamespace(type="MessageType.REASONING")
    meta = {"thread_root_event_id": "$incoming-task"}

    asyncio.run(
        ch.on_event_message_completed(
            SimpleNamespace(),
            "!room:hs.local",
            event,
            meta,
        ),
    )
    assert client.sent == []

    asyncio.run(ch.send("!room:hs.local", "收到任务", meta))

    assert client.sent[0][2]["body"] == "收到任务"
    assert "m.relates_to" not in client.sent[0][2]
    assert client.sent[1][2]["body"] == "thinking"
    assert client.sent[1][2]["m.relates_to"]["event_id"] == "$sent1"
    assert client.sent[1][2]["m.relates_to"]["event_id"] != "$incoming-task"


def test_final_message_completed_stays_in_main_timeline():
    ch = _make_channel()
    captured = []

    async def _capture_send_message_content(to_handle, event, meta):
        captured.append((to_handle, event, meta))

    ch.send_message_content = _capture_send_message_content
    event = SimpleNamespace(type="MessageType.MESSAGE")

    asyncio.run(
        ch.on_event_message_completed(
            SimpleNamespace(),
            "!room:hs.local",
            event,
            {"thread_root_event_id": "$task-root"},
        ),
    )

    assert captured[0][0] == "!room:hs.local"
    assert "matrix_thread_root_event_id" not in captured[0][2]


def test_first_and_final_messages_stay_main_middle_message_goes_thread():
    ch = _make_channel()
    main = []
    thread = []

    async def _capture_main(to_handle, event, meta):
        main.append((to_handle, event.text))
        meta.setdefault("matrix_own_thread_root_event_id", "$first")

    async def _capture_thread(to_handle, parts, meta):
        thread.append((
            to_handle,
            parts[0].text,
            meta.get("matrix_thread_root_event_id"),
        ))

    ch.send_message_content = _capture_main
    ch.send_content_parts = _capture_thread
    ch._message_to_content_parts = lambda event: [
        _FakeTextContent(matrix_channel.ContentType.TEXT, event.text)
    ]
    meta = {"thread_root_event_id": "$incoming-task"}

    for text in ("first", "middle", "final"):
        asyncio.run(
            ch.on_event_message_completed(
                SimpleNamespace(),
                "!room:hs.local",
                SimpleNamespace(type="MessageType.MESSAGE", text=text),
                meta,
            ),
        )

    asyncio.run(ch._on_process_completed(SimpleNamespace(), "!room:hs.local", meta))

    assert main == [
        ("!room:hs.local", "first"),
        ("!room:hs.local", "final"),
    ]
    assert thread == [("!room:hs.local", "middle", "$first")]


def test_pending_message_flushes_before_following_tool_call():
    ch = _make_channel()
    main = []
    thread = []

    async def _capture_main(to_handle, event, meta):
        main.append((to_handle, event.text))
        meta.setdefault("matrix_own_thread_root_event_id", "$first")

    async def _capture_thread(to_handle, parts, meta):
        thread.append((
            to_handle,
            parts[0].text,
            meta.get("matrix_thread_root_event_id"),
        ))

    ch.send_message_content = _capture_main
    ch.send_content_parts = _capture_thread
    ch._message_to_content_parts = lambda event: [
        _FakeTextContent(matrix_channel.ContentType.TEXT, event.text)
    ]
    meta = {"thread_root_event_id": "$incoming-task"}

    events = [
        SimpleNamespace(type="MessageType.MESSAGE", text="first"),
        SimpleNamespace(type="MessageType.MESSAGE", text="准备提交任务"),
        SimpleNamespace(type="MessageType.MCP_TOOL_CALL", text="tool: taskflow"),
        SimpleNamespace(type="MessageType.MESSAGE", text="任务已提交"),
    ]
    for event in events:
        asyncio.run(
            ch.on_event_message_completed(
                SimpleNamespace(),
                "!room:hs.local",
                event,
                meta,
            ),
        )

    asyncio.run(ch._on_process_completed(SimpleNamespace(), "!room:hs.local", meta))

    assert main == [
        ("!room:hs.local", "first"),
        ("!room:hs.local", "任务已提交"),
    ]
    assert thread == [
        ("!room:hs.local", "准备提交任务", "$first"),
        ("!room:hs.local", "tool: taskflow", "$first"),
    ]


def test_tool_call_completed_message_routes_to_task_thread():
    ch = _make_channel()
    client = _FakeClient()
    ch._client = client
    ch._send_typing = _noop_typing
    ch._message_to_content_parts = lambda _event: [
        _FakeTextContent(matrix_channel.ContentType.TEXT, "tool: read_file")
    ]

    async def _send_content_parts(to_handle, parts, meta):
        await ch.send(to_handle, parts[0].text, meta)

    ch.send_content_parts = _send_content_parts
    event = SimpleNamespace(type="MessageType.MCP_TOOL_CALL")
    meta = {"thread_root_event_id": "$incoming-task"}

    asyncio.run(ch.send("!room:hs.local", "收到任务", meta))

    asyncio.run(
        ch.on_event_message_completed(
            SimpleNamespace(),
            "!room:hs.local",
            event,
            meta,
        ),
    )

    assert client.sent[0][2]["body"] == "收到任务"
    assert "m.relates_to" not in client.sent[0][2]
    assert client.sent[1][2]["body"] == "tool: read_file"
    assert client.sent[1][2]["m.relates_to"]["event_id"] == "$sent1"


def test_tool_output_completed_message_without_media_is_not_sent():
    ch = _make_channel()
    captured = []

    async def _capture_send_content_parts(to_handle, parts, meta):
        captured.append((to_handle, event, meta))

    ch.send_content_parts = _capture_send_content_parts
    ch._tool_output_media_parts = lambda _event: []
    event = SimpleNamespace(type="MessageType.MCP_TOOL_CALL_OUTPUT")

    asyncio.run(
        ch.on_event_message_completed(
            SimpleNamespace(),
            "!room:hs.local",
            event,
            {"thread_root_event_id": "$task-root"},
        ),
    )

    assert captured == []


def test_tool_output_completed_message_preserves_attachments():
    ch = _make_channel()
    captured = []
    file_part = SimpleNamespace(
        type=matrix_channel.ContentType.FILE,
        file_url="file:///tmp/result.tar.gz",
    )

    async def _capture_send_content_parts(to_handle, parts, meta):
        captured.append((to_handle, parts, meta))

    ch.send_content_parts = _capture_send_content_parts
    ch._tool_output_media_parts = lambda _event: [file_part]
    event = SimpleNamespace(type="MessageType.MCP_TOOL_CALL_OUTPUT")

    asyncio.run(
        ch.on_event_message_completed(
            SimpleNamespace(),
            "!room:hs.local",
            event,
            {"thread_root_event_id": "$task-root"},
        ),
    )

    assert captured == [
        (
            "!room:hs.local",
            [file_part],
            {"thread_root_event_id": "$task-root"},
        ),
    ]


def test_streaming_tool_content_is_consumed_without_sending():
    ch = _make_channel()
    client = _FakeClient()
    ch._client = client
    ch._send_typing = _noop_typing
    ch._filter_tool_messages = False

    async def _send_content_parts(to_handle, parts, meta):
        await ch.send(to_handle, parts[0].text, meta)

    ch.send_content_parts = _send_content_parts

    event = SimpleNamespace(
        type=matrix_channel.ContentType.DATA,
        status=SimpleNamespace(name="InProgress"),
        data={
            "name": "read_file",
            "output": [{"type": "text", "text": "opened spec.md"}],
        },
    )

    handled = asyncio.run(
        ch.on_event_content(
            SimpleNamespace(),
            "!room:hs.local",
            event,
            {"thread_root_event_id": "$task-root"},
        ),
    )

    assert handled is True
    assert client.sent == []
