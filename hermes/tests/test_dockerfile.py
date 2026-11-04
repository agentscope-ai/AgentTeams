"""Hermes image contract: pinned tag, uv sync --frozen, matrix extra, Python.

Upstream hermes-agent pins every direct dependency exactly and resolves two
metadata conflicts (Pillow 13 vs browser-harness 0.1.13, cryptography vs
alibabacloud-tea-openapi) through ``[tool.uv] override-dependencies``; pip
cannot apply overrides, so a pip install of the matrix extra fails with
ResolutionImpossible on 3.14. The image must therefore install hermes-agent
with ``uv sync --frozen`` against the upstream ``uv.lock`` (which covers
3.14 only), take the matrix transport from the upstream ``matrix`` extra
rather than hand-maintained pins, run the python:3.14 interpreter required
by the pinned tag's marker-gated core dependencies, and keep
``hack/versions.env`` in sync with the Dockerfile ARG.
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


def test_hermes_install_uses_uv_with_the_upstream_lock() -> None:
    text = HERMES_DOCKERFILE.read_text(encoding="utf-8")
    logical = _logical_lines(text)
    sync_lines = [line for line in logical if "uv sync" in line]
    assert sync_lines, "Hermes Dockerfile must install hermes-agent through uv sync"
    line = sync_lines[0]
    assert "--frozen" in line, line
    assert "--extra matrix" in line, line
    assert "--project /opt/hermes-src" in line, line
    assert "UV_PROJECT_ENVIRONMENT=/opt/venv/hermes" in line, line
    assert "python -m venv /opt/venv/hermes" not in text, "uv owns the venv"
    commands = [
        segment
        for line in logical
        if not line.lstrip().startswith("#")
        for segment in re.split(r"&&", line)
    ]
    assert not any(
        "pip install" in command and "/opt/hermes-src" in command for command in commands
    ), (
        "pip cannot apply the upstream [tool.uv] overrides; the pip install of "
        "/opt/hermes-src must be gone"
    )
    assert _arg(text, "UV_VERSION"), "uv must be pinned"


def test_hermes_install_uses_the_upstream_matrix_extra() -> None:
    text = HERMES_DOCKERFILE.read_text(encoding="utf-8")
    logical = _logical_lines(text)
    assert any("--extra matrix" in line for line in logical if "uv sync" in line), (
        "uv sync must request the upstream matrix extra"
    )
    # Older images pinned the transport by hand; the upstream extra owns it now.
    assert '"mautrix[encryption]==0.21.1"' not in text, "hand-maintained mautrix pin"
    assert "matrix-nio" not in text, "matrix-nio is not used by the plugin overlay"


def test_hermes_image_runs_the_pinned_tag_python() -> None:
    text = HERMES_DOCKERFILE.read_text(encoding="utf-8")
    # v0.22.7's core dependencies are marker-gated to python_version >= 3.14,
    # so the runtime interpreter must be 3.14.
    assert re.search(r"^FROM .*python:3\.14-slim", text, re.MULTILINE), (
        "the image must run the 3.14 interpreter the pinned tag requires"
    )
    assert "python3.14/site-packages" in text, (
        "the src overlay must target the 3.14 site-packages"
    )
