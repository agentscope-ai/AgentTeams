from pathlib import Path
import asyncio
import json
import sys
from types import SimpleNamespace

from matrix_overlay_harness import OVERLAY, load_overlay_module


ROOT = Path(__file__).resolve().parents[1]


def test_matrix_overlay_is_installed_by_worker_image() -> None:
    dockerfile = (ROOT / "Dockerfile").read_text(encoding="utf-8")

    assert "qwenpaw/scripts/qwenpaw_site_packages_gate.py" in dockerfile
    assert "apply-matrix-overlay" in dockerfile
    assert "QWENPAW_PIP_SPEC=qwenpaw==1.1.11" in dockerfile or 'qwenpaw==1.1.11' in dockerfile


def test_matrix_from_config_streaming_enabled_defaults_true() -> None:
    module = load_overlay_module()
    channel = module.MatrixChannel.from_config(lambda *_args, **_kwargs: None, {})
    assert channel.streaming_enabled is True


def test_matrix_from_config_streaming_enabled_honors_override() -> None:
    module = load_overlay_module()
    channel = module.MatrixChannel.from_config(
        lambda *_args, **_kwargs: None,
        {"streaming_enabled": False},
    )
    assert channel.streaming_enabled is False


def test_matrix_mark_channel_ready_writes_marker(tmp_path, monkeypatch) -> None:
    module = load_overlay_module()
    marker = tmp_path / "matrix-ready.txt"
    monkeypatch.setenv("AGENTTEAMS_MATRIX_CHANNEL_READY_FILE", str(marker))
    channel = module.MatrixChannel.__new__(module.MatrixChannel)
    channel._mark_channel_ready()
    assert marker.read_text(encoding="utf-8") == "ready\n"


def test_matrix_text_content_applies_structured_mentions() -> None:
    module = load_overlay_module()
    channel = module.MatrixChannel.__new__(module.MatrixChannel)
    channel._user_id = "@bot:hs.local"
    channel._resolve_display_name = lambda user_id, _room_id: user_id

    content = channel._matrix_text_content(
        "!room:hs.local",
        "hello @alice:hs.local",
        {},
        "m.text",
    )

    assert content["format"] == "org.matrix.custom.html"
    assert "https://matrix.to/#/" in content["formatted_body"]
    assert content["m.mentions"] == {"user_ids": ["@alice:hs.local"]}


def test_matrix_text_content_honors_explicit_mention_user_ids() -> None:
    module = load_overlay_module()
    channel = module.MatrixChannel.__new__(module.MatrixChannel)
    channel._user_id = "@bot:hs.local"
    channel._resolve_display_name = lambda user_id, _room_id: user_id

    content = channel._matrix_text_content(
        "!room:hs.local",
        "task assigned",
        {"mention_user_ids": ["@worker:hs.local"]},
        "m.text",
    )

    assert content["m.mentions"] == {"user_ids": ["@worker:hs.local"]}


class _FakeClient:
    def __init__(self):
        self.rooms = {}
        self.sent = []

    async def room_send(self, room_id, message_type, content, **kwargs):
        self.sent.append((room_id, message_type, dict(content), kwargs))
        return SimpleNamespace(event_id=f"$sent{len(self.sent)}")


async def _noop_typing(_room_id, _typing):
    return None


async def _noop_prepare(_room_id):
    return None


def _make_thread_channel():
    module = load_overlay_module()
    channel = module.MatrixChannel.__new__(module.MatrixChannel)
    channel._client = _FakeClient()
    channel._user_id = "@bot:hs.local"
    channel._active_thread_roots = {}
    channel._send_typing = _noop_typing
    channel._prepare_room_send = _noop_prepare
    channel._message_to_content_parts = lambda event: [
        SimpleNamespace(type=module.ContentType.TEXT, text=event.text)
    ]

    async def _send_message_content(to_handle, event, meta):
        await channel.send(to_handle, event.text, meta)

    async def _send_content_parts(to_handle, parts, meta):
        for part in parts:
            await channel.send(to_handle, part.text, meta)

    channel.send_message_content = _send_message_content
    channel.send_content_parts = _send_content_parts
    return module, channel


