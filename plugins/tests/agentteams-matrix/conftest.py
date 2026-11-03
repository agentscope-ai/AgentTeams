"""Shared pytest configuration for agentteams-matrix bundle tests.

Puts the bundle root on ``sys.path`` so the runtime-neutral core package
(``plugins/agentteams-matrix/core``) can be imported as ``core.*``.
"""

import sys
from pathlib import Path

BUNDLE_DIR = Path(__file__).resolve().parents[2] / "agentteams-matrix"
if str(BUNDLE_DIR) not in sys.path:
    sys.path.insert(0, str(BUNDLE_DIR))
