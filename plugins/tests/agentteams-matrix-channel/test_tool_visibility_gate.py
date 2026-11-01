"""``show_tool_calls`` / ``show_tool_results`` gates for tool rendering.

The channel parsed both flags from config, but the constructor never stored
them and the tool branches of ``on_event_message_completed`` rendered
unconditionally (``_thread_content_parts`` even forced tool visibility on).
When a switch is False the corresponding events must be dropped before any
thread action; with the default True the behavior is unchanged.
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
ACTIVITY_KEY = channel_module._MATRIX_TURN_TOOL_ACTIVITY_KEY


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


def test_from_config_saves_tool_visibility_flags():
    """The constructor must keep the flags it is given (previously dropped)."""
    channel = AgentTeamsMatrixChannel.from_config(
        lambda *args, **kwargs: None,
        {"show_tool_calls": False, "show_tool_results": False},
    )
    assert channel.show_tool_calls is False
    assert channel.show_tool_results is False

    default_channel = AgentTeamsMatrixChannel.from_config(
        lambda *args, **kwargs: None, {}
    )
    assert default_channel.show_tool_calls is True
    assert default_channel.show_tool_results is True


def test_tool_call_gated_off_drops_before_any_thread_action():
    """show_tool_calls=False: no activity marker, no thread root, no flush,
    no send."""
    channel = _make_channel(show_tool_calls=False)
    event = SimpleNamespace(type=_enum("FUNCTION_CALL"))
    meta: dict = {}
    with patch.object(channel, "_ensure_thread_root", AsyncMock()) as root, patch.object(
        channel, "_flush_pending_final_message_to_thread", AsyncMock()
    ) as flush, patch.object(
        channel, "_send_or_queue_thread_parts", AsyncMock()
    ) as send:
        asyncio.run(channel.on_event_message_completed(None, ROOM, event, meta))
    root.assert_not_awaited()
    flush.assert_not_awaited()
    send.assert_not_awaited()
    assert ACTIVITY_KEY not in meta


def test_tool_call_default_on_regression():
    """show_tool_calls default True: a completed tool call still routes."""
    channel = _make_channel()
    event = SimpleNamespace(type=_enum("FUNCTION_CALL"))
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
    assert meta.get(ACTIVITY_KEY) is True


def test_tool_output_gated_off_drops_before_any_thread_action():
    """show_tool_results=False: no media send and no activity marker."""
    channel = _make_channel(show_tool_results=False)
    event = SimpleNamespace(type=_enum("FUNCTION_CALL_OUTPUT"))
    meta: dict = {}
    with patch.object(
        channel, "_flush_pending_final_message_to_thread", AsyncMock()
    ) as flush, patch.object(channel, "send_content_parts", AsyncMock()) as send:
        asyncio.run(channel.on_event_message_completed(None, ROOM, event, meta))
    flush.assert_not_awaited()
    send.assert_not_awaited()
    assert ACTIVITY_KEY not in meta


def test_tool_output_default_on_sends_media_parts_regression():
    """show_tool_results default True: media parts from a tool output are
    still sent."""
    channel = _make_channel()
    event = SimpleNamespace(type=_enum("FUNCTION_CALL_OUTPUT"))
    parts = [object()]
    meta: dict = {}
    with patch.object(
        channel, "_tool_output_media_parts", return_value=parts
    ), patch.object(channel, "send_content_parts", AsyncMock()) as send, patch.object(
        channel, "_flush_pending_final_message_to_thread", AsyncMock()
    ) as flush:
        asyncio.run(channel.on_event_message_completed(None, ROOM, event, meta))
    flush.assert_awaited_once()
    send.assert_awaited_once_with(ROOM, parts, meta)
    assert meta.get(ACTIVITY_KEY) is True


def _capture_thread_content_parts_style(channel) -> dict:
    captured: dict = {}

    class _CapturingRenderer:
        def __init__(self, style):
            captured["style"] = style

        def message_to_parts(self, event):
            return []

    channel._renderer = _CapturingRenderer(None)
    channel._thread_content_parts(SimpleNamespace())
    return captured["style"].display_config


def test_thread_content_parts_honours_tool_visibility_flags():
    """_thread_content_parts must pass the channel's flags to the renderer
    (the hardcoded show_tool_* = True override is gone)."""
    channel = _make_channel(show_tool_calls=False, show_tool_results=False)
    display_config = _capture_thread_content_parts_style(channel)
    assert display_config.show_tool_calls is False
    assert display_config.show_tool_results is False


def test_thread_content_parts_defaults_tool_visibility_on():
    """Default channel: tool visibility in the thread renderer stays on."""
    channel = _make_channel()
    display_config = _capture_thread_content_parts_style(channel)
    assert display_config.show_tool_calls is True
    assert display_config.show_tool_results is True


def test_tool_call_still_routes_when_only_show_thinking_off():
    """Regression guard: the show_thinking gate must not swallow tool events."""
    channel = _make_channel(show_thinking=False)
    event = SimpleNamespace(type=_enum("FUNCTION_CALL"))
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
    assert meta.get(ACTIVITY_KEY) is True