def test_matrix_send_media_adds_attachment_relation(tmp_path) -> None:
    module, channel = _make_thread_channel()
    media_path = tmp_path / "report.md"
    media_path.write_text("report\n", encoding="utf-8")

    async def _upload_file(_file_ref):
        return "mxc://hs.local/report"

    async def _flush_pending_thread_parts(_room_id, _meta):
        return None

    channel._upload_file = _upload_file
    channel._flush_pending_thread_parts = _flush_pending_thread_parts

    meta = {"matrixAttachmentParentEventId": "$parent-text"}
    media_uri = asyncio.run(
        channel.send_media(
            "!room:hs.local",
            SimpleNamespace(type=module.ContentType.FILE, file_url=str(media_path)),
            meta,
        ),
    )

    assert media_uri == "mxc://hs.local/report"
    content = channel._client.sent[0][2]
    assert content["msgtype"] == "m.file"
    assert content["m.relates_to"] == {
        "rel_type": "com.agentteams.attachment",
        "event_id": "$parent-text",
    }
    assert module._MATRIX_OWN_THREAD_ROOT_KEY not in meta


def test_matrix_send_media_does_not_inherit_thread_relation(tmp_path) -> None:
    module, channel = _make_thread_channel()
    media_path = tmp_path / "report.md"
    media_path.write_text("report\n", encoding="utf-8")

    async def _upload_file(_file_ref):
        return "mxc://hs.local/report"

    channel._upload_file = _upload_file

    media_uri = asyncio.run(
        channel.send_media(
            "!room:hs.local",
            SimpleNamespace(type=module.ContentType.FILE, file_url=str(media_path)),
            {module._MATRIX_THREAD_META_KEY: "$thread-root"},
        ),
    )

    assert media_uri == "mxc://hs.local/report"
    content = channel._client.sent[0][2]
    assert content["msgtype"] == "m.file"
    assert "m.relates_to" not in content


def test_matrix_thread_root_writes_attachment_context(tmp_path, monkeypatch) -> None:
    _module, channel = _make_thread_channel()
    context_file = tmp_path / "matrix-context.json"
    monkeypatch.setenv("TEAMHARNESS_MATRIX_CONTEXT_FILE", str(context_file))
    meta = {}

    asyncio.run(channel._ensure_thread_root("!room:hs.local", meta))

    data = json.loads(context_file.read_text(encoding="utf-8"))
    record = data["rooms"]["!room:hs.local"]
    assert record["attachmentParentEventId"] == "$sent1"
    assert record["eventId"] == "$sent1"


def test_matrix_thread_message_only_response_edits_processing_root() -> None:
    _module, channel = _make_thread_channel()
    meta = {"thread_root_event_id": "$incoming"}

    asyncio.run(
        channel.on_event_message_completed(
            SimpleNamespace(),
            "!room:hs.local",
            SimpleNamespace(type="MessageType.MESSAGE", text="final answer"),
            meta,
        ),
    )
    asyncio.run(
        channel._on_process_completed(
            SimpleNamespace(),
            "!room:hs.local",
            meta,
        ),
    )

    assert channel._client.sent[0][2]["body"] == "处理中..."
    assert channel._client.sent[1][2]["m.relates_to"] == {
        "rel_type": "m.replace",
        "event_id": "$sent1",
    }
    assert channel._client.sent[1][2]["format"] == "org.matrix.custom.html"
    assert channel._client.sent[1][2]["formatted_body"].startswith("<p>* ")
    assert channel._client.sent[1][2]["m.new_content"]["body"] == "final answer"
    assert (
        channel._client.sent[1][2]["m.new_content"]["format"]
        == "org.matrix.custom.html"
    )
    assert "final answer" in channel._client.sent[1][2]["m.new_content"]["formatted_body"]


def test_matrix_edit_fallback_html_does_not_embed_markdown_block_html() -> None:
    _module, channel = _make_thread_channel()
    meta = {"thread_root_event_id": "$incoming"}
    text = 'Result:\n\n```json\n{"ok": true}\n```'

    asyncio.run(
        channel.on_event_message_completed(
            SimpleNamespace(),
            "!room:hs.local",
            SimpleNamespace(type="MessageType.MESSAGE", text=text),
            meta,
        ),
    )
    asyncio.run(
        channel._on_process_completed(
            SimpleNamespace(),
            "!room:hs.local",
            meta,
        ),
    )

    edit = channel._client.sent[1][2]
    assert edit["m.relates_to"] == {
        "rel_type": "m.replace",
        "event_id": "$sent1",
    }
    assert edit["formatted_body"].startswith("<p>* ")
    assert "* <pre" not in edit["formatted_body"]
    assert "<pre><code" in edit["m.new_content"]["formatted_body"]


