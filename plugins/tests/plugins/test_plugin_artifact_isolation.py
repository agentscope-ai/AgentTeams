"""A release manifest must contain only artifacts from this build."""

import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import zipfile

import pytest

ROOT = Path(__file__).resolve().parents[3]


def test_reused_output_directory_does_not_publish_unrelated_archive(tmp_path):
    if shutil.which("ruby") is None:
        pytest.skip("Ruby is required for the real plugin packager")
    stale = tmp_path / "unrelated-plugin-99.0.0.zip"
    stale.write_bytes(b"unrelated previous build")
    result = subprocess.run(
        ["ruby", str(ROOT / "plugins/scripts/package-plugin.rb"),
         str(ROOT / "plugins/agentteams-teamharness/plugin.yaml")],
        cwd=ROOT, env={**os.environ, "OUT_DIR": str(tmp_path)},
        capture_output=True, text=True, timeout=120,
    )
    assert result.returncode == 0, result.stdout + result.stderr
    manifest = json.loads((tmp_path / "release-manifest.json").read_text())
    names = [artifact["name"] for artifact in manifest["artifacts"]]
    assert stale.name not in names, "Unrelated prior archive entered release metadata"
    assert stale.name not in (tmp_path / "sha256sums.txt").read_text()
    assert "agentteams-teamharness.tar.gz" in names
    for artifact in manifest["artifacts"]:
        assert hashlib.sha256((tmp_path / artifact["name"]).read_bytes()).hexdigest() == artifact["sha256"]

    source = ROOT / "plugins/teamharness"
    bundle = ROOT / "plugins/agentteams-teamharness"
    skill = "skills/team/task-management/SKILL.md"
    adapter = "adapters/qwenpaw/plugin.py"
    for entry in ("contracts", "mcp", "prompts", "skills", adapter):
        assert (bundle / entry).is_symlink()
        assert (bundle / entry).resolve() == (source / entry).resolve()
    with tarfile.open(tmp_path / "agentteams-teamharness.tar.gz") as archive:
        for entry in (skill, adapter, "mcp/common/task_receipt.py"):
            assert archive.getmember(entry).isfile()
            assert archive.extractfile(entry).read() == (source / entry).read_bytes()
    with zipfile.ZipFile(tmp_path / "agentteams-teamharness-qwenpaw.zip") as archive:
        assert archive.read("teamharness/plugin.py") == (source / adapter).read_bytes()
        for entry in (skill, "qwenpaw-skills/task-management/SKILL.md"):
            assert archive.read("teamharness/teamharness/" + entry) == (source / skill).read_bytes()
