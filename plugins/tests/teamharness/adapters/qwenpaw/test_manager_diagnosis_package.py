"""The packaged Manager/Leader skill providers must expose task diagnosis."""
import importlib.util
import os
from pathlib import Path
import subprocess
import zipfile

import pytest

ROOT = Path(__file__).resolve().parents[5]


@pytest.mark.parametrize("package", ["teamharness", "agentteams-teamharness"])
def test_packaged_manager_and_leader_can_load_shared_diagnosis(tmp_path, monkeypatch, package):
    source = ROOT / "plugins" / package
    result = subprocess.run(
        ["ruby", str(source / "adapters/qwenpaw/scripts/build-qwenpaw-plugin.rb"), str(source / "plugin.yaml")],
        cwd=ROOT, env={**os.environ, "OUT_DIR": str(tmp_path)},
        text=True, capture_output=True, check=True,
    )
    archive_path = Path(result.stdout.strip().splitlines()[-1])
    with zipfile.ZipFile(archive_path) as archive:
        archive.extractall(tmp_path / "extracted")
    plugin_path = next((tmp_path / "extracted").glob("*/plugin.py"))
    spec = importlib.util.spec_from_file_location("diagnosis_package_" + package.replace("-", "_"), plugin_path)
    plugin = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(plugin)
    expected = (source / "skills/agent/task-diagnosis/SKILL.md").read_bytes()
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "manager")
    manager_root = plugin._skill_dir()
    assert (manager_root / "task-diagnosis/SKILL.md").read_bytes() == expected
    assert not (manager_root / "task-execution").exists()
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "leader")
    assert (plugin._skill_dir() / "task-diagnosis/SKILL.md").read_bytes() == expected