def test_matrix_thread_parts_prefer_own_processing_root_over_incoming_root() -> None:
    _module, channel = _make_thread_channel()
    meta = {"thread_root_event_id": "$incoming"}

    asyncio.run(
        channel.on_event_message_completed(
            SimpleNamespace(),
            "!room:hs.local",
            SimpleNamespace(type="MessageType.MESSAGE", text="first note"),
            meta,
        ),
    )
    asyncio.run(
        channel.on_event_message_completed(
            SimpleNamespace(),
            "!room:hs.local",
            SimpleNamespace(type="MessageType.MCP_TOOL_CALL", text="tool call"),
            meta,
        ),
    )

    assert channel._client.sent[0][2]["body"] == "处理中..."
    thread_events = [
        content
        for _room_id, _message_type, content, _kwargs in channel._client.sent[1:]
        if content.get("m.relates_to", {}).get("rel_type") == "m.thread"
    ]
    assert [event["body"] for event in thread_events] == [
        "first note",
        "tool call",
    ]
    assert {
        event["m.relates_to"]["event_id"] for event in thread_events
    } == {"$sent1"}


def test_matrix_streaming_reasoning_waits_for_end_before_thread_item() -> None:
    module, channel = _make_thread_channel()
    meta = {"thread_root_event_id": "$incoming"}

    asyncio.run(
        channel.on_streaming_start(
            SimpleNamespace(),
            "!room:hs.local",
            SimpleNamespace(type="reasoning", id="reasoning-1"),
            meta,
            "reasoning",
        ),
    )
    asyncio.run(
        channel.on_streaming_delta(
            SimpleNamespace(),
            "!room:hs.local",
            SimpleNamespace(text="ignored"),
            meta,
            "reasoning",
            accumulated_text="partial thinking",
        ),
    )
    assert len(channel._client.sent) == 1

    asyncio.run(
        channel.on_streaming_delta(
            SimpleNamespace(),
            "!room:hs.local",
            SimpleNamespace(text="ignored"),
            meta,
            "reasoning",
            accumulated_text="partial thinking hidden by throttle",
        ),
    )
    assert len(channel._client.sent) == 1

    meta[module._MATRIX_STREAMING_REASONING_LAST_EDIT_KEY] = -10
    asyncio.run(
        channel.on_streaming_delta(
            SimpleNamespace(),
            "!room:hs.local",
            SimpleNamespace(text="ignored"),
            meta,
            "reasoning",
            accumulated_text="partial thinking and updated thinking",
        ),
    )
    assert len(channel._client.sent) == 1

    asyncio.run(
        channel.on_streaming_end(
            SimpleNamespace(),
            "!room:hs.local",
            SimpleNamespace(type="reasoning", text="fallback"),
            meta,
            "reasoning",
            accumulated_text="partial thinking and updated thinking final tail",
        ),
    )
    assert meta.get(module._MATRIX_STREAMING_REASONING_EVENT_ID_KEY) is None
    assert meta.get(module._MATRIX_STREAMING_REASONING_STREAM_ID_KEY) is None
    thinking = channel._client.sent[1][2]
    assert thinking["msgtype"] == "m.notice"
    assert thinking["body"] == (
        "Thinking:\n\npartial thinking and updated thinking final tail"
    )
    assert thinking["m.relates_to"] == {
        "rel_type": "m.thread",
        "event_id": "$sent1",
        "is_falling_back": False,
    }
    asyncio.run(
        channel.on_event_message_completed(
            SimpleNamespace(),
            "!room:hs.local",
            SimpleNamespace(type="MessageType.MCP_TOOL_CALL", text="tool call"),
            meta,
        ),
    )
    asyncio.run(
        channel.on_streaming_start(
            SimpleNamespace(),
            "!room:hs.local",
            SimpleNamespace(type="reasoning", id="reasoning-2"),
            meta,
            "reasoning",
        ),
    )
    asyncio.run(
        channel.on_streaming_delta(
            SimpleNamespace(),
            "!room:hs.local",
            SimpleNamespace(text="ignored"),
            meta,
            "reasoning",
            accumulated_text="second step thinking",
        ),
    )
    asyncio.run(
        channel.on_streaming_end(
            SimpleNamespace(),
            "!room:hs.local",
            SimpleNamespace(type="reasoning", text="fallback"),
            meta,
            "reasoning",
            accumulated_text="second step thinking",
        ),
    )
    asyncio.run(
        channel.on_streaming_end(
            SimpleNamespace(),
            "!room:hs.local",
            SimpleNamespace(type="message", text="fallback final"),
            meta,
            "message",
            accumulated_text="final answer",
        ),
    )
    asyncio.run(
        channel._on_process_completed(
            SimpleNamespace(),
            "!room:hs.local",
            meta,
        ),
    )

    assert channel._client.sent[0][2]["body"] == "处理中..."
    tool_call = channel._client.sent[2][2]
    assert tool_call["body"] == "tool call"
    assert tool_call["m.relates_to"] == {
        "rel_type": "m.thread",
        "event_id": "$sent1",
        "is_falling_back": False,
    }
    second_thinking = channel._client.sent[3][2]
    assert second_thinking["body"] == "Thinking:\n\nsecond step thinking"
    assert second_thinking["m.relates_to"] == {
        "rel_type": "m.thread",
        "event_id": "$sent1",
        "is_falling_back": False,
    }
    final = channel._client.sent[4][2]
    assert final["m.relates_to"] == {
        "rel_type": "m.replace",
        "event_id": "$sent1",
    }
    assert final["m.new_content"]["body"] == "final answer"


