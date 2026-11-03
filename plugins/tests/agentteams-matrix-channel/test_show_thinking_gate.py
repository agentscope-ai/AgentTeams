"""``show_thinking`` gate for reasoning output.

The channel parses ``show_thinking`` from config (default True), but the
reasoning stream tail and completed REASONING messages used to reach the
room regardless. When the switch is False neither path may send anything;
with the default True the behavior is unchanged byte for byte.
"""

from __future__ import annotations

import asyncio
import importlib.util
import sys
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch

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
PENDING_KEY = channel_module._MATRIX_PENDING_FINAL_MESSAGE_KEY


def _enum(name: str) -> object:
    """Fake enum-like value: the channel's _enum_name() reads .name."""
    value = SimpleNamespace()
    value.name = name
    return value


def _make_channel(**overrides):
    kwargs = {
        "process": lambda *args, **kw: None,
        "share_session_in_group": True,
    }
    kwargs.update(overrides)
    return AgentTeamsMatrixChannel(**kwargs)


def test_reasoning_stream_end_gated_off():
    """show_thinking=False: the reasoning streaming tail never reaches the room."""
    channel = _make_channel(show_thinking=False)
    with patch.object(channel, "_ensure_thread_root", AsyncMock()) as root, patch.object(
        channel, "_send_streaming_thread_text", AsyncMock()
    ) as send:
        meta: dict = {}
        asyncio.run(
            channel.on_streaming_end(
                None, ROOM, SimpleNamespace(), meta, "reasoning", "long thinking"
            )
        )
    send.assert_not_awaited()
    root.assert_not_awaited()
    assert channel_module._MATRIX_STREAMING_REASONING_EVENT_ID_KEY not in meta


def test_reasoning_stream_end_default_on_regression():
    """show_thinking default True: the existing 'Thinking:' send is byte-identical."""
    channel = _make_channel()
    with patch.object(channel, "_ensure_thread_root", AsyncMock()), patch.object(
        channel, "_send_streaming_thread_text", AsyncMock()
    ) as send:
        asyncio.run(
            channel.on_streaming_end(
                None, ROOM, SimpleNamespace(), {}, "reasoning", "long thinking"
            )
        )
    send.assert_awaited_once()
    assert send.await_args.args[2] == "Thinking:\n\nlong thinking"


def test_completed_reasoning_gated_off():
    """show_thinking=False: a completed REASONING event is dropped before the thread root."""
    channel = _make_channel(show_thinking=False)
    event = SimpleNamespace(type=_enum("REASONING"))
    with patch.object(channel, "_ensure_thread_root", AsyncMock()) as root, patch.object(
        channel, "_flush_pending_final_message_to_thread", AsyncMock()
    ) as flush, patch.object(
        channel, "_message_to_content_parts", return_value=[]
    ):
        asyncio.run(channel.on_event_message_completed(None, ROOM, event, {}))
    root.assert_not_awaited()
    flush.assert_not_awaited()


def test_completed_message_branch_unaffected_by_gate():
    """The gate only targets REASONING; a normal MESSAGE event still routes."""
    channel = _make_channel(show_thinking=False)
    event = SimpleNamespace(type=_enum("MESSAGE"))
    meta: dict = {}
    with patch.object(channel, "_ensure_thread_root", AsyncMock()) as root, patch.object(
        channel, "_flush_pending_final_message_to_thread", AsyncMock()
    ):
        asyncio.run(channel.on_event_message_completed(None, ROOM, event, meta))
    root.assert_awaited_once()
    assert meta.get(PENDING_KEY) is event


def test_completed_reasoning_default_on_routes():
    """show_thinking default True: a completed REASONING event still routes."""
    channel = _make_channel()
    event = SimpleNamespace(type=_enum("REASONING"))
    parts = [object()]
    meta: dict = {}
    with patch.object(channel, "_ensure_thread_root", AsyncMock()) as root, patch.object(
        channel, "_flush_pending_final_message_to_thread", AsyncMock()
    ) as flush, patch.object(
        channel, "_message_to_content_parts", return_value=parts
    ), patch.object(
        channel, "_send_or_queue_thread_parts", AsyncMock()
    ) as send:
        asyncio.run(channel.on_event_message_completed(None, ROOM, event, meta))
    root.assert_awaited_once()
    flush.assert_awaited_once()
    send.assert_awaited_once_with(ROOM, parts, meta)
    assert channel_module._MATRIX_FORCE_NOTICE_KEY not in meta


def test_streaming_end_clears_prepopulated_metadata_when_gated_off():
    """show_thinking=False: stale reasoning-stream metadata is still cleaned up."""
    channel = _make_channel(show_thinking=False)
    reasoning_keys = (
        channel_module._MATRIX_STREAMING_REASONING_EVENT_ID_KEY,
        channel_module._MATRIX_STREAMING_REASONING_LAST_EDIT_KEY,
        channel_module._MATRIX_STREAMING_REASONING_STREAM_ID_KEY,
    )
    meta = {key: "stream-state" for key in reasoning_keys}
    with patch.object(channel, "_ensure_thread_root", AsyncMock()) as root, patch.object(
        channel, "_send_streaming_thread_text", AsyncMock()
    ) as send:
        asyncio.run(
            channel.on_streaming_end(
                None, ROOM, SimpleNamespace(), meta, "reasoning", "long thinking"
            )
        )
    send.assert_not_awaited()
    root.assert_not_awaited()
    for key in reasoning_keys:
        assert key not in meta
