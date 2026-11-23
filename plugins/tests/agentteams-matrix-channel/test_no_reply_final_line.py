#!/usr/bin/env python3
"""NO_REPLY final-line detector regression for the AgentTeams Matrix channel.

``_ends_with_no_reply_control`` implements the send-side NO_REPLY protocol:
it reports whether the final non-empty output line is the bare ``NO_REPLY``
token, so the channel can suppress the send. Its documented contract is
"final non-empty output line" — an output that contains no non-empty line at
all (whitespace-only text: ``""``, ``"\\n"``, ``"  \\n\\t"``) therefore must
classify as ``False``.

An earlier revision implemented this as
``bool(text) and text.rstrip().splitlines()[-1].strip() == "NO_REPLY"``.
``bool(text)`` only excludes the empty string: for whitespace-only text
``rstrip()`` produces ``""`` whose ``splitlines()`` is empty, and the
unconditional ``[-1]`` raises ``IndexError``. The detector is the first
statement of ``AgentTeamsMatrixChannel.send``, so a whitespace-only final
output crashes the delivery path instead of being handled as an ordinary
(non-NO_REPLY) send.

This test pins the QwenPaw channel detector contract.

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
CHANNEL_PATH = REPO_ROOT / "qwenpaw/adapters/matrix/agentteams_matrix/channel.py"


def _load_channel(path: Path):
    spec = importlib.util.spec_from_file_location(
        "agentteams_matrix_no_reply_test", path
    )
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


CHANNEL = _load_channel(CHANNEL_PATH)

# (text, expected) — the detector contract.
CASES = [
    # Bare token, the suppression contract itself.
    ("NO_REPLY", True),
    ("NO_REPLY\n", True),
    ("  NO_REPLY  ", True),
    # Substantive text is never suppressed.
    ("hello", False),
    ("hello\nworld", False),
    ("hello\nNO_REPLY\nworld\n", False),
    # The token must be the *final non-empty* line; trailing blank lines and
    # whitespace around it do not change the verdict.
    ("hello\nNO_REPLY\n   \n\t\n", True),
    # Empty output: nothing to send, nothing to suppress.
    ("", False),
    # Regression: whitespace-only output has no final non-empty line — the
    # detector must return False, not raise IndexError.
    (" ", False),
    ("\n", False),
    (" \n\t\n  ", False),
]


@pytest.mark.parametrize("text,expected", CASES)
def test_no_reply_final_line_contract(text, expected):
    detector = CHANNEL._ends_with_no_reply_control
    assert detector(text) is expected


def test_whitespace_only_does_not_raise():
    """The historical crash: whitespace-only text raised IndexError."""
    detector = CHANNEL._ends_with_no_reply_control
    for text in (" ", "\n", " \n\t\n  "):
        detector(text)  # must not raise
