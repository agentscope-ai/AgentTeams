"""P3b: runtime version pins + OpenClaw patch series checks.

Covers:
1. hack/versions.env is sourceable KEY=value with exactly 3 non-empty keys.
2. openclaw-base/Dockerfile checks out via $OPENCLAW_TAG (no bare 40-hex commit).
3. hermes/Dockerfile pins HERMES_GIT_REF=v0.22.5.
4. Every hack/patches/openclaw/0*.patch parses (git apply --stat, syntax level).
5. hack/check-openclaw-patches.sh is syntactically valid and --help exits 0.
Plus: a conditional end-to-end apply gate that runs the check script against
a local OpenClaw checkout when that checkout actually matches the patch
base tree (recorded base blob shas), and skips with evidence when it does not.
"""

from __future__ import annotations

import re
import subprocess
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]

VERSIONS_ENV = ROOT / "hack" / "versions.env"
OPENCLAW_DOCKERFILE = ROOT / "openclaw-base" / "Dockerfile"
HERMES_DOCKERFILE = ROOT / "hermes" / "Dockerfile"
MAKEFILE = ROOT / "Makefile"
PATCH_DIR = ROOT / "hack" / "patches" / "openclaw"
CHECK_SCRIPT = ROOT / "hack" / "check-openclaw-patches.sh"

OPENCLAW_TAG = "v2026.10.6"
HERMES_TAG = "v0.22.5"

# (path, base blob sha) pairs recorded during the 2026-10-28 verification
# pass. Used to decide whether a local checkout matches the patch base tree.
RECORDED_BASE_BLOBS = [
    ("src/auto-reply/reply/history.ts", "ab75f86534d0d6bbd099a5d90eb28fb93e27a808"),
    ("src/gateway/config-reload-plan.ts", "1ab4dac66a7b5aee3fe2a655f13f5007a865094c"),
    (
        "extensions/matrix/src/matrix/client/shared.ts",
        "ff13bb0e6c9f8f16634edbc8dcd6d1e8e693ad51",
    ),
    (
        "extensions/matrix/src/matrix/client-bootstrap.ts",
        "28e72fd72745140fa51c11e6c41da778162270c5",
    ),
]


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


def _run(cmd: list[str], cwd: Path | None = None) -> subprocess.CompletedProcess:
    return subprocess.run(
        cmd, cwd=str(cwd) if cwd else None, capture_output=True, text=True
    )


def test_versions_env_has_three_nonempty_keys() -> None:
    env = _parse_env(VERSIONS_ENV)
    assert set(env) == {"OPENCLAW_TAG", "HERMES_TAG", "QWENPAW_WHEEL"}
    assert env["OPENCLAW_TAG"] == OPENCLAW_TAG
    assert env["HERMES_TAG"] == HERMES_TAG
    assert env["QWENPAW_WHEEL"].startswith("qwenpaw==")


def test_openclaw_dockerfile_uses_tag_not_bare_commit() -> None:
    text = OPENCLAW_DOCKERFILE.read_text(encoding="utf-8")
    assert f"ARG OPENCLAW_TAG={OPENCLAW_TAG}" in text
    assert "${OPENCLAW_TAG}" in text
    fetch_line = next(
        (ln for ln in text.splitlines() if "git fetch" in ln and "origin" in ln),
        "",
    )
    assert "OPENCLAW_TAG" in fetch_line, f"git fetch does not use the tag: {fetch_line!r}"
    bare = re.findall(r"\b[0-9a-f]{40}\b", text)
    assert not bare, f"bare 40-hex commit(s) in Dockerfile: {bare}"
    # The patch series is applied and committed in-image.
    assert 'commit -m "agentteams patch series"' in text
    # .git must be kept (P7 rebase) — it must not be in the final rm list.
    rm_block = text.split("rm -rf \\")[-1]
    assert not re.search(r"^\s*\.git\s*\\?\s*$", rm_block, re.MULTILINE)
    # The Dockerfile ARG default must match versions.env.
    assert _parse_env(VERSIONS_ENV)["OPENCLAW_TAG"] == OPENCLAW_TAG


