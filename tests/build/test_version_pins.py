"""P3c: OpenClaw upstream tag pin (no native Matrix patch series).

The AgentTeams Matrix bundle is a separate plugin/config projection; it does
not patch OpenClaw's ``extensions/matrix`` sources. This test asserts the
OpenClaw pin is an upstream tag and that no AgentTeams native patch context
leaks back into the build.
"""

from __future__ import annotations

import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]

VERSIONS_ENV = ROOT / "hack" / "versions.env"
OPENCLAW_DOCKERFILE = ROOT / "openclaw-base" / "Dockerfile"
MAKEFILE = ROOT / "Makefile"

OPENCLAW_TAG = "v2026.10.6"
UPSTREAM_REPO = "https://github.com/openclaw/openclaw.git"


def _parse_env(path: Path) -> dict[str, str]:
    env: dict[str, str] = {}
    for raw in path.read_text(encoding="utf-8").splitlines():
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        assert "=" in line, f"{path.name}: not KEY=value: {raw!r}"
        key, value = line.split("=", 1)
        assert re.fullmatch(r"[A-Z0-9_]+", key), f"{path.name}: bad key {key!r}"
        assert value, f"{path.name}: empty value for {key}"
        env[key] = value
    return env


def test_versions_env_is_openclaw_only() -> None:
    env = _parse_env(VERSIONS_ENV)
    assert env == {"OPENCLAW_TAG": OPENCLAW_TAG}


def test_openclaw_pins_upstream_tag() -> None:
    text = OPENCLAW_DOCKERFILE.read_text(encoding="utf-8")
    assert "npm install -g pnpm@12.15.0" in text
    assert "pnpm@10." not in text
    assert "pnpm install --frozen-lockfile" in text
    assert "matrix-sdk-crypto-nodejs/package.json" in text
    assert f"ARG OPENCLAW_REPO_URL={UPSTREAM_REPO}" in text
    assert f"ARG OPENCLAW_TAG={OPENCLAW_TAG}" in text
    fetch_line = next(
        (ln for ln in text.splitlines() if "git fetch" in ln and "origin" in ln),
        "",
    )
    assert '"${OPENCLAW_TAG}"' in fetch_line, f"git fetch does not use the tag: {fetch_line!r}"
    bare = re.findall(r"\b[0-9a-f]{40}\b", text)
    assert not bare, f"bare 40-hex commit(s) in Dockerfile: {bare}"


def test_no_native_matrix_patch_series() -> None:
    text = OPENCLAW_DOCKERFILE.read_text(encoding="utf-8")
    assert "agentteams-patches" not in text
    assert "agentteams patch series" not in text
    assert not (ROOT / "hack" / "patches" / "openclaw").exists()
    assert not (ROOT / "hack" / "check-openclaw-patches.sh").exists()


def test_makefile_has_no_openclaw_patch_context() -> None:
    text = MAKEFILE.read_text(encoding="utf-8")
    assert "openclaw-patches" not in text


def test_dot_git_is_kept_for_online_upgrade() -> None:
    text = OPENCLAW_DOCKERFILE.read_text(encoding="utf-8")
    # The final cleanup must not remove .git (P7 uses the git checkout).
    assert "\n        .git .github \\\n" not in text
    assert "\n        .github \\\n" in text
