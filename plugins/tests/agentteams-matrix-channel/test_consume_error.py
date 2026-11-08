#!/usr/bin/env python3
"""Consume-error room send sanitization (#1334) for the AgentTeams Matrix
channel.

Non-cancellation consume/streaming failures must not leak the raw err_text
(e.g. a bare ``Internal error``) into the room: the room receives only the
sanitized ``_CONSUME_ERROR_NOTICE`` while the raw text is kept in the logs.
Cancellation noise stays suppressed, and the thread-root status edits
(已取消 / 处理异常) are unchanged.
"""

from __future__ import annotations

import asyncio
import importlib.util
import logging
import sys
from pathlib import Path
from unittest.mock import AsyncMock, patch

import pytest

pytest.importorskip("nio")
# channel.py imports the qwenpaw runtime at module load; skip cleanly when it
# is absent (the dedicated CI job installs it, so the suite runs there).
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
# Register before exec: channel.py uses @dataclass with string annotations,
# which resolve the owning module via sys.modules.
sys.modules["agentteams_matrix_channel"] = channel_module
_spec.loader.exec_module(channel_module)

AgentTeamsMatrixChannel = channel_module.AgentTeamsMatrixChannel
_CONSUME_ERROR_NOTICE = channel_module._CONSUME_ERROR_NOTICE

ROOM = "!roomid:matrix.local"
ROOT_ID = "$rootevent:matrix.local"
ROOT_KEY = channel_module._MATRIX_OWN_THREAD_ROOT_KEY


def _make_channel(**overrides):
    kwargs = {
        "process": lambda *args, **kw: None,
        "share_session_in_group": True,
    }
    kwargs.update(overrides)
    return AgentTeamsMatrixChannel(**kwargs)


def _sent_text_from_room_send(room_send) -> str:
    """Extract the room-facing text from a captured send_content_parts call.

    The base ``_on_consume_error`` forwards its err_text into the room via
    ``send_content_parts(to_handle, [TextContent(...)], meta)``; that seam is
    what the user actually sees.
    """
    to_handle, parts, _meta = room_send.await_args.args
    assert to_handle == ROOM
    return "".join(getattr(part, "text", "") for part in parts)


def test_cancellation_path_suppressed():
    """Cancellation errors stay silent in the room; thread root shows 已取消."""
    channel = _make_channel()
    channel._active_thread_roots[ROOM] = ROOT_ID
    with patch.object(channel, "_edit_thread_root", AsyncMock()) as edit, patch.object(
        channel, "_send_typing", AsyncMock()
    ) as typing, patch.object(
        channel, "send_content_parts", AsyncMock()
    ) as room_send:
        asyncio.run(
            channel._on_consume_error(None, ROOM, "Task has been cancelled")
        )
    edit.assert_awaited_once_with(ROOM, {ROOT_KEY: ROOT_ID}, "已取消")
    room_send.assert_not_awaited()
    typing.assert_awaited_once_with(ROOM, False)


def test_generic_error_sanitized(caplog):
    """Raw error text is logged; the room only gets the sanitized notice."""
    channel = _make_channel()
    channel._active_thread_roots[ROOM] = ROOT_ID
    raw_err = "Internal error: upstream LLM unreachable (traceback...)"
    with caplog.at_level(
        logging.WARNING, logger="qwenpaw.channels.matrix"
    ), patch.object(channel, "_edit_thread_root", AsyncMock()), patch.object(
        channel, "send_content_parts", AsyncMock()
    ) as room_send:
        asyncio.run(channel._on_consume_error(object(), ROOM, raw_err))
    room_send.assert_awaited_once()
    sent_text = _sent_text_from_room_send(room_send)
    assert "Internal error" not in sent_text
    assert sent_text == _CONSUME_ERROR_NOTICE
    assert raw_err in caplog.text


def test_thread_root_status_regression():
    """Non-cancellation errors mark the thread root 处理异常; a missing
    active thread root must not crash and the masked notice still sends."""
    channel = _make_channel()
    channel._active_thread_roots[ROOM] = ROOT_ID
    with patch.object(channel, "_edit_thread_root", AsyncMock()) as edit, patch.object(
        channel, "send_content_parts", AsyncMock()
    ) as room_send:
        asyncio.run(channel._on_consume_error(object(), ROOM, "Internal error"))
    edit.assert_awaited_once_with(ROOM, {ROOT_KEY: ROOT_ID}, "处理异常")
    room_send.assert_awaited_once()
    assert _sent_text_from_room_send(room_send) == _CONSUME_ERROR_NOTICE

    # The first call popped the root; without an active thread root the
    # handler must not crash and the masked notice still reaches the room.
    with patch.object(channel, "_edit_thread_root", AsyncMock()) as edit2, patch.object(
        channel, "send_content_parts", AsyncMock()
    ) as room_send2:
        asyncio.run(channel._on_consume_error(object(), ROOM, "Internal error"))
    edit2.assert_not_awaited()
    room_send2.assert_awaited_once()
    assert _sent_text_from_room_send(room_send2) == _CONSUME_ERROR_NOTICE
