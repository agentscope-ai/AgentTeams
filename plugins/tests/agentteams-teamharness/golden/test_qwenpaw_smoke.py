"""QwenPaw install smoke test for the agentteams-teamharness bundle.

Builds the QwenPaw package with the bundle's ruby build script, installs it
through the real ``qwenpaw`` CLI, and asserts that the installed plugin
keeps the unchanged legacy identity (``id == "teamharness"``). Skipped when
the environment has no qwenpaw CLI or no ruby (both are available in CI;
neither is installed in the QC container).

Iron rules:

- Every qwenpaw invocation runs with ``QWENPAW_WORKING_DIR`` pointed at a
  pytest tmp directory. The real working directory is never touched.
- The build runs with ``OUT_DIR`` and ``PYTHONPYCACHEPREFIX`` inside the
  tmp directory, so no artifact lands in the repo tree.
"""

import json
import os
import shutil
import subprocess
import zipfile
from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).resolve().parents[4]
BUNDLE_DIR = REPO_ROOT / "plugins" / "agentteams-teamharness"
BUILD_SCRIPT = (
    BUNDLE_DIR / "adapters" / "qwenpaw" / "scripts" / "build-qwenpaw-plugin.rb"
)
MANIFEST = BUNDLE_DIR / "plugin.yaml"

EXPECTED_PLUGIN_ID = "teamharness"
EXPECTED_VERSION = "0.1.0"


def _is_installed_plugins_root(working_dir: Path) -> Path:
    return working_dir / "plugins"


def test_qwenpaw_install_keeps_legacy_identity(tmp_path: Path) -> None:
    if shutil.which("qwenpaw") is None or shutil.which("ruby") is None:
        pytest.skip(
            "qwenpaw CLI and ruby are required for the QwenPaw install "
            "smoke test; neither is installed in this environment"
        )

    # Iron rule: qwenpaw always points at a throwaway working directory.
    working_dir = tmp_path / "qwenpaw-workdir"
    working_dir.mkdir()
    env = dict(os.environ)
    env["QWENPAW_WORKING_DIR"] = str(working_dir)
    env["OUT_DIR"] = str(tmp_path / "dist")
    env["PYTHONPYCACHEPREFIX"] = str(tmp_path / "pycache")
    env.pop("TEAMHARNESS_INSTALL_LOG", None)

    # 1. Build the QwenPaw package from the bundle manifest.
    result = subprocess.run(
        ["ruby", str(BUILD_SCRIPT), str(MANIFEST)],
        cwd=REPO_ROOT,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    assert result.returncode == 0, (
        f"build failed: {result.stdout}\n{result.stderr}"
    )
    zip_path = Path(result.stdout.strip().splitlines()[-1])
    assert zip_path.is_file(), f"build did not produce a zip at {zip_path}"

    # 2. Unpack the package and install it through the real CLI.
    extract_dir = tmp_path / "extract"
    with zipfile.ZipFile(zip_path) as archive:
        archive.extractall(extract_dir)
    package_dirs = [p for p in extract_dir.iterdir() if p.is_dir()]
    assert len(package_dirs) == 1, (
        f"expected a single package root in the zip, got {package_dirs}"
    )
    package_dir = package_dirs[0]
    assert (package_dir / "plugin.json").is_file()

    result = subprocess.run(
        ["qwenpaw", "plugin", "install", str(package_dir), "--force"],
        cwd=REPO_ROOT,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    assert result.returncode == 0, (
        f"qwenpaw plugin install failed: {result.stdout}\n{result.stderr}"
    )

    # 3. Assert the installed plugin kept the unchanged legacy identity.
    installed = None
    plugins_root = _is_installed_plugins_root(working_dir)
    if plugins_root.is_dir():
        for candidate in sorted(plugins_root.rglob("plugin.json")):
            manifest = json.loads(candidate.read_text(encoding="utf-8"))
            if manifest.get("id") == EXPECTED_PLUGIN_ID:
                installed = manifest
                break
    assert installed is not None, (
        f"no installed plugin with id {EXPECTED_PLUGIN_ID!r} found under "
        f"{plugins_root}"
    )
    assert installed["version"] == EXPECTED_VERSION

    # 4. Clean up through the CLI when it supports uninstall.
    help_result = subprocess.run(
        ["qwenpaw", "plugin", "--help"],
        cwd=REPO_ROOT,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if help_result.returncode == 0 and "uninstall" in help_result.stdout:
        result = subprocess.run(
            ["qwenpaw", "plugin", "uninstall", EXPECTED_PLUGIN_ID],
            cwd=REPO_ROOT,
            env=env,
            input="y\n",
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
        assert result.returncode == 0, (
            f"qwenpaw plugin uninstall failed: "
            f"{result.stdout}\n{result.stderr}"
        )


def test_reinstall_after_native_uninstall(tmp_path: Path) -> None:
    """A native ``qwenpaw plugin uninstall`` removes the installed plugin
    directory but leaves the bundle marker behind. The next install must
    verify the installed payload and reinstall, not take the marker's
    no-op path and report "up to date".
    """
    if shutil.which("qwenpaw") is None or shutil.which("ruby") is None:
        pytest.skip(
            "qwenpaw CLI and ruby are required for the native-uninstall "
            "reinstall test; neither is installed in this environment"
        )

    # Iron rule: qwenpaw always points at a throwaway working directory.
    working_dir = tmp_path / "qwenpaw-workdir"
    working_dir.mkdir()
    env = dict(os.environ)
    env["QWENPAW_WORKING_DIR"] = str(working_dir)
    env["OUT_DIR"] = str(tmp_path / "dist")
    env["PYTHONPYCACHEPREFIX"] = str(tmp_path / "pycache")
    env.pop("TEAMHARNESS_INSTALL_LOG", None)

    install = BUNDLE_DIR / "adapters" / "qwenpaw" / "install.sh"
    plugin_dir = working_dir / "plugins" / EXPECTED_PLUGIN_ID
    marker = working_dir / ".agentteams" / "teamharness" / "installed.json"

    # 1. Install through the bundle script (real CLI, real build).
    result = subprocess.run(
        ["bash", str(install)],
        cwd=REPO_ROOT,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    assert result.returncode == 0, (
        f"install failed: {result.stdout}\n{result.stderr}"
    )
    assert (plugin_dir / "plugin.json").is_file()
    recorded = json.loads(marker.read_text(encoding="utf-8"))
    assert recorded["pluginDir"] == str(plugin_dir)
    assert recorded["files"]["plugin.json"]

    # 2. Native uninstall: the CLI removes the plugin directory, the
    #    bundle marker survives.
    result = subprocess.run(
        ["qwenpaw", "plugin", "uninstall", EXPECTED_PLUGIN_ID],
        cwd=REPO_ROOT,
        env=env,
        input="y\n",
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    assert result.returncode == 0, (
        f"qwenpaw plugin uninstall failed: {result.stdout}\n{result.stderr}"
    )
    assert not plugin_dir.exists()
    assert marker.is_file(), "native uninstall is expected to keep the marker"

    # 3. Reinstall: the stale marker must not take the no-op path.
    result = subprocess.run(
        ["bash", str(install)],
        cwd=REPO_ROOT,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    assert result.returncode == 0, (
        f"reinstall failed: {result.stdout}\n{result.stderr}"
    )
    assert "up to date" not in result.stdout
    assert (plugin_dir / "plugin.json").is_file()