def test_matrix_streaming_message_only_edits_processing_root_on_completion() -> None:
    _module, channel = _make_thread_channel()
    meta = {"thread_root_event_id": "$incoming"}

    asyncio.run(
        channel.on_streaming_start(
            SimpleNamespace(),
            "!room:hs.local",
            SimpleNamespace(type="message"),
            meta,
            "message",
        ),
    )
    asyncio.run(
        channel.on_streaming_delta(
            SimpleNamespace(),
            "!room:hs.local",
            SimpleNamespace(text="ignored"),
            meta,
            "message",
            accumulated_text="partial",
        ),
    )
    assert channel._client.sent[0][2]["body"] == "处理中..."
    assert len(channel._client.sent) == 1

    asyncio.run(
        channel.on_streaming_delta(
            SimpleNamespace(),
            "!room:hs.local",
            SimpleNamespace(text="ignored"),
            meta,
            "message",
            accumulated_text="partial hidden by throttle",
        ),
    )
    assert len(channel._client.sent) == 1

    asyncio.run(
        channel.on_streaming_delta(
            SimpleNamespace(),
            "!room:hs.local",
            SimpleNamespace(text="ignored"),
            meta,
            "message",
            accumulated_text="partial updated",
        ),
    )
    assert len(channel._client.sent) == 1

    asyncio.run(
        channel.on_streaming_end(
            SimpleNamespace(),
            "!room:hs.local",
            SimpleNamespace(type="message", text="fallback final"),
            meta,
            "message",
            accumulated_text="final answer",
        ),
    )
    assert len(channel._client.sent) == 1

    asyncio.run(
        channel._on_process_completed(
            SimpleNamespace(),
            "!room:hs.local",
            meta,
        ),
    )
    assert len(channel._client.sent) == 2
    final = channel._client.sent[1][2]
    assert final["m.relates_to"] == {
        "rel_type": "m.replace",
        "event_id": "$sent1",
    }
    assert final["m.new_content"]["body"] == "final answer"


def test_matrix_no_reply_final_closes_processing_root_as_processed() -> None:
    _module, channel = _make_thread_channel()
    meta = {"thread_root_event_id": "$incoming"}

    asyncio.run(
        channel.on_streaming_start(
            SimpleNamespace(),
            "!room:hs.local",
            SimpleNamespace(type="message"),
            meta,
            "message",
        ),
    )
    asyncio.run(
        channel.on_streaming_end(
            SimpleNamespace(),
            "!room:hs.local",
            SimpleNamespace(type="message", text="NO_REPLY"),
            meta,
            "message",
            accumulated_text="NO_REPLY",
        ),
    )
    asyncio.run(
        channel._on_process_completed(
            SimpleNamespace(),
            "!room:hs.local",
            meta,
        ),
    )

    assert channel._client.sent[0][2]["body"] == "处理中..."
    final = channel._client.sent[1][2]
    assert final["m.relates_to"] == {
        "rel_type": "m.replace",
        "event_id": "$sent1",
    }
    assert final["m.new_content"]["body"] == "已处理"
    assert "!room:hs.local" not in channel._active_thread_roots


def test_matrix_short_text_sends_direct_message() -> None:
    _module, channel = _make_thread_channel()

    asyncio.run(channel.send("!room:hs.local", "short answer", {}))

    assert len(channel._client.sent) == 1
    content = channel._client.sent[0][2]
    assert content["msgtype"] == "m.text"
    assert content["body"] == "short answer"
    assert content["format"] == "org.matrix.custom.html"


