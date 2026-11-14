"""The packaged Manager/Leader skill providers must expose task diagnosis."""
import importlib.util
import os
import shutil
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
    assert b"agt monitoring" in expected and b"agt logs" in expected
    assert not (manager_root / "task-execution").exists()
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "leader")
    assert (plugin._skill_dir() / "task-diagnosis/SKILL.md").read_bytes() == expected


@pytest.mark.parametrize("target", ["build-agentteams-controller", "push-agentteams-controller"])
def test_controller_image_context_contains_manager_diagnosis(tmp_path, target):
    # Execute the real Makefile staging recipes; intercept only Docker so no
    # image is built or published. Inspect the context at the build boundary.
    shutil.copy(ROOT / "Makefile", tmp_path / "Makefile")
    shutil.copytree(ROOT / "manager/agent", tmp_path / "manager/agent")
    shutil.copytree(ROOT / "plugins/teamharness", tmp_path / "plugins/teamharness")
    (tmp_path / "agentteams-controller").mkdir()
    (tmp_path / "hack").mkdir()
    shutil.copy(ROOT / "hack/versions.env", tmp_path / "hack/versions.env")
    binary = tmp_path / "bin"
    binary.mkdir()
    docker = binary / "docker"
    docker.write_text("""#!/usr/bin/env python3
from pathlib import Path
import sys
if sys.argv[1:2] == ['build'] or sys.argv[1:3] == ['buildx', 'build']:
    root = Path('agentteams-controller/agent/manager-agent/skills')
    skill = (root / 'task-diagnosis/SKILL.md').read_bytes()
    assert skill == Path('plugins/teamharness/skills/agent/task-diagnosis/SKILL.md').read_bytes()
    assert b'agt monitoring' in skill and b'agt logs' in skill
    assert (root / 'resource-management/SKILL.md').is_file()
    Path('context-verified').write_text('ok')
""")
    docker.chmod(0o755)
    subprocess.run(["make", target, "PUSH_LATEST=", "MULTIARCH_PLATFORMS=linux/arm64"], cwd=tmp_path,
                   env={**os.environ, "PATH": str(binary) + os.pathsep + os.environ["PATH"]},
                   capture_output=True, text=True, check=True)
    assert (tmp_path / "context-verified").read_text() == "ok"
