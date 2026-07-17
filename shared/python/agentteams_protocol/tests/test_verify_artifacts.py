import json
from pathlib import Path

import pytest

from agentteams_protocol.task import (
    FileSystemTaskStore,
    TaskMeta,
    TaskResult,
    TaskflowError,
    VerifiableClaim,
    parse_task_result,
    verify_task_artifacts,
)


def _write_task(
    workspace: Path,
    task_id: str,
    *,
    result: TaskResult,
    verifiable_claims: list[dict] | None = None,
) -> None:
    task_dir = workspace / "shared" / "tasks" / task_id
    task_dir.mkdir(parents=True, exist_ok=True)
    meta = {
        "task_id": task_id,
        "project_id": "proj-01",
        "task_title": "Verify me",
        "assigned_to": "worker-a",
        "status": "submitted",
    }
    if verifiable_claims is not None:
        meta["verifiable_claims"] = verifiable_claims
    (task_dir / "meta.json").write_text(json.dumps(meta), encoding="utf-8")
    lines = [
        f"STATUS: {result.status}",
        f"SUMMARY: {result.summary}",
        "",
        "DELIVERABLES:",
    ]
    lines.extend(f"- {item}" for item in result.deliverables)
    (task_dir / "result.md").write_text("\n".join(lines) + "\n", encoding="utf-8")


def test_verify_task_artifacts_passes_with_nonempty_result_and_deliverables(tmp_path: Path) -> None:
    workspace = tmp_path / "workspace"
    task_id = "task-01"
    deliverable = f"shared/tasks/{task_id}/workspace/output.md"
    result = TaskResult(status="SUCCESS", summary="Done.", deliverables=[deliverable])
    _write_task(workspace, task_id, result=result)
    (workspace / deliverable).parent.mkdir(parents=True, exist_ok=True)
    (workspace / deliverable).write_text("artifact body\n", encoding="utf-8")

    report = verify_task_artifacts(workspace, task_id=task_id, result=result)

    assert report.verified is True
    assert len(report.claims) == 2
    assert all(claim.passed for claim in report.claims)


def test_verify_task_artifacts_fails_when_deliverable_missing(tmp_path: Path) -> None:
    workspace = tmp_path / "workspace"
    task_id = "task-02"
    deliverable = f"shared/tasks/{task_id}/missing.md"
    result = TaskResult(status="SUCCESS", summary="Done.", deliverables=[deliverable])
    _write_task(workspace, task_id, result=result)

    report = verify_task_artifacts(workspace, task_id=task_id, result=result)

    assert report.verified is False
    failed = [claim for claim in report.claims if not claim.passed]
    assert any(claim.path == deliverable for claim in failed)


def test_verify_task_artifacts_honors_optional_claims(tmp_path: Path) -> None:
    workspace = tmp_path / "workspace"
    task_id = "task-03"
    optional = f"shared/tasks/{task_id}/optional.md"
    result = TaskResult(status="SUCCESS", summary="Done.", deliverables=[])
    _write_task(
        workspace,
        task_id,
        result=result,
        verifiable_claims=[
            {"path": f"shared/tasks/{task_id}/result.md", "check": "nonempty", "required": True},
            {"path": optional, "check": "exists", "required": False},
        ],
    )

    report = verify_task_artifacts(workspace, task_id=task_id, result=result)

    assert report.verified is True
    optional_claim = next(claim for claim in report.claims if claim.path == optional)
    assert optional_claim.passed is False
    assert optional_claim.required is False


def test_verify_task_artifacts_via_task_store(tmp_path: Path) -> None:
    workspace = tmp_path / "workspace"
    store = FileSystemTaskStore(workspace)
    task_id = "task-04"
    result = TaskResult(status="SUCCESS", summary="Done.", deliverables=[])
    _write_task(workspace, task_id, result=result)

    report = verify_task_artifacts(store, task_id=task_id)

    assert report.verified is True


def test_verify_task_artifacts_fails_on_empty_result(tmp_path: Path) -> None:
    workspace = tmp_path / "workspace"
    task_id = "task-05"
    task_dir = workspace / "shared" / "tasks" / task_id
    task_dir.mkdir(parents=True, exist_ok=True)
    (task_dir / "result.md").write_text("", encoding="utf-8")

    with pytest.raises(TaskflowError):
        verify_task_artifacts(workspace, task_id=task_id)