def test_matrix_long_text_uses_media_fallback(tmp_path: Path) -> None:
    module, channel = _make_thread_channel()
    channel._workspace_dir = tmp_path
    uploaded = []

    async def _fake_upload(file_ref):
        uploaded.append(Path(file_ref))
        return "mxc://hs.local/full-content"

    channel._upload_file = _fake_upload
    long_text = "long-content\n" + ("x" * (module.MATRIX_TEXT_EVENT_SAFE_BYTES + 1000))

    asyncio.run(channel.send("!room:hs.local", long_text, {}))

    assert len(channel._client.sent) == 2
    assert uploaded and uploaded[0].read_text(encoding="utf-8") == long_text
    summary = channel._client.sent[0][2]
    media = channel._client.sent[1][2]
    assert media["msgtype"] == "m.file"
    assert media["url"] == "mxc://hs.local/full-content"
    assert media["m.relates_to"] == {
        "rel_type": "com.agentteams.attachment",
        "event_id": "$sent1",
    }
    assert summary["msgtype"] == "m.text"
    assert summary["body"].startswith("long-content\n")
    assert "单条 Matrix 消息已按安全长度截断" in summary["body"]
    assert "完整内容已缓存为 Matrix 附件" in summary["body"]
    assert "附件地址：mxc://hs.local/full-content" in summary["body"]
    assert long_text not in summary["body"]
    assert summary[module.MATRIX_LONG_MESSAGE_METADATA_KEY] == {
        "version": 1,
        "url": "mxc://hs.local/full-content",
        "filename": uploaded[0].name,
        "mimetype": "text/markdown; charset=utf-8",
    }
    assert (
        module._matrix_event_payload_size(summary)
        <= module.MATRIX_TEXT_EVENT_FALLBACK_BUDGET_BYTES
    )


def test_matrix_long_text_fallback_summary_stays_under_threshold(tmp_path: Path) -> None:
    module, channel = _make_thread_channel()
    channel._workspace_dir = tmp_path

    async def _fake_upload(_file_ref):
        return None

    channel._upload_file = _fake_upload
    long_text = "z" * (module.MATRIX_TEXT_EVENT_SAFE_BYTES * 4)

    asyncio.run(channel.send("!room:hs.local", long_text, {}))

    assert len(channel._client.sent) == 1
    summary = channel._client.sent[0][2]
    assert summary["msgtype"] == "m.text"
    assert summary["body"].startswith("z" * 100)
    assert "单条 Matrix 消息已按安全长度截断" in summary["body"]
    assert "完整内容已缓存为本地文件" in summary["body"]
    assert "Matrix 附件上传失败" in summary["body"]
    assert long_text not in summary["body"]
    assert module.MATRIX_LONG_MESSAGE_METADATA_KEY not in summary
    assert (
        module._matrix_event_payload_size(summary)
        <= module.MATRIX_TEXT_EVENT_FALLBACK_BUDGET_BYTES
    )


def test_matrix_long_edit_uses_media_fallback(tmp_path: Path) -> None:
    module, channel = _make_thread_channel()
    channel._workspace_dir = tmp_path
    uploaded = []

    async def _fake_upload(file_ref):
        uploaded.append(Path(file_ref))
        return "mxc://hs.local/edited-full-content"

    channel._upload_file = _fake_upload
    long_text = "final\n" + ("y" * (module.MATRIX_TEXT_EVENT_SAFE_BYTES + 1000))

    asyncio.run(
        channel._edit_matrix_event(
            "!room:hs.local",
            "$root",
            long_text,
            msgtype="m.text",
            html=module._md_to_html(long_text),
        ),
    )

    assert len(channel._client.sent) == 2
    edit = channel._client.sent[0][2]
    media = channel._client.sent[1][2]
    assert media["msgtype"] == "m.file"
    assert media["m.relates_to"] == {
        "rel_type": "com.agentteams.attachment",
        "event_id": "$root",
    }
    assert edit["m.relates_to"] == {"rel_type": "m.replace", "event_id": "$root"}
    assert edit["m.new_content"]["body"].startswith("final\n")
    assert "单条 Matrix 消息已按安全长度截断" in edit["m.new_content"]["body"]
    assert "完整内容已缓存为 Matrix 附件" in edit["m.new_content"]["body"]
    assert "附件地址：mxc://hs.local/edited-full-content" in edit["m.new_content"]["body"]
    assert long_text not in edit["m.new_content"]["body"]
    expected_metadata = {
        "version": 1,
        "url": "mxc://hs.local/edited-full-content",
        "filename": uploaded[0].name,
        "mimetype": "text/markdown; charset=utf-8",
    }
    assert edit[module.MATRIX_LONG_MESSAGE_METADATA_KEY] == expected_metadata
    assert (
        edit["m.new_content"][module.MATRIX_LONG_MESSAGE_METADATA_KEY]
        == expected_metadata
    )
    assert (
        module._matrix_event_payload_size(edit)
        <= module.MATRIX_TEXT_EVENT_FALLBACK_BUDGET_BYTES
    )


