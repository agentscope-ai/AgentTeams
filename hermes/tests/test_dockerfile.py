"""Hermes image contract: pinned tag, editable install, matrix extra, Python.

Upstream hermes-agent blocks wheel/sdist builds (``setup.py`` raises "Building
wheels or sdists for hermes-agent is not supported") and directs development
installs to editable mode; its pyproject pins every direct dependency exactly.
The image must therefore keep the path attached to ``--editable`` (pip parses
``--editable --index-url ... /path`` as an editable requirement named
``--index-url``), take the matrix transport from the upstream ``matrix`` extra
rather than hand-maintained pins, stay on the 3.11 image the pinned tag
supports (``requires-python >=3.11,<3.14``), and keep ``hack/versions.env`` in
sync with the Dockerfile ARG.
"""

from __future__ import annotations

import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
HERMES_DOCKERFILE = ROOT / "hermes" / "Dockerfile"
VERSIONS_ENV = ROOT / "hack" / "versions.env"


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


def _arg(text: str, name: str) -> str:
    match = re.search(rf"^ARG {name}=(\S+)", text, re.MULTILINE)
    assert match, f"hermes/Dockerfile: no ARG {name}"
    return match.group(1)


def test_hermes_pin_matches_versions_env() -> None:
    env = {}
    for raw in VERSIONS_ENV.read_text(encoding="utf-8").splitlines():
        line = raw.strip()
        if line and not line.startswith("#") and "=" in line:
            key, value = line.split("=", 1)
            env[key] = value
    dockerfile = HERMES_DOCKERFILE.read_text(encoding="utf-8")
    assert _arg(dockerfile, "HERMES_GIT_REF") == env["HERMES_GIT_REF"]


def test_hermes_editable_path_is_attached_to_the_option() -> None:
    text = HERMES_DOCKERFILE.read_text(encoding="utf-8")
    pip_lines = [
        line
        for line in _logical_lines(text)
        if "pip install" in line and "hermes-src" in line
    ]
    assert pip_lines, "Hermes Dockerfile has no pip install of /opt/hermes-src"
    editable_path = re.compile(r"--editable\s+\"?/opt/hermes-src(\[matrix\])?\"?")
    for line in pip_lines:
        assert editable_path.search(line), line
        assert not re.search(r"--editable\s+--[a-z]", line), line


def test_hermes_install_uses_the_upstream_matrix_extra() -> None:
    text = HERMES_DOCKERFILE.read_text(encoding="utf-8")
    install = [line for line in _logical_lines(text) if "hermes-src" in line]
    assert any("[matrix]" in line for line in install), (
        "the editable install must request the upstream matrix extra"
    )
    # Older images pinned the transport by hand; the upstream extra owns it now.
    assert '"mautrix[encryption]==0.21.1"' not in text, "hand-maintained mautrix pin"
    assert "matrix-nio" not in text, "matrix-nio is not used by the plugin overlay"


def test_hermes_image_stays_on_the_supported_python() -> None:
    text = HERMES_DOCKERFILE.read_text(encoding="utf-8")
    assert "python:3.14" not in text, "v2026.9.24 requires Python <3.14"
    assert re.search(r"^FROM .*python:3\.11", text, re.MULTILINE), (
        "the image must run the pinned tag's supported 3.11 interpreter"
    )
