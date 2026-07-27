import ast
import json
from pathlib import Path


ROOT = Path(__file__).parents[2]
PLUGIN = ROOT / "plugins" / "agentteams-matrix-channel"


def test_matrix_plugin_uses_distinct_custom_channel_key():
    source = (PLUGIN / "agentteams_matrix" / "channel.py").read_text(encoding="utf-8")
    tree = ast.parse(source)
    assignments = {
        target.id: ast.literal_eval(statement.value)
        for statement in tree.body
        if isinstance(statement, ast.Assign)
        for target in statement.targets
        if isinstance(target, ast.Name) and isinstance(statement.value, ast.Constant)
    }
    assert assignments["CHANNEL_KEY"] == "agentteams_matrix"


def test_matrix_plugin_targets_qwenpaw_2_and_does_not_patch_builtin_matrix():
    manifest = json.loads((PLUGIN / "plugin.json").read_text(encoding="utf-8"))
    dockerfile = (ROOT / "qwenpaw" / "Dockerfile").read_text(encoding="utf-8")

    assert manifest["type"] == "channel"
    assert manifest["qwenpaw_version"] == {"min": "2.0.1", "max": "2.1.0"}
    assert "qwenpaw/app/channels/matrix/channel.py" not in dockerfile
    assert "/opt/agentteams/plugins/agentteams-matrix-channel" in dockerfile