def test_matrix_long_edit_fallback_failure_does_not_block_completion_cleanup(
    tmp_path: Path,
) -> None:
    module, channel = _make_thread_channel()
    channel._workspace_dir = tmp_path
    meta = {"thread_root_event_id": "$incoming"}
    typing_events = []

    async def _capture_typing(room_id, typing):
        typing_events.append((room_id, typing))

    async def _fake_upload(_file_ref):
        return "mxc://hs.local/edited-full-content"

    async def _failing_prepare(room_id):
        if len(channel._client.sent) >= 2:
            raise RuntimeError("summary send failed")

    channel._send_typing = _capture_typing
    channel._prepare_room_send = _failing_prepare
    channel._upload_file = _fake_upload
    long_text = "final\n" + ("y" * (module.MATRIX_TEXT_EVENT_SAFE_BYTES + 1000))

    asyncio.run(
        channel.on_streaming_start(
            SimpleNamespace(),
            "!room:hs.local",
            SimpleNamespace(type="message"),
            meta,
            "message",
        ),
    )
    asyncio.run(
        channel.on_streaming_end(
            SimpleNamespace(),
            "!room:hs.local",
            SimpleNamespace(type="message", text="fallback final"),
            meta,
            "message",
            accumulated_text=long_text,
        ),
    )
    asyncio.run(
        channel._on_process_completed(
            SimpleNamespace(),
            "!room:hs.local",
            meta,
        ),
    )

    assert "!room:hs.local" not in channel._active_thread_roots
    assert typing_events[-1] == ("!room:hs.local", False)


def test_matrix_consume_error_closes_processing_root() -> None:
    _module, channel = _make_thread_channel()
    meta = {"thread_root_event_id": "$incoming"}

    asyncio.run(channel._ensure_thread_root("!room:hs.local", meta))
    asyncio.run(
        channel._on_consume_error(
            SimpleNamespace(),
            "!room:hs.local",
            "runtime loop failed",
        ),
    )

    assert channel._client.sent[0][2]["body"] == "处理中..."
    error = channel._client.sent[1][2]
    assert error["m.relates_to"] == {
        "rel_type": "m.replace",
        "event_id": "$sent1",
    }
    assert error["m.new_content"]["body"] == "处理异常"
    assert "!room:hs.local" not in channel._active_thread_roots


class _FakeCommandRegistry:
    def is_control_command(self, text):
        return text.strip().lower().split(None, 1)[0] in {
            "/stop",
            "/clear",
            "/approve",
        }


class _FakeInboundRoom:
    room_id = "!room:hs.local"
    users = {}
    topic = ""

    def user_name(self, user_id):
        if user_id == "@copywriting-assistant:hs.local":
            return "copywriting-assistant"
        return user_id


async def _false_dm(*_args):
    return False


async def _noop_read_receipt(_room_id, _event_id):
    return None


def _make_inbound_channel(command_registry=True):
    module = load_overlay_module()
    channel = module.MatrixChannel.__new__(module.MatrixChannel)
    channel._user_id = "@copywriting-assistant:hs.local"
    channel._client = _FakeClient()
    channel.dm_disabled = False
    channel.group_disabled = False
    channel.groups = {}
    channel.history_limit = 50
    channel._room_histories = {}
    channel._teamharness_task_room_cache = {}
    channel._dm_room_cache = {}
    if command_registry:
        channel._command_registry = _FakeCommandRegistry()
    channel._is_dm_room = _false_dm
    channel._check_allowed = lambda *_args: True
    channel._require_mention = lambda _room_id: True
    channel._send_read_receipt = _noop_read_receipt
    channel._send_typing = _noop_typing
    channel.enqueued = []
    channel._enqueue = channel.enqueued.append
    return channel


def _matrix_event(body: str, mentioned: bool = False):
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


class _FakeTaskRoom(_FakeInboundRoom):
    room_id = "!task:hs.local"
    topic = "Task room for demo-001 [source: dingtalk]"
    users = {
        "@copywriting-assistant:hs.local": object(),
        "@worker:hs.local": object(),
    }

    def user_name(self, user_id):
        if user_id == "@copywriting-assistant:hs.local":
            return "copywriting-assistant"
        if user_id == "@worker:hs.local":
            return "worker"
        return user_id


def _matrix_event_from(
    sender: str,
    body: str,
    *,
    event_id: str = "$event",
    content_extra=None,
):
    content = {"m.mentions": {}}
    if content_extra:
        content.update(content_extra)
    return SimpleNamespace(
        sender=sender,
        body=body,
        event_id=event_id,
        server_timestamp=0,
        source={"content": content},
    )


