"""Hermes image: keep the editable path attached to its pip option.

pip 26.2.2 parses ``--editable --index-url ... /opt/hermes-src`` as an
editable requirement named ``--index-url`` and fails before resolving the
source tree.  Keep the path directly after ``--editable`` and put index
options before it.
"""

from __future__ import annotations

import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
HERMES_DOCKERFILE = ROOT / "hermes" / "Dockerfile"


def _logical_lines(text: str) -> list[str]:
    lines: list[str] = []
    current = ""
    for raw in text.splitlines():
        line = raw.rstrip()
        if line.endswith("\\"):
            current += line[:-1] + " "
            continue
        current += line
        if current.strip():
            lines.append(current.strip())
        current = ""
    if current.strip():
        lines.append(current.strip())
    return lines


def test_hermes_editable_path_is_attached_to_the_option() -> None:
    text = HERMES_DOCKERFILE.read_text(encoding="utf-8")
    pip_lines = [line for line in _logical_lines(text) if "pip install" in line and "hermes-src" in line]
    assert pip_lines, "Hermes Dockerfile has no pip install of /opt/hermes-src"
    for line in pip_lines:
        assert "--editable /opt/hermes-src" in line or "--editable=/opt/hermes-src" in line, line
        assert not re.search(r"--editable\s+--[a-z]", line), line
