"""Test environment: fake core on the import path, clean sidecar env.

The tests run without a real homeserver, matrix-nio or network:
MATRIX_CORE_DIR points at the in-tree fake package (tests/fake_core),
which implements the exact core surface the sidecar touches. All
sidecar-related environment is cleared per test (and re-set where a
test needs it), and HOME is redirected so the default config/socket
paths land in the test's tmp dir.
"""

from __future__ import annotations

import os
import sys
from pathlib import Path

import pytest

TESTS_DIR = Path(__file__).resolve().parent
PLUGIN_ROOT = TESTS_DIR.parents[1]
FAKE_CORE = TESTS_DIR / "fake_core"

# The sidecar package lives at the plugin root; make it importable no
# matter which directory pytest is invoked from.
if str(PLUGIN_ROOT) not in sys.path:
    sys.path.insert(0, str(PLUGIN_ROOT))

SIDECAR_ENV = (
    "AGENTTEAMS_MATRIX_URL",
    "AGENTTEAMS_MATRIX_USER",
    "AGENTTEAMS_MATRIX_TOKEN",
    "AGENTTEAMS_MATRIX_SOCKET",
    "AGENTTEAMS_MATRIX_AGENT",
    "AGENTTEAMS_MATRIX_SIDECAR_CONFIG",
    "AGENTTEAMS_MATRIX_STATE_DIR",
    "AGENTTEAMS_OPENCLAW_STATE_DIR",
    "MATRIX_CORE_DIR",
)


@pytest.fixture(autouse=True)
def sidecar_env(monkeypatch, tmp_path):
    for name in SIDECAR_ENV:
        monkeypatch.delenv(name, raising=False)
    monkeypatch.setenv("MATRIX_CORE_DIR", str(FAKE_CORE))
    monkeypatch.setenv("HOME", str(tmp_path))
    yield
