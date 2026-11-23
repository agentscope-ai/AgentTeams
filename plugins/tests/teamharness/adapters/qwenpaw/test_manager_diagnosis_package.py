"""The packaged Manager/Leader skill providers must expose task diagnosis."""
import importlib.util
import os
import shutil
from pathlib import Path
import subprocess
import zipfile

import pytest

ROOT = Path(__file__).resolve().parents[5]


def test_packaged_manager_and_leader_can_load_shared_diagnosis(tmp_path, monkeypatch):
    source = ROOT / "plugins/teamharness"
    result = subprocess.run(
        ["ruby", str(ROOT / "qwenpaw/adapters/teamharness/scripts/build-qwenpaw-plugin.rb"), str(source / "plugin.yaml")],
        cwd=ROOT, env={**os.environ, "OUT_DIR": str(tmp_path)},
        text=True, capture_output=True, check=True,
    )
    archive_path = Path(result.stdout.strip().splitlines()[-1])
    with zipfile.ZipFile(archive_path) as archive:
        archive.extractall(tmp_path / "extracted")
    plugin_path = next((tmp_path / "extracted").glob("*/plugin.py"))
    spec = importlib.util.spec_from_file_location("diagnosis_package", plugin_path)
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
    leader = (source / "prompts/agent/leader.md").read_text().strip()
    assert leader in plugin._team_prompt(None)
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "team_leader")
    assert leader in plugin._team_prompt(None)
    monkeypatch.setenv("AGENTTEAMS_AGENT_ROLE", "worker")
    assert (source / "prompts/agent/worker.md").read_text().strip() in plugin._team_prompt(None)


@pytest.mark.parametrize("target", ["build-agentteams-controller", "push-agentteams-controller"])
def test_controller_image_context_contains_manager_diagnosis(tmp_path, target):
    # Execute the real Makefile staging recipes; intercept only Docker so no
    # image is built or published. Inspect the context at the build boundary.
    shutil.copy(ROOT / "Makefile", tmp_path / "Makefile")
    shutil.copytree(ROOT / "manager/agent", tmp_path / "manager/agent")
    shutil.copytree(ROOT / "plugins/teamharness", tmp_path / "plugins/teamharness", symlinks=True)
    shutil.copytree(ROOT / "plugins/agentteams-matrix", tmp_path / "plugins/agentteams-matrix", symlinks=True)
    compatibility = tmp_path / "plugins/agentteams-teamharness"
    compatibility.mkdir()
    (compatibility / "plugin.yaml").write_text("compatibility plugin")
    (tmp_path / "plugins/tests").mkdir()
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
    leader = Path('agentteams-controller/agent/team-leader-agent')
    public = Path('plugins/teamharness')
    assert Path('agentteams-controller/plugins/teamharness/plugin.yaml').is_file()
    for bundle in ['teamharness', 'agentteams-matrix']:
        assert not (Path('agentteams-controller/plugins') / bundle / 'adapters/qwenpaw').exists()
    assert not Path('agentteams-controller/plugins/agentteams-matrix-channel').exists()
    assert not Path('agentteams-controller/plugins/agentteams-teamharness').exists()
    assert not Path('agentteams-controller/plugins/tests').exists()
    for template in ['worker-agent', 'hermes-worker-agent', 'openhuman-worker-agent', 'copaw-worker-agent']:
        skills = Path('agentteams-controller/agent') / template / 'skills'
        for retired in ['task-progress', 'project-participation', 'task-management']:
            assert not (skills / retired).exists()
    assert (leader / 'AGENTS.md').read_bytes() == (public / 'prompts/agent/leader.md').read_bytes()
    for group, names in {
        'team': ['communication', 'file-sharing', 'roomflow', 'team-coordination',
                 'project-management', 'task-delegation', 'task-management', 'organization'],
        'agent': ['task-diagnosis', 'mcporter', 'find-skills'],
    }.items():
        for name in names:
            assert (leader / 'skills' / name / 'SKILL.md').read_bytes() == (public / 'skills' / group / name / 'SKILL.md').read_bytes()
    assert b'External work starts as a Project.' not in (leader / 'AGENTS.md').read_bytes()
    for template in ['worker-agent', 'hermes-worker-agent', 'copaw-worker-agent']:
        worker = Path('agentteams-controller/agent') / template
        assert (worker / 'AGENTS.md').read_bytes() == (public / 'prompts/agent/worker.md').read_bytes()
        assert (worker / 'skills/task-execution/SKILL.md').read_bytes() == (public / 'skills/team/task-execution/SKILL.md').read_bytes()
    Path('context-verified').write_text('ok')
""")
    docker.chmod(0o755)
    subprocess.run(["make", target, "PUSH_LATEST=", "MULTIARCH_PLATFORMS=linux/arm64"], cwd=tmp_path,
                   env={**os.environ, "PATH": str(binary) + os.pathsep + os.environ["PATH"]},
                   capture_output=True, text=True, check=True)
    assert (tmp_path / "context-verified").read_text() == "ok"


def test_embedded_uses_the_assembled_controller_leader_assets():
    dockerfile = (ROOT / "agentteams-controller/Dockerfile.embedded").read_text()
    assert "COPY --from=agentteams-controller /opt/agentteams/plugins/ /opt/agentteams/plugins/" in dockerfile
    assert "COPY plugins/ /opt/agentteams/plugins/" not in dockerfile
    for template in ["team-leader-agent", "worker-agent", "hermes-worker-agent", "copaw-worker-agent"]:
        assert f"COPY --from=agentteams-controller /opt/agentteams/agent/{template}/ /opt/agentteams/agent/{template}/" in dockerfile
        assert f"COPY manager/agent/{template}/" not in dockerfile


def test_retired_leader_business_copies_are_not_maintained():
    legacy = ROOT / "manager/agent/team-leader-agent"
    assert not (legacy / "AGENTS.md").exists()
    assert not (legacy / "skills").exists()
