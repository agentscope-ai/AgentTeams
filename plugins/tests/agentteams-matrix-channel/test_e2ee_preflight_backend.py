#!/usr/bin/env python3
"""E2EE preflight backend-detection regression (P9, PR1 M5a).

matrix-nio 0.26 ships E2EE through vodozemac and no longer provides a
top-level olm module, so the preflight must decide on the detected
crypto backend (vodozemac or olm) instead of probing olm alone:

  - backend available, ``encryption=True``  -> encryption survives
  - no backend,      ``encryption=True``  -> disabled + error log
  - ``encryption=False``                   -> no-op, backend not probed

NOTE: the adapter channel module imports the qwenpaw runtime
(schemas / BaseChannel) and the bundle core package.  This module runs
for real in the dedicated matrix-channel CI job and in the QwenPaw
dev container; in environments without those runtime deps it skips
(importorskip) instead of erroring.
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
    / "agentteams-matrix"
    / "adapters"
    / "qwenpaw"
    / "agentteams_matrix"
    / "channel.py"
)

_spec = importlib.util.spec_from_file_location(
    "agentteams_matrix_adapter_channel", CHANNEL_MODULE_PATH
)
assert _spec is not None and _spec.loader is not None
adapter_channel = importlib.util.module_from_spec(_spec)
sys.modules["agentteams_matrix_adapter_channel"] = adapter_channel
_spec.loader.exec_module(adapter_channel)

AgentTeamsMatrixChannel = adapter_channel.AgentTeamsMatrixChannel
CHANNEL_LOGGER = "qwenpaw.channels.matrix"


def _make_channel(encryption: bool) -> "AgentTeamsMatrixChannel":
    return AgentTeamsMatrixChannel(
        process=lambda *args, **kw: None,
        share_session_in_group=True,
        encryption=encryption,
    )


def test_preflight_keeps_encryption_when_backend_available(
    monkeypatch,
):
    """A vodozemac-only runtime (matrix-nio 0.26+) must not be
    demoted: with a backend importable, encryption stays on."""
    monkeypatch.setattr(
        adapter_channel, "crypto_available", lambda: True
    )
    channel = _make_channel(encryption=True)
    channel._preflight_e2ee_dependencies()
    assert channel.encryption is True


def test_preflight_refuses_when_backend_missing(monkeypatch):
    """#1469 round 13: encryption requested + no olm -> refuse to start.

    The old behaviour demoted the channel to plaintext; that sends
    m.room.message into rooms whose state says encrypted.
    """
    monkeypatch.setattr(
        adapter_channel, "crypto_available", lambda: False
    )
    channel = _make_channel(encryption=True)
    with pytest.raises(RuntimeError, match="refusing to start in plaintext"):
        channel._preflight_e2ee_dependencies()
    # No silent demotion of the configured intent.
    assert channel.encryption is True


def test_preflight_noop_when_encryption_disabled(monkeypatch):
    """encryption=False never consults the backend probe."""
    calls: list[int] = []

    def _record() -> bool:
        calls.append(1)
        return True

    monkeypatch.setattr(adapter_channel, "crypto_available", _record)
    channel = _make_channel(encryption=False)
    channel._preflight_e2ee_dependencies()
    assert channel.encryption is False
    assert calls == []
