"""Install/uninstall idempotence tests for the agentteams-teamharness bundle.

Exercises the real bundle scripts twice each (install, install, uninstall,
uninstall) against a fake QwenPaw working directory and a fake ``qwenpaw``
executable on PATH. No real QwenPaw service is required; the fake records
every invocation so the test can prove the second install is a no-op.
"""

import json
import os
import subprocess
from pathlib import Path

GOLDEN_DIR = Path(__file__).resolve().parent
REPO_ROOT = GOLDEN_DIR.parents[3]
BUNDLE_DIR = REPO_ROOT / "plugins" / "agentteams-teamharness"
INSTALL = BUNDLE_DIR / "scripts" / "install.sh"
UNINSTALL = BUNDLE_DIR / "scripts" / "uninstall.sh"


def _make_fake_qwenpaw(bin_dir: Path, call_log: Path) -> None:
    script = f"""#!/usr/bin/env bash
printf '%s\\n' "$*" >> "$(printf '%s' {call_log})"
exit 0
"""
    fake = bin_dir / "qwenpaw"
    fake.write_text(script, encoding="utf-8")
    fake.chmod(0o755)


def _env(tmp_path: Path, working_dir: Path, bin_dir: Path) -> dict:
    env = dict(os.environ)
    env["HOME"] = str(tmp_path)
    env["QWENPAW_WORKING_DIR"] = str(working_dir)
    env["PATH"] = f"{bin_dir}{os.pathsep}{env.get('PATH', '')}"
    env.pop("TEAMHARNESS_INSTALL_LOG", None)
    return env


def _read_calls(call_log: Path) -> list:
    if not call_log.is_file():
        return []
    return [
        line
        for line in call_log.read_text(encoding="utf-8").splitlines()
        if line.strip()
    ]


def _run(script: Path, env: dict) -> None:
    result = subprocess.run(
        ["bash", str(script)],
        env=env,
        cwd=REPO_ROOT,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    assert result.returncode == 0, (
        f"{script.name} failed: {result.stdout}\n{result.stderr}"
    )


def test_install_twice_then_uninstall_twice(tmp_path):
    working_dir = tmp_path / "qwenpaw-workdir"
    working_dir.mkdir()
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    call_log = tmp_path / "qwenpaw-calls.log"
    _make_fake_qwenpaw(bin_dir, call_log)
    env = _env(tmp_path, working_dir, bin_dir)

    state_root = working_dir / ".agentteams"
    marker = state_root / "agentteams-teamharness" / "installed.json"

    # Install #1: builds the package and registers it through the CLI.
    _run(INSTALL, env)
    assert marker.is_file()
    recorded = json.loads(marker.read_text(encoding="utf-8"))
    assert recorded["bundle"] == "agentteams-teamharness"
    assert recorded["pluginId"] == "teamharness"
    assert recorded["contentHash"]
    first_calls = _read_calls(call_log)
    assert len(first_calls) == 1
    assert first_calls[0].startswith("plugin install ")
    assert first_calls[0].endswith("--force")

    # Install #2: marker matches the bundle content, so the installer is a
    # no-op and the QwenPaw CLI is not invoked again.
    _run(INSTALL, env)
    assert _read_calls(call_log) == first_calls
    assert json.loads(marker.read_text(encoding="utf-8")) == recorded

    # Uninstall #1: CLI uninstall + marker/state removal.
    _run(UNINSTALL, env)
    assert not marker.exists()
    assert not state_root.exists(), "installer state residue left behind"
    uninstall_calls = _read_calls(call_log)
    assert any(
        call == "plugin uninstall teamharness" for call in uninstall_calls
    )

    # Uninstall #2: no-op, still clean.
    _run(UNINSTALL, env)
    assert not marker.exists()
    assert not state_root.exists()
    assert not (working_dir / "plugins").exists() or not any(
        (working_dir / "plugins").iterdir()
    )
