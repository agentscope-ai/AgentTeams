import json
import os
from pathlib import Path
import subprocess

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[1]
WORKFLOW = yaml.safe_load((ROOT / ".github/workflows/publish-harnesses.yml").read_text())
SCRIPT = WORKFLOW["jobs"]["prepare"]["steps"][0]["run"]


@pytest.mark.parametrize("runtime,architecture,publish,success,count", [
    ("all", "all", "false", True, 12),
    ("codex", "amd64", "false", True, 1),
    ("claudecode", "all", "false", True, 2),
    ("opencode", "all", "false", True, 2),
    ("codex", "amd64", "true", False, 0),
    ("invalid", "all", "false", False, 0),
])
def test_actual_workflow_selection(tmp_path, runtime, architecture, publish, success, count):
    output = tmp_path / "output"
    result = subprocess.run(["bash", "-e", "-c", SCRIPT], capture_output=True, text=True,
        env={**os.environ, "VERSION": "v1.2.8", "RUNTIME": runtime,
             "ARCHITECTURE": architecture, "PUBLISH": publish,
             "GITHUB_SHA": "a" * 40, "GITHUB_OUTPUT": str(output)})
    assert (result.returncode == 0) == success, result.stderr
    if success:
        values = dict(line.split("=", 1) for line in output.read_text().splitlines())
        assert len(json.loads(values["runtimes"])) * len(json.loads(values["architectures"])) == count
        assert "include" not in WORKFLOW["jobs"]["harness-platform"]["strategy"]["matrix"]
