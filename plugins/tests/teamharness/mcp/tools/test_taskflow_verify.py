import json
import sys
from pathlib import Path

repo_root = Path(__file__).resolve().parents[5]
mcp_dir = repo_root / "plugins" / "teamharness" / "mcp"
sys.path.insert(0, str(mcp_dir))
import _bootstrap  # noqa: E402,F401

import mcp_common as common  # noqa: E402


def test_verify_task_artifacts_reports_missing_deliverable(tmp_path: Path) -> None:
    workspace = tmp_path / "workspace"
    task_id = "verify-task-01"
    task_dir = workspace / "shared" / "tasks" / task_id
    task_dir.mkdir(parents=True)
    (task_dir / "result.md").write_text("result body\n", encoding="utf-8")

    verification = common._verify_task_artifacts(
        {"workspaceDir": str(workspace)},
        task_id,
        result={
            "status": "SUCCESS",
            "summary": "Done.",
            "deliverables": [
                f"shared/tasks/{task_id}/result.md",
                f"shared/tasks/{task_id}/missing.md",
            ],
        },
    )

    assert verification["verified"] is False
    failed = common._failed_required_claims(verification)
    assert any("missing.md" in claim["path"] for claim in failed)


def test_verify_task_artifacts_passes_when_files_exist(tmp_path: Path) -> None:
    workspace = tmp_path / "workspace"
    task_id = "verify-task-02"
    task_dir = workspace / "shared" / "tasks" / task_id
    task_dir.mkdir(parents=True)
    (task_dir / "result.md").write_text("result body\n", encoding="utf-8")
    (task_dir / "artifact.md").write_text("artifact\n", encoding="utf-8")

    verification = common._verify_task_artifacts(
        {"workspaceDir": str(workspace)},
        task_id,
        result={
            "status": "SUCCESS",
            "summary": "Done.",
            "deliverables": [
                f"shared/tasks/{task_id}/result.md",
                f"shared/tasks/{task_id}/artifact.md",
            ],
        },
    )

    assert verification["verified"] is True
    assert common._failed_required_claims(verification) == []


def test_failed_required_claims_ignores_optional_failures() -> None:
    verification = {
        "verified": True,
        "claims": [
            {"path": "a", "required": True, "passed": True},
            {"path": "b", "required": False, "passed": False},
        ],
    }

    assert common._failed_required_claims(verification) == []


def test_check_task_reports_missing_artifact_as_ineffective(tmp_path: Path) -> None:
    workspace = tmp_path / "workspace"
    task_id = "check-task-01"
    task_dir = workspace / "shared" / "tasks" / task_id
    task_dir.mkdir(parents=True, exist_ok=True)
    (task_dir / "result.md").write_text("result body\n", encoding="utf-8")
    task = {
        "task_id": task_id,
        "project_id": "proj-01",
        "status": "submitted",
        "result_status": "SUCCESS",
        "summary": "Done.",
        "deliverables": [
            f"shared/tasks/{task_id}/result.md",
            f"shared/tasks/{task_id}/missing.md",
        ],
    }

    result = {
        "status": task["result_status"],
        "summary": task["summary"],
        "deliverables": list(task["deliverables"]),
    }
    verification = common._verify_task_artifacts(
        {"workspaceDir": str(workspace)},
        task_id,
        result=result,
    )
    failed_claims = common._failed_required_claims(verification)
    effective = (
        task["status"] == "submitted"
        and verification.get("verified") is True
    )

    assert effective is False
    assert failed_claims
    assert any("missing.md" in claim["path"] for claim in failed_claims)
