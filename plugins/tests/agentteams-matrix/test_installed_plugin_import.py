#!/usr/bin/env python3
"""Installed-plugin import check for the QwenPaw adapter (P9/PR1 M3-fix).

Since M3 the channel imports the shared ``core`` package, so the installer
must ship the bundle's ``core/`` directory into the plugin target: an
installed plugin without it dies at load time with
``ModuleNotFoundError: No module named 'core'``. These tests run the real
install.sh / uninstall.sh (subprocess, operator-style) against a scratch
working directory and verify the installed plugin imports, and that
uninstall reconciles ``core/`` while keeping user files.
"""

from __future__ import annotations

import os
import shutil
import subprocess
import sys
from pathlib import Path

import pytest

BUNDLE_DIR = Path(__file__).resolve().parents[2] / "agentteams-matrix"
PLUGIN_ID = "agentteams-matrix-channel"


def _run(cmd: list, cwd: Path, env: dict) -> subprocess.CompletedProcess:
    return subprocess.run(
        cmd, cwd=str(cwd), env=env, capture_output=True, text=True
    )


def _operator_env(working_dir: Path) -> dict:
    env = dict(os.environ)
    env["QWENPAW_WORKING_DIR"] = str(working_dir)
    return env


def _install(bundle_copy: Path, working_dir: Path) -> str:
    proc = _run(
        ["bash", str(bundle_copy / "adapters" / "qwenpaw" / "install.sh")],
        cwd=bundle_copy,
        env=_operator_env(working_dir),
    )
    assert proc.returncode == 0, f"install.sh failed: {proc.stderr}"
    return proc.stdout


def _target(working_dir: Path) -> Path:
    return working_dir / "plugins" / PLUGIN_ID


@pytest.fixture()
def bundle_copy(tmp_path: Path) -> Path:
    copy = tmp_path / "bundle"
    shutil.copytree(BUNDLE_DIR, copy)
    return copy


def test_installed_plugin_imports_core(tmp_path: Path, bundle_copy: Path) -> None:
    working_dir = tmp_path / "work"
    _install(bundle_copy, working_dir)
    # Idempotency: a second install of the same payload is a no-op.
    assert "up to date" in _install(bundle_copy, working_dir)

    target = _target(working_dir)
    assert (target / "core" / "__init__.py").is_file()

    env = dict(os.environ)
    env["PYTHONPATH"] = str(target)
    proc = _run(
        [sys.executable, "-c", "import agentteams_matrix.channel"],
        cwd=target,
        env=env,
    )
    assert proc.returncode == 0, (
        f"installed plugin import failed: {proc.stderr}"
    )


def test_uninstall_removes_core_and_keeps_user_files(
    tmp_path: Path, bundle_copy: Path
) -> None:
    working_dir = tmp_path / "work"
    _install(bundle_copy, working_dir)

    target = _target(working_dir)
    (target / "notes.txt").write_text("user file\n", encoding="utf-8")

    proc = _run(
        ["bash", str(bundle_copy / "adapters" / "qwenpaw" / "uninstall.sh")],
        cwd=bundle_copy,
        env=_operator_env(working_dir),
    )
    assert proc.returncode == 0, f"uninstall.sh failed: {proc.stderr}"

    assert not (target / "core").exists()
    assert not (target / "agentteams_matrix").exists()
    assert (target / "notes.txt").is_file()
