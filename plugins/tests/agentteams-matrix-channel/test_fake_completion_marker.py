#!/usr/bin/env python3
"""Fake-completion discrimination for the AgentTeams Matrix channel (#1320).

A silent turn — no tool calls and no visible final text — must not be
rendered as「已完成」("Done").  The channel records per-turn tool activity
and consults it when finalizing the placeholder:

    tool activity seen            -> 「已完成」 (quiet completion, unchanged)
    no tool activity, no text     -> 「本回合无产出」 (#1320 fix)
    NO_REPLY control line         -> 「已处理」 (unchanged)
    visible final text            -> the text itself (unchanged)

NOTE: channel.py imports the qwenpaw runtime (schemas / BaseChannel). This
module runs for real in the dedicated `matrix-channel-completion-guard` CI job
(which installs matrix-nio + qwenpaw) and in the QwenPaw dev container; in
environments without those runtime deps it skips (importorskip) instead of
erroring.
"""

from __future__ import annotations

import asyncio
import importlib.util
import sys
from pathlib import Path

import pytest

pytest.importorskip("nio")
pytest.importorskip("qwenpaw")

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
sys.modules["agentteams_matrix_channel"] = channel_module
_spec.loader.exec_module(channel_module)

AgentTeamsMatrixChannel = channel_module.AgentTeamsMatrixChannel

PLACEHOLDER_KEY = channel_module._MATRIX_PLACEHOLDER_THREAD_ROOT_KEY
ACTIVITY_KEY = channel_module._MATRIX_TURN_TOOL_ACTIVITY_KEY
STREAMING_KEY = channel_module._MATRIX_STREAMING_FINAL_TEXT_KEY
PENDING_KEY = channel_module._MATRIX_PENDING_FINAL_MESSAGE_KEY

ROOM = "!room:test"


def _make_channel():
    captured = []

    async def fake_edit(to_handle, send_meta, text, **kwargs):
        captured.append(text)

    async def noop(*args, **kwargs):
        return None

    channel = AgentTeamsMatrixChannel(
        process=lambda *args, **kwargs: None,
        share_session_in_group=True,
    )
    channel._edit_thread_root = fake_edit
    channel._send_typing = noop
    channel._send_plain_text = noop
    channel._evaluate_send_gate = lambda *args, **kwargs: ("pass", 0)
    return channel, captured


def _silence_base_hooks(monkeypatch):
    for cls in AgentTeamsMatrixChannel.__mro__[1:]:
        if "_on_process_completed" in cls.__dict__:
            monkeypatch.setattr(cls, "_on_process_completed", None, raising=False)


def _complete(channel, send_meta):
    asyncio.run(channel._on_process_completed(None, ROOM, send_meta))


def test_silent_turn_without_tools_is_not_marked_done(monkeypatch):
    _silence_base_hooks(monkeypatch)
    channel, captured = _make_channel()
    _complete(channel, {PLACEHOLDER_KEY: True})
    assert captured == ["本回合无产出"]


def test_tool_activity_keeps_the_done_marker(monkeypatch):
    _silence_base_hooks(monkeypatch)
    channel, captured = _make_channel()
    _complete(channel, {PLACEHOLDER_KEY: True, ACTIVITY_KEY: True})
    assert captured == ["已完成"]


def test_no_reply_stays_handled_with_or_without_tools(monkeypatch):
    _silence_base_hooks(monkeypatch)
    channel, captured = _make_channel()
    _complete(channel, {PLACEHOLDER_KEY: True, STREAMING_KEY: "NO_REPLY"})
    assert captured == ["已处理"]
    channel2, captured2 = _make_channel()
    _complete(
        channel2,
        {PLACEHOLDER_KEY: True, STREAMING_KEY: "NO_REPLY", ACTIVITY_KEY: True},
    )
    assert captured2 == ["已处理"]


def test_visible_text_is_posted_unchanged(monkeypatch):
    _silence_base_hooks(monkeypatch)
    channel, captured = _make_channel()
    _complete(channel, {PLACEHOLDER_KEY: True, STREAMING_KEY: "hello world"})
    assert captured == ["hello world"]


