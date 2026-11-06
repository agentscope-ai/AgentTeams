"""Shared harness for the bundle L3/L4 lifecycle conformance suites (P-b/P-c).

``L3`` is *install -> load -> uninstall* on a throwaway HOME with no residue.
``L4`` is *N-1 install -> N upgrade -> N-1 rollback* with the plugin tree and
the ownership marker staying in step at every hop.

Everything here is black box: the real bundle scripts are executed with
``bash`` against a temporary HOME, and no live runtime, network or host state
is involved. A failure message carries the script's stdout/stderr so the
reason is readable without re-running anything by hand.
"""

from __future__ import annotations

import hashlib
import json
import os
import shutil
import subprocess
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]

# Variables through which a bundle script would discover (and touch) the
# host's real runtimes. They are scrubbed for every script invocation.
_HOST_RUNTIME_ENV = (
    "OPENCLAW_CONFIG",
    "OPENCLAW_CONFIG_PATH",
    "OPENCLAW_WORKSPACE",
    "HERMES_HOME",
    "HERMES_PLUGIN_HOME",
    "QWENPAW_WORKING_DIR",
    "QWENPAW_SECRET_DIR",
    "TEAMHARNESS_INSTALL_LOG",
    "TEAMHARNESS_RUNTIME_CONFIG",
)


def isolated_env(home: Path, **overrides: str) -> dict:
    """Environment for a bundle script: throwaway HOME, no host runtime state."""
    env = dict(os.environ)
    for name in _HOST_RUNTIME_ENV:
        env.pop(name, None)
    env["HOME"] = str(home)
    env.update(overrides)
    return env


def run(
    script: Path,
    *args: str,
    env: dict,
    expect: int | None = 0,
) -> subprocess.CompletedProcess:
    """Run a bundle script and fail with its output when the exit code lies."""
    result = subprocess.run(
        ["bash", str(script), *args],
        env=env,
        cwd=str(REPO_ROOT),
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        timeout=300,
    )
    if expect is not None:
        assert result.returncode == expect, (
            f"{script.name} {' '.join(args)} exited {result.returncode}, "
            f"expected {expect}\n"
            f"--- stdout ---\n{result.stdout}\n"
            f"--- stderr ---\n{result.stderr}"
        )
    return result


def files(root: Path) -> dict:
    """Map of relpath -> sha256 for every file under ``root``."""
    found = {}
    if root.is_dir():
        for path in sorted(root.rglob("*")):
            if path.is_file():
                found[path.relative_to(root).as_posix()] = hashlib.sha256(
                    path.read_bytes()
                ).hexdigest()
    return found


def residue(root: Path) -> list:
    """Files still present under ``root`` (empty list = clean uninstall)."""
    return sorted(files(root))


def read_json(path: Path) -> dict:
    return json.loads(path.read_text(encoding="utf-8"))


def read_yaml(path: Path):
    import yaml

    return yaml.safe_load(path.read_text(encoding="utf-8")) or {}


def stage_bundle(
    src: Path,
    dst: Path,
    *,
    version: str,
    add: dict | None = None,
    remove: tuple = (),
) -> Path:
    """Copy a bundle tree into a distinct release candidate (N-1 / N).

    ``version`` rewrites ``plugin.json`` so the two staged payloads are
    genuinely different releases; ``add``/``remove`` place the file that only
    one of the two releases carries, which is what makes an upgrade and a
    rollback observable.
    """
    shutil.copytree(src, dst)
    manifest = dst / "plugin.json"
    data = json.loads(manifest.read_text(encoding="utf-8"))
    data["version"] = version
    manifest.write_text(json.dumps(data, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    for rel in remove:
        (dst / rel).unlink()
    for rel, content in (add or {}).items():
        target = dst / rel
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(content, encoding="utf-8")
    return dst


def compile_python_tree(root: Path) -> None:
    """Byte-compile every installed ``.py`` file (a real loadability check).

    The adapters that only run inside their host runtime import modules that
    do not exist outside it, so an ``import`` would test the host, not the
    payload. Compiling the installed tree proves the runtime can load the
    files it was given.
    """
    import py_compile

    for path in sorted(root.rglob("*.py")):
        try:
            py_compile.compile(str(path), doraise=True, cfile=str(path) + ".pyc-check")
        except py_compile.PyCompileError as exc:  # pragma: no cover - failure path
            raise AssertionError(f"installed payload does not compile: {path}: {exc}") from exc
        finally:
            Path(str(path) + ".pyc-check").unlink(missing_ok=True)


def node_check(module: Path) -> None:
    """Syntax-check a staged JS/ESM plugin with node when it is available."""
    node = shutil.which("node")
    if node is None:
        return
    result = subprocess.run(
        [node, "--check", str(module)],
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    assert result.returncode == 0, (
        f"node --check rejected {module.name}:\n{result.stderr}"
    )
