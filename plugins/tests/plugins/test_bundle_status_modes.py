"""Exercise every advertised ``status.sh`` runtime mode on an isolated HOME.

The bundle status scripts dispatch on ``--runtime`` and each branch builds its
own JSON. A branch that misses an import still exits non-zero only when the
caller actually asks for that runtime, so the modes are exercised here with a
throwaway HOME (and an isolated ``QWENPAW_WORKING_DIR``) instead of trusting
the default path.
"""

from __future__ import annotations

import json
import os
import subprocess
from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).resolve().parents[3]


@pytest.mark.parametrize("bundle,runtime", [
    ("agentteams-matrix", "openclaw"),
    ("agentteams-teamharness", "openclaw"),
    ("agentteams-teamharness", "hermes"),
])
def test_status_reports_each_runtime_as_json(tmp_path: Path, bundle: str, runtime: str) -> None:
    home = tmp_path / "home"
    home.mkdir()
    working = tmp_path / "qwenpaw"
    working.mkdir()
    env = {
        "HOME": str(home),
        "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
        "QWENPAW_WORKING_DIR": str(working),
    }
    script = REPO_ROOT / "plugins" / bundle / "scripts" / "status.sh"
    assert script.is_file(), f"missing status script: {script}"

    result = subprocess.run(
        ["bash", str(script), "--runtime", runtime, "--json"],
        env=env,
        text=True,
        capture_output=True,
        cwd=str(REPO_ROOT),
        timeout=120,
    )
    assert result.returncode == 0, (
        f"{bundle} status.sh --runtime {runtime} exited {result.returncode}\n"
        f"--- stdout ---\n{result.stdout}\n--- stderr ---\n{result.stderr}"
    )
    payload = json.loads(result.stdout)
    assert payload["runtime"] == runtime
    assert payload["installed"] is False, (
        "an isolated HOME cannot already have the plugin installed"
    )


def test_retired_matrix_hermes_status_is_rejected(tmp_path: Path) -> None:
    script = REPO_ROOT / "plugins/agentteams-matrix/scripts/status.sh"
    result = subprocess.run(
        ["bash", str(script), "--runtime", "hermes", "--json"],
        env={"HOME": str(tmp_path), "PATH": os.environ.get("PATH", "/usr/bin:/bin")},
        text=True,
        capture_output=True,
        cwd=str(REPO_ROOT),
        timeout=120,
    )
    assert result.returncode == 2
    assert "unknown runtime: hermes" in result.stderr
    assert result.stdout == ""
