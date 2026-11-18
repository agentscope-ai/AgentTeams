"""Test-only native policy setup must never select an existing installation."""
import importlib.util
from pathlib import Path
import tomllib

import pytest

spec = importlib.util.spec_from_file_location("fixture_policy", Path(__file__).parent / "fixtures/openhuman_recovery_policy.py")
policy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(policy)


@pytest.mark.parametrize("commands", ['["agt", "mc"]', '[\n    "agt",\n    "mc",\n]'])
def test_only_the_bounded_step_tool_is_added_and_autonomy_is_preserved(tmp_path, commands):
    path = tmp_path / "config.toml"
    path.write_text(f'[autonomy]\nallowed_commands = {commands}\nworkspace_only = true\nlevel = "supervised"\nrequire_approval_for_medium_risk = true\n\n[mcp_client]\nenabled = true\n')
    for _ in range(2):
        policy.configure(path, "recovery-1631-live-20261117-v18", "http://recovery-1631-live-20261117-v18-controller:8090")
        result = tomllib.loads(path.read_text())
        assert result["autonomy"] == {"allowed_commands": ["agt", "mc", "agentteams-recovery-step"], "workspace_only": True,
                                      "level": "supervised", "require_approval_for_medium_risk": True}
        assert result["mcp_client"] == {"enabled": True}


@pytest.mark.parametrize("prefix,controller", [
    ("", "http://existing-controller:8090"),
    ("agentteams", "http://agentteams-controller:8090"),
    ("recovery-1631-live-20261117-v18", "http://recovery-1631-live-20261117-v17-controller:8090"),
])
def test_other_installations_are_rejected_without_changing_files(tmp_path, prefix, controller):
    path = tmp_path / "config.toml"
    original = '[autonomy]\nallowed_commands = ["agt"]\n'
    path.write_text(original)
    with pytest.raises(ValueError, match="explicitly named"):
        policy.configure(path, prefix, controller)
    assert path.read_text() == original
