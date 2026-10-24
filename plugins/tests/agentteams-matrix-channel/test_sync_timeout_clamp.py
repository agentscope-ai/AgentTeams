#!/usr/bin/env python3
"""sync_timeout_ms clamp regression for the AgentTeams Matrix channel.

The constructor used to assign ``self.sync_timeout_ms = sync_timeout_ms``
verbatim, so a misconfigured JSON / env-injected dict could produce:

  - ``sync_timeout_ms = 0``   — nio's ``sync(timeout=0)`` becomes a
    non-blocking call, turning the sync loop into a busy-spin that
    hammers the homeserver.
  - ``sync_timeout_ms < 0``   — ``sync_s = self.sync_timeout_ms / 1000``
    feeds the downstream ``max(sync_s + 30, 60)`` clamp a negative
    duration; ``request_timeout`` then becomes whatever the floor
    says, masking the misconfiguration instead of surfacing it.

Pin the constructor's clamp to ``_MIN_SYNC_TIMEOUT_MS`` (1 second) so
both inputs collapse to a sane minimum and the rest of the consumer
chain can rely on a positive integer.

NOTE: channel.py imports the qwenpaw runtime (schemas / BaseChannel).
This module runs for real in the dedicated matrix-channel CI job and
in the QwenPaw dev container; in environments without those runtime
deps it skips (importorskip) instead of erroring.
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
MIN_SYNC_TIMEOUT_MS = channel_module._MIN_SYNC_TIMEOUT_MS


def _make_channel(**overrides):
    kwargs = {
        "process": lambda *args, **kw: None,
        "share_session_in_group": True,
    }
    kwargs.update(overrides)
    return AgentTeamsMatrixChannel(**kwargs)


def test_zero_sync_timeout_clamped_to_minimum():
    """``sync_timeout_ms=0`` must not survive the constructor: it would
    turn the nio ``sync(timeout=0)`` call into a busy-spin."""
    channel = _make_channel(sync_timeout_ms=0)
    assert channel.sync_timeout_ms == MIN_SYNC_TIMEOUT_MS
    assert channel.sync_timeout_ms > 0


def test_negative_sync_timeout_clamped_to_minimum():
    """A negative ``sync_timeout_ms`` must collapse to the floor so the
    downstream ``max(sync_s + 30, 60)`` clamp sees a positive input."""
    channel = _make_channel(sync_timeout_ms=-5000)
    assert channel.sync_timeout_ms == MIN_SYNC_TIMEOUT_MS


def test_default_sync_timeout_unchanged():
    """The default value (30s) must pass through untouched so existing
    deployments keep their original cadence."""
    channel = _make_channel()
    assert channel.sync_timeout_ms == 30000


def test_explicit_positive_sync_timeout_unchanged():
    """Any explicit positive value must be honored as-is."""
    channel = _make_channel(sync_timeout_ms=15000)
    assert channel.sync_timeout_ms == 15000