def test_parse_task_result_accepts_deliverables_heading() -> None:
    text = (
        "STATUS: SUCCESS\n"
        "SUMMARY: Done.\n\n"
        "## Deliverables\n"
        "- shared/tasks/task-01/output.md\n"
        "- shared/tasks/task-01/notes.txt\n\n"
        "## Notes\n"
        "- shipped on time\n"
    )
    result = parse_task_result(text)
    assert result.deliverables == [
        "shared/tasks/task-01/output.md",
        "shared/tasks/task-01/notes.txt",
    ]
    assert result.notes == ["shipped on time"]


def test_verify_task_artifacts_resolves_task_relative_claim_path(tmp_path: Path) -> None:
    workspace = tmp_path / "workspace"
    task_id = "task-rel"
    task_dir = workspace / "shared" / "tasks" / task_id
    task_dir.mkdir(parents=True, exist_ok=True)
    (task_dir / "auth.py").write_text("print('ok')\n", encoding="utf-8")
    deliverable = f"shared/tasks/{task_id}/auth.py"
    result = TaskResult(status="SUCCESS", summary="Done.", deliverables=[deliverable])
    _write_task(
        workspace,
        task_id,
        result=result,
        verifiable_claims=[{"path": "auth.py", "check": "nonempty", "required": True}],
    )

    report = verify_task_artifacts(workspace, task_id=task_id, result=result)

    assert report.verified is True
    relative_claim = next(claim for claim in report.claims if claim.path == "auth.py")
    assert relative_claim.passed is True


def test_verify_task_artifacts_resolves_shared_prefixed_claim_path(tmp_path: Path) -> None:
    workspace = tmp_path / "workspace"
    task_id = "task-shared"
    task_dir = workspace / "shared" / "tasks" / task_id
    task_dir.mkdir(parents=True, exist_ok=True)
    artifact = task_dir / "artifact.md"
    artifact.write_text("body\n", encoding="utf-8")
    claim_path = f"shared/tasks/{task_id}/artifact.md"
    result = TaskResult(status="SUCCESS", summary="Done.", deliverables=[claim_path])
    _write_task(workspace, task_id, result=result)

    report = verify_task_artifacts(workspace, task_id=task_id, result=result)

    assert report.verified is True
    assert any(claim.path == claim_path and claim.passed for claim in report.claims)


def test_verify_task_artifacts_rejects_path_traversal(tmp_path: Path) -> None:
    workspace = tmp_path / "workspace"
    task_id = "task-traversal"
    result = TaskResult(status="SUCCESS", summary="Done.", deliverables=[])
    _write_task(
        workspace,
        task_id,
        result=result,
        verifiable_claims=[
            {"path": "../escape.md", "check": "exists", "required": True},
        ],
    )

    report = verify_task_artifacts(workspace, task_id=task_id, result=result)

    assert report.verified is False
    failed = next(claim for claim in report.claims if claim.path == "../escape.md")
    assert failed.passed is False
    assert "invalid claim path" in failed.detail


def test_verify_task_artifacts_nonempty_rejects_directory(tmp_path: Path) -> None:
    workspace = tmp_path / "workspace"
    task_id = "task-dir"
    task_dir = workspace / "shared" / "tasks" / task_id
    task_dir.mkdir(parents=True, exist_ok=True)
    (task_dir / "output").mkdir()
    claim_path = f"shared/tasks/{task_id}/output"
    result = TaskResult(status="SUCCESS", summary="Done.", deliverables=[claim_path])
    _write_task(workspace, task_id, result=result)

    report = verify_task_artifacts(workspace, task_id=task_id, result=result)

    assert report.verified is False
    failed = next(claim for claim in report.claims if claim.path == claim_path)
    assert failed.detail == "path is not a file"


def test_verify_task_artifacts_fail_closed_without_filesystem() -> None:
    result = TaskResult(
        status="SUCCESS",
        summary="Done.",
        deliverables=["shared/tasks/task-nofs/result.md"],
    )

    report = verify_task_artifacts(None, task_id="task-nofs", result=result)

    assert report.verified is False
    assert all(not claim.passed for claim in report.claims)
    assert all(claim.detail == "filesystem unavailable" for claim in report.claims)


def test_verify_task_artifacts_no_filesystem_with_only_optional_claims() -> None:
    task_id = "task-nofs-opt"
    result = TaskResult(status="SUCCESS", summary="Done.", deliverables=[])
    meta = TaskMeta(
        task_id=task_id,
        project_id="proj",
        task_title="Optional only",
        assigned_to="worker",
        verifiable_claims=[
            VerifiableClaim(
                path=f"shared/tasks/{task_id}/result.md",
                check="nonempty",
                required=False,
            ),
        ],
    )

    report = verify_task_artifacts(None, task_id=task_id, result=result, meta=meta)

    assert report.verified is True
    assert report.claims == []
