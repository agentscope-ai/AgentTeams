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