def test_tool_call_message_sets_activity_flag_and_reasoning_does_not(monkeypatch):
    _silence_base_hooks(monkeypatch)

    async def noop(*args, **kwargs):
        return None

    channel, _ = _make_channel()
    channel._ensure_thread_root = noop
    channel._flush_pending_final_message_to_thread = noop
    channel._message_to_content_parts = lambda event: []

    class _Event:
        def __init__(self, type_):
            self.type = type_

    tool_meta = {}
    asyncio.run(
        channel.on_event_message_completed(
            None, ROOM, _Event(channel_module.MessageType.FUNCTION_CALL), tool_meta
        )
    )
    assert tool_meta.get(ACTIVITY_KEY) is True

    reasoning_meta = {}
    asyncio.run(
        channel.on_event_message_completed(
            None, ROOM, _Event(channel_module.MessageType.REASONING), reasoning_meta
        )
    )
    assert ACTIVITY_KEY not in reasoning_meta


def test_whitespace_streaming_is_not_marked_done(monkeypatch):
    """Streaming text that is empty after normalization is still "no output"."""
    _silence_base_hooks(monkeypatch)
    channel, captured = _make_channel()
    _complete(channel, {PLACEHOLDER_KEY: True, STREAMING_KEY: "  \n\t "})
    assert captured == ["本回合无产出"]


def test_empty_pending_final_message_is_not_marked_done(monkeypatch):
    """An empty pending final message must not flip the marker."""
    _silence_base_hooks(monkeypatch)
    channel, captured = _make_channel()
    monkeypatch.setattr(channel, "_text_from_message_event", lambda event: "")
    _complete(channel, {PLACEHOLDER_KEY: True, PENDING_KEY: object()})
    assert captured == ["本回合无产出"]


def test_tool_output_message_sets_activity_flag(monkeypatch):
    """FUNCTION_CALL_OUTPUT messages set the turn tool-activity flag."""
    _silence_base_hooks(monkeypatch)

    async def noop(*args, **kwargs):
        return None

    channel, _ = _make_channel()
    channel._flush_pending_final_message_to_thread = noop
    channel._tool_output_media_parts = lambda event: []

    class _Event:
        def __init__(self, type_):
            self.type = type_

    tool_output_meta = {}
    asyncio.run(
        channel.on_event_message_completed(
            None,
            ROOM,
            _Event(channel_module.MessageType.FUNCTION_CALL_OUTPUT),
            tool_output_meta,
        )
    )
    assert tool_output_meta.get(ACTIVITY_KEY) is True


def test_quiet_tool_output_turn_keeps_the_done_marker(monkeypatch):
    """Tool output alone (no visible text) finalizes as 已完成 — with the flag
    set by the real event path rather than a manually seeded meta."""
    _silence_base_hooks(monkeypatch)

    async def noop(*args, **kwargs):
        return None

    channel, captured = _make_channel()
    channel._flush_pending_final_message_to_thread = noop
    channel._tool_output_media_parts = lambda event: []

    class _Event:
        def __init__(self, type_):
            self.type = type_

    send_meta = {}
    asyncio.run(
        channel.on_event_message_completed(
            None,
            ROOM,
            _Event(channel_module.MessageType.FUNCTION_CALL_OUTPUT),
            send_meta,
        )
    )
    send_meta[PLACEHOLDER_KEY] = True
    _complete(channel, send_meta)
    assert captured == ["已完成"]


