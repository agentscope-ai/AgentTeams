"""P3: runtime version pins (OpenClaw tag + QwenPaw pip spec).

The container build has two independent version pins:

- ``OPENCLAW_TAG`` in ``openclaw-base/Dockerfile`` — the upstream OpenClaw
  release tag the base image installs (via ``npx -y openclaw@<tag>``).
- ``QWENPAW_PIP_SPEC`` in ``qwenpaw/Dockerfile`` and
  ``manager/Dockerfile.qwenpaw`` — the ``qwenpaw==X.Y.Z`` pip spec the
  QwenPaw runtime image installs.

This test file is the single source of truth for both: it reads
``hack/versions.env`` and asserts that the Dockerfiles' ``ARG`` defaults
still match the pin, so a pin bump that forgets to update a Dockerfile
fails CI instead of shipping a mismatched image.

It also guards the derived openclaw-base tag line:
``OPENCLAW_BASE_TAG = <OPENCLAW_TAG without the leading v> - node<NODE_MIN_VERSION>``,
and that no build-reachable file hard-codes the legacy April base tag.
The legacy April base tag must not reappear anywhere a build can read it.
"""

from __future__ import annotations

import re
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
OPENCLAW_TAG = "v2026.10.9"
QWENPAW_PIP_SPEC = "qwenpaw==2.2.2.post1"
UPSTREAM_REPO = "https://github.com/openclaw/openclaw"

# The legacy April base tag (split so this file itself is not a
# hard-coded occurrence). No build-reachable file may carry it.
LEGACY_BASE_TAG = "".join(("20260423", "-8359cbc"))

# Files a build can actually read (Makefile + the three Dockerfiles that
# reference the base image) — the strongest ban, checked byte-for-byte.
BASE_TAG_CONSUMER_FILES = (
    "Makefile",
    "manager/Dockerfile",
    "worker/Dockerfile",
    "manager/docker-legacy/Dockerfile.aliyun",
)

# Source trees scanned for any stray hard-coded tag (legacy or otherwise).
# Workflow scripts (``hack/*.sh``, ``.github/workflows/*.yml``) are in the
# scan, not the byte-for-byte consumer list: they may legitimately *derive*
# the tag, they must not pin it.
BASE_TAG_CONSUMER_GLOBS = (
    (".github/workflows", "*.yml"),
    ("hack", "*.sh"),
)

#: Every pin in hack/versions.env maps to (dockerfile path, ARG name) —
#: or, for QWENPAW_PIP_SPEC, to the two Dockerfiles that consume it
#: (qwenpaw runtime image + manager variant).
PIN_TARGETS: dict[str, "tuple[str, str] | list[tuple[str, str]]"] = {
    "OPENCLAW_TAG": ("openclaw-base/Dockerfile", "OPENCLAW_TAG"),
    "QWENPAW_PIP_SPEC": [
        ("qwenpaw/Dockerfile", "QWENPAW_PIP_SPEC"),
        ("manager/Dockerfile.qwenpaw", "QWENPAW_PIP_SPEC"),
    ],
}


def _load_env() -> dict[str, str]:
    env = {}
    for line in (REPO / "hack/versions.env").read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        env[key.strip()] = value.strip()
    return env


def _iter_targets(name: str):
    target = PIN_TARGETS[name]
    return target if isinstance(target, list) else [target]


def _dockerfile_arg(dockerfile: str, arg: str) -> str:
    """Return the ARG default for ``arg`` in the given Dockerfile."""
    text = (REPO / dockerfile).read_text(encoding="utf-8")
    match = re.search(rf"^ARG {arg}=(\S+)$", text, re.MULTILINE)
    assert match, f"ARG {arg} not found in {dockerfile}"
    return match.group(1)


def test_versions_env_contains_only_known_pins():
    env = _load_env()
    # OPENCLAW_BASE_TAG is derived (asserted by
    # test_openclaw_base_tag_is_derived), not a source pin, so it is
    # expected in the env file but not in PIN_TARGETS.
    assert set(env) == set(PIN_TARGETS) | {"OPENCLAW_BASE_TAG"}, (
        f"hack/versions.env keys changed: {sorted(env)}"
    )
    assert env["OPENCLAW_TAG"] == OPENCLAW_TAG
    assert env["QWENPAW_PIP_SPEC"] == QWENPAW_PIP_SPEC