def _use_real_dm_detector(channel):
    module = sys.modules[channel.__class__.__module__]
    delattr(channel, "_is_dm_room")
    channel._dm_room_cache = {}

    async def _joined_members(_room_id):
        response = module.JoinedMembersResponse()
        response.members = [
            SimpleNamespace(user_id="@copywriting-assistant:hs.local"),
            SimpleNamespace(user_id="@worker:hs.local"),
        ]
        return response

    channel._client.joined_members = _joined_members
    return module


def _first_text(payload):
    return payload["content_parts"][0].text


def test_matrix_teamharness_task_room_is_not_dm_even_with_two_members() -> None:
    channel = _make_inbound_channel()
    _use_real_dm_detector(channel)
    room = _FakeTaskRoom()

    is_dm = asyncio.run(
        channel._is_dm_room(
            room.room_id,
            "@worker:hs.local",
            room,
        ),
    )

    assert is_dm is False


def test_matrix_teamharness_task_room_detected_from_task_meta(tmp_path) -> None:
    channel = _make_inbound_channel()
    _use_real_dm_detector(channel)
    channel._workspace_dir = tmp_path
    task_dir = tmp_path / "shared" / "tasks" / "demo-001"
    task_dir.mkdir(parents=True)
    (task_dir / "meta.json").write_text(
        '{"task_id":"demo-001","room_id":"!task:hs.local"}',
        encoding="utf-8",
    )
    room = SimpleNamespace(room_id="!task:hs.local", users=_FakeTaskRoom.users)

    is_dm = asyncio.run(
        channel._is_dm_room(
            room.room_id,
            "@worker:hs.local",
            room,
        ),
    )

    assert is_dm is False


def test_matrix_task_room_filters_tool_echo_but_accepts_completion() -> None:
    channel = _make_inbound_channel()
    _use_real_dm_detector(channel)
    room = _FakeTaskRoom()

    asyncio.run(
        channel._on_room_event(
            room,
            _matrix_event_from(
                "@worker:hs.local",
                '🔧 **read_file**\n```\n{"file_path":"shared/tasks/demo-001/spec.md"}\n```',
                event_id="$tool",
                content_extra={
                    "m.relates_to": {
                        "rel_type": "m.thread",
                        "event_id": "$root",
                    },
                },
            ),
        ),
    )

    assert channel.enqueued == []

    asyncio.run(
        channel._on_room_event(
            room,
            _matrix_event_from(
                "@worker:hs.local",
                "@copywriting-assistant:hs.local TASK_COMPLETED: demo-001 - "
                "Result: shared/tasks/demo-001/result.md",
                event_id="$done",
            ),
        ),
    )

    assert len(channel.enqueued) == 1
    assert "TASK_COMPLETED: demo-001" in _first_text(channel.enqueued[0])


def test_matrix_task_room_filters_plain_tool_display_before_context() -> None:
    channel = _make_inbound_channel()
    _use_real_dm_detector(channel)
    room = _FakeTaskRoom()

    asyncio.run(
        channel._on_room_event(
            room,
            _matrix_event_from(
                "@teamleader:hs.local",
                "teamleader: 🔧 **projectflow**\n"
                "```\n"
                '{"action":"plan_dag","payload":{"tasks":[{"assignedTo":'
                '"@copywriting-assistant:hs.local"}]}}\n'
                "```",
                event_id="$tool-display",
                content_extra={
                    "m.mentions": {
                        "user_ids": ["@copywriting-assistant:hs.local"],
                    },
                },
            ),
        ),
    )

    assert channel.enqueued == []
    assert channel._room_histories == {}

    asyncio.run(
        channel._on_room_event(
            room,
            _matrix_event_from(
                "@teamleader:hs.local",
                "@copywriting-assistant:hs.local TASK_ASSIGNED: demo-001",
                event_id="$assigned",
            ),
        ),
    )

    assert len(channel.enqueued) == 1
    assert "TASK_ASSIGNED: demo-001" in _first_text(channel.enqueued[0])


def test_matrix_ordinary_own_message_is_skipped() -> None:
    channel = _make_inbound_channel()

    asyncio.run(
        channel._on_room_event(
            _FakeInboundRoom(),
            _matrix_event_from(
                "@copywriting-assistant:hs.local",
                "ordinary self echo",
            ),
        ),
    )

    assert channel.enqueued == []


