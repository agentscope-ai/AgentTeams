from pathlib import Path

import yaml


ROOT = Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / ".github" / "workflows" / "test-integration.yml"


def test_integration_workflow_runs_qwenpaw_like_copaw() -> None:
    workflow = yaml.load(
        WORKFLOW.read_text(encoding="utf-8"),
        Loader=yaml.BaseLoader,
    )

    pull_request_paths = workflow["on"]["pull_request"]["paths"]
    push_paths = workflow["on"]["push"]["paths"]
    assert "qwenpaw/**" in pull_request_paths
    assert "qwenpaw/**" in push_paths

    build_targets = workflow["jobs"]["build-images"]["strategy"]["matrix"]["target"]
    assert "qwenpaw-worker" in build_targets

    matrix_step = next(
        step
        for step in workflow["jobs"]["detect-changes"]["steps"]
        if step.get("id") == "test-matrix"
    )
    matrix_script = matrix_step["run"]
    assert matrix_script.count('"worker_runtime":"qwenpaw"') == 3
    assert '"filter_env":"SHARD_B_TESTS"' not in matrix_script
    assert "SHARD_B_TESTS" not in workflow["env"]
    assert "14" not in workflow["env"]["NON_GITHUB_TESTS"].split()
    assert (
        '"shard":"qwenpaw-teamharness","filter_env":"SHARD_QWENPAW_TESTS",'
        '"manager_runtime":"copaw","worker_runtime":"qwenpaw",'
        '"requires_secret":true'
    ) in matrix_script
    assert workflow["jobs"]["integration-tests"]["strategy"]["matrix"] == (
        "${{ fromJSON(needs.detect-changes.outputs.test_matrix) }}"
    )
