"""A task can submit its existing report without rewriting a second copy."""
from pathlib import Path
import sys

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[3] / "teamharness/mcp"))
import server
from mcp.common.paths import _task_result_path


def test_default_and_named_report_use_the_same_task_directory(tmp_path):
    args = {"workspaceDir": str(tmp_path)}
    assert _task_result_path(args, "qa") == ("shared/tasks/qa/result.md", tmp_path / "shared/tasks/qa/result.md")
    assert _task_result_path(args, "qa", "shared/tasks/qa/qa-report.md")[1] == tmp_path / "shared/tasks/qa/qa-report.md"


@pytest.mark.parametrize("path", [
    "shared/tasks/other/result.md", "shared/tasks/qa/../other/result.md",
    "shared/tasks/qa/meta.json", "shared/tasks/qa/spec.md",
    "shared/tasks/qa/inputs/result.md", "shared/tasks/qa/base/result.md",
    "shared/tasks/qa/", "shared/tasks/qa/reports//result.md", 42, "",
    "shared/tasks/qa/result.md\n", "shared/tasks/qa/re\nsult.md",
])
def test_report_cannot_replace_protocol_or_upstream_files(tmp_path, path):
    with pytest.raises(ValueError):
        _task_result_path({"workspaceDir": str(tmp_path)}, "qa", path)


def test_report_cannot_follow_a_link_into_another_task(tmp_path):
    own = tmp_path / "shared/tasks/qa"; own.mkdir(parents=True)
    peer = tmp_path / "shared/tasks/dev"; peer.mkdir()
    (own / "reports").symlink_to(peer, target_is_directory=True)
    with pytest.raises(ValueError):
        _task_result_path({"workspaceDir": str(tmp_path)}, "qa", "shared/tasks/qa/reports/result.md")


def test_completion_uses_the_submitted_report_not_an_old_default_copy(tmp_path, monkeypatch):
    from mcp.project import flow
    from mcp.project.flow import _completion_artifact_registered
    args = {"workspaceDir": str(tmp_path)}
    directory = tmp_path / "shared/tasks/qa"; directory.mkdir(parents=True)
    (directory / "result.md").write_text("Stale draft")
    monkeypatch.setattr(flow, "_load_authoritative_task_optional", lambda *args:
        {"task_id":"qa", "result_path":"shared/tasks/qa/qa-report.md"})
    assert not _completion_artifact_registered(args, "qa")
    (directory / "qa-report.md").write_text("Actual reviewed evidence")
    assert _completion_artifact_registered(args, "qa")
