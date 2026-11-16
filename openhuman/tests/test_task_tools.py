import importlib.util
from pathlib import Path
import sys
import tomllib

import pytest

root = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('openhuman_task_tools', root / 'openhuman/scripts/configure-teamharness.py')
config = importlib.util.module_from_spec(spec)
spec.loader.exec_module(config)

@pytest.mark.parametrize('role', ['manager', 'team_leader', 'worker'])
def test_native_stdio_mapping_uses_the_projected_identity_and_role_skills(tmp_path, monkeypatch, role):
    (tmp_path / 'agent-config').mkdir()
    (tmp_path / 'agent-config/runtime.yaml').write_text('member:\n  role: '+role+'\n')
    (tmp_path / 'config.toml').write_text('[channels_config.matrix]\nroom_id="!home:test"\n')
    (tmp_path / 'workspace').mkdir()
    (tmp_path / 'workspace/SOUL.md').write_text('Your existing role.\n')
    monkeypatch.setenv('AGENTTEAMS_AGENT_ROLE', role)
    config.configure(tmp_path, root / 'plugins/teamharness')
    native = tomllib.loads((tmp_path / 'config.toml').read_text())
    server = native['mcp_client']['servers'][0]
    assert server['command'] == 'python3'
    assert server['name'] == 'teamharness'
    assert server['env']['TEAMHARNESS_SHARED_DIR'] == str(tmp_path / 'workspace/shared')
    assert Path(server['env']['TEAMHARNESS_RUNTIME_CONFIG']).is_file()
    skills = {p.name for p in (tmp_path / 'workspace/skills').iterdir()}
    assert ('project-management' in skills) == (role == 'team_leader')
    assert ('task-execution' in skills) == (role == 'worker')
    assert ('resource-management' in skills) == (role == 'manager')
    assert 'mcp_call_tool' in (tmp_path / 'workspace/SOUL.md').read_text()

def test_old_controller_without_member_contract_does_not_gain_a_broken_server(tmp_path, monkeypatch):
    monkeypatch.delenv('TEAMHARNESS_COORDINATION_URL', raising=False)
    config.configure(tmp_path, root / 'plugins/teamharness')
    assert not (tmp_path / 'config.toml').exists()
    monkeypatch.setenv('TEAMHARNESS_COORDINATION_URL', 'http://controller/api/v1/coordination')
    with pytest.raises(ValueError):
        config.configure(tmp_path, root / 'plugins/teamharness')
