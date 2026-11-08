"""The QwenPaw channel package must not ship bytecode.

build-qwenpaw-plugin.rb syntax-checks the payload with py_compile before
zipping; the compilation output must land outside the staging root, so the
ZIP contains no __pycache__/ or .pyc entries. Skipped when ruby is
unavailable (QC container; CI installs ruby 3.3 in the job).
"""

import os
import shutil
import subprocess
import zipfile
from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).resolve().parents[3]
BUILD_SCRIPT = (
    REPO_ROOT
    / "plugins"
    / "agentteams-matrix"
    / "adapters"
    / "qwenpaw"
    / "scripts"
    / "build-qwenpaw-plugin.rb"
)


def test_channel_zip_contains_no_bytecode(tmp_path: Path) -> None:
    if shutil.which("ruby") is None:
        pytest.skip("ruby is required to build the QwenPaw channel package; not installed in this environment")

    result = subprocess.run(
        ["ruby", str(BUILD_SCRIPT)],
        cwd=REPO_ROOT,
        env={
            **os.environ,
            "OUT_DIR": str(tmp_path / "dist"),
            "PYTHONPYCACHEPREFIX": str(tmp_path / "pycache"),
        },
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    assert result.returncode == 0, f"build failed: {result.stdout}\n{result.stderr}"
    zip_path = Path(result.stdout.strip().splitlines()[-1])
    assert zip_path.is_file(), f"build did not produce a zip at {zip_path}"

    with zipfile.ZipFile(zip_path) as archive:
        names = archive.namelist()

    bytecode = [
        name
        for name in names
        if name.endswith(".pyc") or name.endswith("__pycache__") or "/__pycache__/" in name
    ]
    assert not bytecode, f"ZIP contains bytecode entries: {bytecode}"
    assert any(name.endswith("plugin.py") for name in names), "ZIP must contain the channel plugin.py payload"
    assert any(name.endswith("plugin.json") for name in names), "ZIP must contain the channel plugin.json"