def test_openclaw_base_tag_is_derived():
    env = _load_env()
    base = env.get("OPENCLAW_BASE_TAG", "")
    openclaw_tag = env.get("OPENCLAW_TAG", "")
    dockerfile = (REPO / "openclaw-base/Dockerfile").read_text(encoding="utf-8")
    assert f"ARG OPENCLAW_TAG={openclaw_tag}" in dockerfile
    m = re.search(r"^ARG NODE_MIN_VERSION=(\S+)$", dockerfile, re.MULTILINE)
    assert m, "ARG NODE_MIN_VERSION not found in openclaw-base/Dockerfile"
    derived = openclaw_tag.lstrip("v") + "-node" + m.group(1)
    assert base == derived, (
        f"OPENCLAW_BASE_TAG={base!r} is not derived from "
        f"OPENCLAW_TAG={openclaw_tag!r} + NODE_MIN_VERSION={m.group(1)!r} "
        f"(expected {derived!r})"
    )


def test_no_hard_coded_openclaw_base_tag():
    for rel in BASE_TAG_CONSUMER_FILES:
        text = (REPO / rel).read_text(encoding="utf-8")
        assert LEGACY_BASE_TAG not in text, (
            f"legacy base tag hard-coded in {rel}; "
            "derive it from OPENCLAW_TAG + NODE_MIN_VERSION instead"
        )
    for directory, pattern in BASE_TAG_CONSUMER_GLOBS:
        for path in sorted((REPO / directory).glob(pattern)):
            text = path.read_text(encoding="utf-8")
            assert LEGACY_BASE_TAG not in text, (
                f"legacy base tag hard-coded in {path.relative_to(REPO)}; "
                "derive it from OPENCLAW_TAG + NODE_MIN_VERSION instead"
            )


def test_pin_targets_match_versions_env():
    env = _load_env()
    for name in PIN_TARGETS:
        for dockerfile, arg in _iter_targets(name):
            assert env[name] == _dockerfile_arg(dockerfile, arg), (
                f"versions.env {name}={env[name]} but {dockerfile} has ARG {arg}="
                f"{_dockerfile_arg(dockerfile, arg)}"
            )


def test_openclaw_pins_upstream_tag():
    """The Dockerfile installs exactly the pinned upstream tag.

    The pin is a release tag (v2026.10.9), not a commit hash — this
    asserts the install line references the pin and the upstream repo.
    """
    openclaw_df = REPO / "openclaw-base/Dockerfile"
    text = openclaw_df.read_text(encoding="utf-8")
    assert f"ARG OPENCLAW_TAG={OPENCLAW_TAG}" in text
    assert f"npx -y openclaw@${{OPENCLAW_TAG}}" in text
    assert f"ARG OPENCLAW_REPO={UPSTREAM_REPO}" in text


def test_no_native_matrix_patch_series():
    """The native Matrix series is no longer applied to OpenClaw.

    19 patches under ``openclaw-base/patches-native-matrix/`` were deleted
    in the v2026.6.9 upgrade (native channel now ships). The Dockerfile
    must reference none of them.
    """
    text = (REPO / "openclaw-base/Dockerfile").read_text(encoding="utf-8")
    assert "patches-native-matrix" not in text
    assert "patch -Np1" not in text


def test_makefile_has_no_openclaw_patch_context():
    """Makefile: no leftover -C openclaw-base / patch-application lines."""
    text = (REPO / "Makefile").read_text(encoding="utf-8")
    assert "cd openclaw-base && patch" not in text
    assert "patches-native-matrix" not in text
    # The build still passes the openclaw tag as an ARG (pin propagation).
    assert "OPENCLAW_TAG" in text


def test_dot_git_is_kept_for_online_upgrade():
    """``.git`` is deliberately preserved in openclaw-base for ``openclaw update``.

    The COPY of the unpacked npm tarball must include ``.github`` (so the
    multi-line COPY list still has its trailing continuation), and the
    Dockerfile must document the ``.git`` retention.
    """
    text = (REPO / "openclaw-base/Dockerfile").read_text(encoding="utf-8")
    assert "\n        .github \\\n" in text
    assert ".git .github \\" not in text
