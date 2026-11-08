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

This test pins the detector contract on both shipped copies of the module:

  1. the live ``plugins/agentteams-matrix-channel`` implementation, and
  2. the QwenPaw bundle adapter copy under
     ``plugins/agentteams-matrix/adapters/qwenpaw`` (``channel.py`` is the
     bundle-owned exception in the copy-drift contract, so the two files can
     diverge and must be pinned to the same behavior).

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
CHANNEL_COPY_PATHS = {
    "channel": (
        REPO_ROOT
        / "plugins"
        / "agentteams-matrix-channel"
        / "agentteams_matrix"
        / "channel.py"
    ),
    "bundle_adapter": (
        REPO_ROOT
        / "plugins"
        / "agentteams-matrix"
        / "adapters"
        / "qwenpaw"
        / "agentteams_matrix"
        / "channel.py"
    ),
}


def _load_copy(label: str, path: Path):
    spec = importlib.util.spec_from_file_location(
        f"agentteams_matrix_channel_{label}", path
    )
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


COPIES = {
    label: _load_copy(label, path) for label, path in CHANNEL_COPY_PATHS.items()
}

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
@pytest.mark.parametrize("label", sorted(COPIES))
def test_no_reply_final_line_contract(label, text, expected):
    detector = COPIES[label]._ends_with_no_reply_control
    assert detector(text) is expected


@pytest.mark.parametrize("text,expected", CASES)
def test_copies_agree(text, expected):
    """The channel copy and the bundle adapter copy stay behaviorally in sync."""
    results = {
        label: COPIES[label]._ends_with_no_reply_control(text)
        for label in sorted(COPIES)
    }
    assert len(set(results.values())) == 1
    assert next(iter(results.values())) is expected


@pytest.mark.parametrize("label", sorted(COPIES))
def test_whitespace_only_does_not_raise(label):
    """The historical crash: whitespace-only text raised IndexError."""
    detector = COPIES[label]._ends_with_no_reply_control
    for text in (" ", "\n", " \n\t\n  "):
        detector(text)  # must not raise