def test_makefile_passes_openclaw_patches_build_context() -> None:
    text = MAKEFILE.read_text(encoding="utf-8")
    assert "openclaw-patches=./hack/patches/openclaw" in text


def test_hermes_dockerfile_pins_version() -> None:
    text = HERMES_DOCKERFILE.read_text(encoding="utf-8")
    assert f"ARG HERMES_GIT_REF={HERMES_TAG}" in text
    assert "hack/versions.env" in text
    clone_line = next(
        (ln for ln in text.splitlines() if "git clone" in ln and "HERMES_GIT_REF" in ln),
        "",
    )
    assert "${HERMES_GIT_REF}" in clone_line, f"git clone does not use the ref: {clone_line!r}"


def test_all_openclaw_patches_parse() -> None:
    patches = sorted(PATCH_DIR.glob("0*.patch"))
    assert len(patches) == 8, f"expected 8 patches, found {[p.name for p in patches]}"
    for patch in patches:
        r = _run(["git", "apply", "--stat", str(patch)], cwd=ROOT)
        assert r.returncode == 0, f"{patch.name}: git apply --stat failed: {r.stderr}"


def test_check_script_syntax_and_help() -> None:
    r = _run(["bash", "-n", str(CHECK_SCRIPT)])
    assert r.returncode == 0, f"bash -n failed: {r.stderr}"
    r = _run(["bash", str(CHECK_SCRIPT), "--help"])
    assert r.returncode == 0, f"--help exited {r.returncode}: {r.stderr}"
    assert "Usage" in r.stdout


def _git(repo: Path, *args: str) -> subprocess.CompletedProcess:
    return _run(["git", "-C", str(repo), *args])


def _base_tree_mismatches(repo: Path) -> list[str]:
    mismatches = []
    for rel, blob in RECORDED_BASE_BLOBS:
        r = _git(repo, "rev-parse", f"HEAD:{rel}")
        if r.returncode != 0:
            mismatches.append(f"{rel}: missing in checkout")
        elif r.stdout.strip() != blob:
            mismatches.append(f"{rel}: got {r.stdout.strip()[:12]} want {blob[:12]}")
    return mismatches


def _default_openclaw_checkout() -> Path | None:
    """Same default as check-openclaw-patches.sh: <git main root>/../OpenClaw."""
    r = _run(["git", "rev-parse", "--show-toplevel"], cwd=ROOT)
    if r.returncode != 0:
        return None
    toplevel = Path(r.stdout.strip())
    c = _run(["git", "rev-parse", "--git-common-dir"], cwd=toplevel)
    if c.returncode != 0:
        return None
    common = c.stdout.strip()
    if not common.startswith("/"):
        common = str(toplevel / common)
    return Path(common).parent / "../OpenClaw"


def test_check_script_full_apply_against_matching_checkout() -> None:
    repo = _default_openclaw_checkout()
    if repo is not None and repo.is_dir():
        repo = repo.resolve()
        if _git(repo, "rev-parse", "--git-dir").returncode != 0:
            repo = None
    if repo is None:
        pytest.skip("no local OpenClaw checkout found")
    mismatches = _base_tree_mismatches(repo)
    if mismatches:
        pytest.skip(
            f"{repo} does not match the patch base tree ({'; '.join(mismatches)}); "
            "the series is instead verified byte-for-byte against the recorded "
            "post-patch GitHub blob shas. Point --repo at a higress-group/openclaw "
            "checkout of the pinned state to run the full gate."
        )
    r = _run(["bash", str(CHECK_SCRIPT), "--repo", str(repo)])
    assert r.returncode == 0, f"check script failed: {r.stdout}{r.stderr}"
    assert "OK 8 patches apply to" in r.stdout
