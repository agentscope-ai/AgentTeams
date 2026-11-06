"""P3: runtime version pins (OpenClaw tag, Hermes git ref, QwenPaw pip spec).

The container build carries independent version pins, one per runtime:

- ``OPENCLAW_TAG`` in ``openclaw-base/Dockerfile`` — the upstream OpenClaw
  release tag the base image installs (via ``npx -y openclaw@<tag>``).
- ``HERMES_GIT_REF`` in ``hermes/Dockerfile`` — the hermes-agent release
  tag the Hermes worker image checks out.
- ``QWENPAW_PIP_SPEC`` in ``qwenpaw/Dockerfile`` and
  ``manager/Dockerfile.qwenpaw`` — the ``qwenpaw==X.Y.Z`` pip spec the
  QwenPaw runtime images install.

This test file is the single source of truth for all three: it reads
``hack/versions.env`` and asserts that the Dockerfiles' ``ARG`` defaults
still match the pins, so a pin bump that forgets to update a Dockerfile
fails CI instead of shipping a mismatched image.

It also asserts the OpenClaw pin is an upstream tag and that no AgentTeams
native patch context leaks back into the build, and it guards the derived
openclaw-base tag line: ``OPENCLAW_BASE_TAG = <OPENCLAW_TAG without the
leading v> - node<NODE_MIN_VERSION>``, with no build-reachable file
hard-coding the legacy April base tag.
"""

from __future__ import annotations

import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]

VERSIONS_ENV = ROOT / "hack" / "versions.env"
OPENCLAW_DOCKERFILE = ROOT / "openclaw-base" / "Dockerfile"
MAKEFILE = ROOT / "Makefile"

OPENCLAW_TAG = "v2026.10.9"
UPSTREAM_REPO = "https://github.com/openclaw/openclaw.git"

# Legacy April base pin — must not appear in any CI-consumed file.
# The value is assembled at runtime from fragments so the legacy tag does not
# occur as a contiguous literal in this source (or its .pyc): the gate-5
# repo-wide grep for the old tag must stay clean.
LEGACY_BASE_TAG = "".join(("20260423", "-8359cbc"))

#: CI-consumed files that may name the openclaw-base image (explicit files).
BASE_TAG_CONSUMER_FILES = (
    MAKEFILE,
    ROOT / "manager" / "Dockerfile",
    ROOT / "worker" / "Dockerfile",
    ROOT / "manager" / "docker-legacy" / "Dockerfile.aliyun",
)
#: CI-consumed files that may name the openclaw-base image (globs).
BASE_TAG_CONSUMER_GLOBS = (
    (ROOT / ".github" / "workflows", "*.yml"),
    (ROOT / "hack", "*.sh"),
)


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


def test_versions_env_holds_exactly_the_pinned_keys() -> None:
    """Every key in versions.env is accounted for: direct pins or derived ones.

    Strict equality - a new key must be classified here, not silently added.
    """
    env = _parse_env(VERSIONS_ENV)
    assert set(env) == set(PIN_TARGETS) | {"OPENCLAW_BASE_TAG"}, (
        f"hack/versions.env keys changed: {sorted(env)}"
    )
    assert env["OPENCLAW_TAG"] == OPENCLAW_TAG


def test_openclaw_base_tag_is_derived() -> None:
    env = _parse_env(VERSIONS_ENV)
    text = OPENCLAW_DOCKERFILE.read_text(encoding="utf-8")
    node_min = next(
        (ln.split("=", 1)[1] for ln in text.splitlines() if ln.startswith("ARG NODE_MIN_VERSION=")),
        "",
    )
    assert node_min, "openclaw-base/Dockerfile: ARG NODE_MIN_VERSION not found"
    expected = f"{env['OPENCLAW_TAG'].lstrip('v')}-node{node_min}"
    assert env["OPENCLAW_BASE_TAG"] == expected, (
        f"OPENCLAW_BASE_TAG must be {expected!r} "
        "(<OPENCLAW_TAG without the leading v>-node<NODE_MIN_VERSION>)"
    )


def test_no_hard_coded_openclaw_base_tag() -> None:
    env = _parse_env(VERSIONS_ENV)
    expected = env["OPENCLAW_BASE_TAG"]
    files = list(BASE_TAG_CONSUMER_FILES)
    files.extend(
        p
        for directory, pattern in BASE_TAG_CONSUMER_GLOBS
        for p in sorted(directory.glob(pattern))
    )
    assert files, "openclaw-base tag consumers vanished — extend the scan list"
    for path in files:
        text = path.read_text(encoding="utf-8")
        assert LEGACY_BASE_TAG not in text, (
            f"{path.relative_to(ROOT)}: legacy base tag {LEGACY_BASE_TAG!r} still present"
        )
        tags = [
            tag
            for tag in re.findall(r"openclaw-base[: @]([0-9][0-9A-Za-z.\-]*)", text)
            if not tag.startswith("$")
        ]
        for tag in tags:
            assert tag == expected, (
                f"{path.relative_to(ROOT)}: openclaw-base:{tag} does not match "
                f"OPENCLAW_BASE_TAG={expected}"
            )


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


# --- Hermes pin (#1472) -----------------------------------------------------
HERMES_GIT_REF = "v0.22.7"

# --- QwenPaw pin (#1471) ----------------------------------------------------
QWENPAW_PIP_SPEC = "qwenpaw==2.2.2.post1"

# Every pin in hack/versions.env must mirror the matching Dockerfile ARG
# default (Docker cannot source env files).
PIN_TARGETS = {
    "OPENCLAW_TAG": (("openclaw-base/Dockerfile", "OPENCLAW_TAG"),),
    "HERMES_GIT_REF": (("hermes/Dockerfile", "HERMES_GIT_REF"),),
    # The QwenPaw spec is consumed by two images (worker + Manager), so the
    # value is a tuple of (dockerfile, ARG) pairs.
    "QWENPAW_PIP_SPEC": (
        ("qwenpaw/Dockerfile", "QWENPAW_PIP_SPEC"),
        ("manager/Dockerfile.qwenpaw", "QWENPAW_PIP_SPEC"),
    ),
}


def _dockerfile_arg(dockerfile: str, arg: str) -> str:
    text = (ROOT / dockerfile).read_text(encoding="utf-8")
    match = re.search(rf"^ARG {arg}=(\S+)", text, re.MULTILINE)
    assert match, f"{dockerfile}: no ARG {arg} line"
    return match.group(1)


def test_versions_env_pins_match_dockerfile_args() -> None:
    env = _parse_env(VERSIONS_ENV)
    for key, targets in PIN_TARGETS.items():
        assert key in env, f"versions.env is missing the {key} pin"
        for dockerfile, arg in targets:
            assert env[key] == _dockerfile_arg(dockerfile, arg), (
                f"versions.env {key}={env[key]!r} != ARG {arg} in {dockerfile}"
            )