@pytest.mark.parametrize("relative, carries_trace", [
    ("plugins/agentteams-matrix-channel/agentteams_matrix/channel.py", False),
    ("plugins/agentteams-matrix/adapters/qwenpaw/agentteams_matrix/channel.py", True),
])
@pytest.mark.parametrize("outcome", ["confirmed", "rejected", "notice"])
@pytest.mark.parametrize("placeholder", [False, True])
def test_only_acknowledged_manager_final_text_completes_source(monkeypatch, relative, carries_trace, outcome, placeholder):
    from types import SimpleNamespace
    from unittest.mock import AsyncMock

    spec = importlib.util.spec_from_file_location("_manager_final_channel", REPO_ROOT / relative)
    module = importlib.util.module_from_spec(spec)
    monkeypatch.setitem(sys.modules, spec.name, module)
    spec.loader.exec_module(module)
    cls = module.AgentTeamsMatrixChannel
    for base in cls.__mro__[1:]:
        if "_on_process_completed" in base.__dict__:
            monkeypatch.setattr(base, "_on_process_completed", None)
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "manager")
    channel = cls(process=lambda *a, **kw: None, share_session_in_group=True)
    channel._session_receipts = SimpleNamespace(complete=AsyncMock())
    channel._send_typing = AsyncMock()
    channel._prepare_room_send = AsyncMock()
    channel._evaluate_send_gate = lambda *_a: ("pass", 0)
    response = SimpleNamespace(event_id="$reply" if outcome != "rejected" else None)
    channel._client = SimpleNamespace(joined_members=AsyncMock(return_value=None),
                                       room_send=AsyncMock(return_value=response))

    async def send(to, text, meta):
        await channel._room_send_with_retry(to, "m.room.message", {
            "msgtype": "m.notice" if outcome == "notice" else "m.text", "body": text,
        })
    async def edit(to, meta, text, **kwargs):
        assert kwargs["msgtype"] == "m.text"
        await send(to, text, meta)

    channel.send = send
    channel._edit_thread_root = edit
    trace_context = {"source_event_id": "$source"}
    metadata = {module._MATRIX_STREAMING_FINAL_TEXT_KEY: "final answer", "room_id": ROOM,
                "event_id": "$source", module._MATRIX_PLACEHOLDER_THREAD_ROOT_KEY: placeholder,
                "otel_delivery": trace_context}
    asyncio.run(channel._on_process_completed(SimpleNamespace(), ROOM, metadata))
    if outcome == "confirmed":
        kwargs = {"trace_context": trace_context} if carries_trace else {}
        channel._session_receipts.complete.assert_awaited_once_with(ROOM, "$source", **kwargs)
    else:
        channel._session_receipts.complete.assert_not_awaited()


def test_manager_completion_receipt_retries_from_the_shared_outbox(monkeypatch, tmp_path):
    from types import SimpleNamespace
    from unittest.mock import Mock

    monkeypatch.syspath_prepend(str(REPO_ROOT / "shared/lib"))
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "manager")
    monkeypatch.setenv("AGENTTEAMS_WORKER_HOME", str(tmp_path))
    from agentteams_receipts import RECEIPT_TYPE
    import agentteams_receipts

    path = CHANNEL_MODULE_PATH.with_name("session_receipts.py")
    spec = importlib.util.spec_from_file_location("_manager_session_receipts", path)
    receipts = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(receipts)
    channel = SimpleNamespace(homeserver="http://fixture.invalid", _client=SimpleNamespace(access_token="fixture"))
    failed = Mock(side_effect=OSError("first delivery interrupted"))
    succeeded = Mock(return_value={"event_id": "$completed"})

    async def run():
        monkeypatch.setattr(agentteams_receipts, "matrix_request", lambda *_: failed)
        original = receipts.SessionReceipts(channel, tmp_path / "sessions")
        await original.complete(ROOM, "$source")
        await original.flush()
        monkeypatch.setattr(agentteams_receipts, "matrix_request", lambda *_: succeeded)
        restored = receipts.SessionReceipts(channel, tmp_path / "sessions")
        await restored.flush()
        await restored.flush()

    asyncio.run(run())
    succeeded.assert_called_once()
    assert failed.call_args == succeeded.call_args
    assert succeeded.call_args.args[2] == {
        "version": 1, "status": "completed", "m.relates_to": {"rel_type": RECEIPT_TYPE, "event_id": "$source"},
    }
