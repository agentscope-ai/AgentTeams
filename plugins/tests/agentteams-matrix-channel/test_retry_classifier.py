#!/usr/bin/env python3
"""Retry-classifier regression for the AgentTeams Matrix channel.

The send-retry helper ``_room_send_with_retry`` classifies each nio
``RoomSendError`` via the static method ``_is_retryable_room_send_response``.
The classifier references the per-class errcode whitelist via
``AgentTeamsMatrixChannel._RETRYABLE_MATRIX_ERRCODES``; an earlier revision
used the stale name ``MatrixChannel._RETRYABLE_MATRIX_ERRCODES`` and raised
``NameError`` on the very first transient response, breaking every retry.

This test pins three contracts:

  1. ``M_LIMIT_EXCEEDED`` is classified as retryable.
  2. ``M_UNKNOWN`` is classified as retryable.
  3. Other errcodes (e.g. ``M_FORBIDDEN``) are *not* retryable by errcode,
     but the classifier does not raise ``NameError`` while consulting the
     whitelist.

NOTE: channel.py imports the qwenpaw runtime (schemas / BaseChannel). This
module runs for real in the dedicated matrix-channel CI job (which installs
matrix-nio + qwenpaw) and in the QwenPaw dev container; in environments
without those runtime deps it skips (importorskip) instead of erroring.
"""

from __future__ import annotations

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


def _make_channel():
    return AgentTeamsMatrixChannel(
        process=lambda *args, **kw: None,
        share_session_in_group=True,
    )


def _make_roomsend_error(status_code: str | None = None):
    from nio import RoomSendError

    return RoomSendError(message="boom", status_code=status_code, room_id="!room:test")


def test_m_limit_exceeded_is_retryable():
    """M_LIMIT_EXCEEDED is in the whitelist and must be classified retryable."""
    channel = _make_channel()
    assert channel._is_retryable_room_send_response(
        _make_roomsend_error("M_LIMIT_EXCEEDED")
    ) is True


def test_m_unknown_is_retryable():
    """M_UNKNOWN is in the whitelist and must be classified retryable."""
    channel = _make_channel()
    assert channel._is_retryable_room_send_response(
        _make_roomsend_error("M_UNKNOWN")
    ) is True


def test_classifier_does_not_raise_nameerror_on_non_whitelisted_errcode():
    """The classifier must consult the per-class whitelist via the live
    class name; a stale ``MatrixChannel._RETRYABLE_MATRIX_ERRCODES`` lookup
    used to raise ``NameError`` and short-circuit every retry.
    """
    channel = _make_channel()
    # M_FORBIDDEN is intentionally not in the whitelist. The lookup must
    # still resolve without raising; the classifier then returns False.
    try:
        verdict = channel._is_retryable_room_send_response(
            _make_roomsend_error("M_FORBIDDEN")
        )
    except NameError as exc:  # pragma: no cover - explicit regression
        pytest.fail(
            "_is_retryable_room_send_response raised NameError; the "
            "RETRYABLE_MATRIX_ERRCODES lookup must use the live class "
            f"name. Original error: {exc!r}"
        )
    assert verdict is False


def test_whitelist_attribute_lives_on_agentteamsmatrixchannel():
    """The errcode whitelist must be reachable via the live class name."""
    assert hasattr(AgentTeamsMatrixChannel, "_RETRYABLE_MATRIX_ERRCODES")
    assert "M_LIMIT_EXCEEDED" in AgentTeamsMatrixChannel._RETRYABLE_MATRIX_ERRCODES
    assert "M_UNKNOWN" in AgentTeamsMatrixChannel._RETRYABLE_MATRIX_ERRCODES