def test_matrix_teamharness_self_trigger_bypasses_own_skip_and_mention_gate() -> None:
    channel = _make_inbound_channel()

    asyncio.run(
        channel._on_room_event(
            _FakeInboundRoom(),
            _matrix_event_from(
                "@copywriting-assistant:hs.local",
                "PROJECT_REQUESTED: req-123",
                content_extra={
                    "m.teamharness.trigger": {
                        "kind": "self_cross_session",
                        "type": "PROJECT_REQUESTED",
                        "targetRoomId": "!room:hs.local",
                        "targetSession": "matrix:!room:hs.local",
                    },
                },
            ),
        ),
    )

    assert len(channel.enqueued) == 1
    payload = channel.enqueued[0]
    assert "PROJECT_REQUESTED: req-123" in _first_text(payload)
    assert payload["meta"]["teamharness_trigger"] is True
    assert payload["meta"]["teamharness_trigger_type"] == "PROJECT_REQUESTED"


def test_matrix_teamharness_self_trigger_requires_allowlisted_type() -> None:
    channel = _make_inbound_channel()

    asyncio.run(
        channel._on_room_event(
            _FakeInboundRoom(),
            _matrix_event_from(
                "@copywriting-assistant:hs.local",
                "TASK_COMPLETED: not a project request",
                content_extra={
                    "m.teamharness.trigger": {
                        "kind": "self_cross_session",
                        "type": "TASK_COMPLETED",
                        "targetRoomId": "!room:hs.local",
                    },
                },
            ),
        ),
    )

    assert channel.enqueued == []


def test_matrix_control_command_strips_mention_before_enqueue() -> None:
    channel = _make_inbound_channel()

    asyncio.run(
        channel._on_room_event(
            _FakeInboundRoom(),
            _matrix_event("copywriting-assistant: /stop", mentioned=True),
        ),
    )

    assert len(channel.enqueued) == 1
    assert _first_text(channel.enqueued[0]) == "/stop"


def test_matrix_control_command_strips_at_localpart_mention_before_enqueue() -> None:
    channel = _make_inbound_channel()
    channel._user_id = "@worker:at-cn-ojs4upkyq01"

    asyncio.run(
        channel._on_room_event(
            _FakeInboundRoom(),
            _matrix_event_from(
                "@24774e777ea409f1bc37ea990615d3a5:at-cn-ojs4upkyq01",
                "@worker /stop",
                event_id="~!MnRNwPjGJF2BtzpvWe:at-cn-ojs4upkyq01:m1782979172665.54",
                content_extra={
                    "m.mentions": {
                        "user_ids": ["@worker:at-cn-ojs4upkyq01"],
                    },
                },
            ),
        ),
    )

    assert len(channel.enqueued) == 1
    assert _first_text(channel.enqueued[0]) == "/stop"


def test_matrix_double_slash_control_command_normalizes_with_mention() -> None:
    channel = _make_inbound_channel()

    asyncio.run(
        channel._on_room_event(
            _FakeInboundRoom(),
            _matrix_event("copywriting-assistant: //stop", mentioned=True),
        ),
    )

    assert len(channel.enqueued) == 1
    assert _first_text(channel.enqueued[0]) == "/stop"


def test_matrix_known_slash_command_normalizes_without_registry() -> None:
    channel = _make_inbound_channel(command_registry=False)

    asyncio.run(
        channel._on_room_event(
            _FakeInboundRoom(),
            _matrix_event("copywriting-assistant: //clear", mentioned=True),
        ),
    )

    assert len(channel.enqueued) == 1
    assert _first_text(channel.enqueued[0]) == "/clear"


def test_matrix_stop_command_passes_through_without_registry() -> None:
    channel = _make_inbound_channel(command_registry=False)

    asyncio.run(
        channel._on_room_event(
            _FakeInboundRoom(),
            _matrix_event("copywriting-assistant: /stop", mentioned=True),
        ),
    )

    assert len(channel.enqueued) == 1
    assert _first_text(channel.enqueued[0]) == "/stop"


def test_matrix_bare_stop_not_recognized_without_slash() -> None:
    channel = _make_inbound_channel()

    asyncio.run(
        channel._on_room_event(
            _FakeInboundRoom(),
            _matrix_event("copywriting-assistant: stop", mentioned=True),
        ),
    )

    assert len(channel.enqueued) == 1
    assert "/stop" not in _first_text(channel.enqueued[0])


def test_matrix_stop_command_requires_mention_in_group() -> None:
    channel = _make_inbound_channel()

    asyncio.run(channel._on_room_event(_FakeInboundRoom(), _matrix_event("/stop")))

    assert len(channel.enqueued) == 0


def test_matrix_control_command_requires_mention_in_group() -> None:
    channel = _make_inbound_channel()

    asyncio.run(channel._on_room_event(_FakeInboundRoom(), _matrix_event("/approve")))

    assert len(channel.enqueued) == 0
