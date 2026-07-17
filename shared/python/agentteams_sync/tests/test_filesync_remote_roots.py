"""Tests for explicit shared/global remote root overrides (TeamHarness MCP)."""

from __future__ import annotations

from pathlib import Path

from agentteams_sync.filesync import FileSync


def test_resolve_shared_path_uses_shared_remote_root_override(tmp_path: Path) -> None:
    workspace = tmp_path / "workspace"
    sync = FileSync(
        endpoint="http://minio:9000",
        access_key="minio",
        secret_key="password",
        bucket="agentteams-storage",
        worker_name="demo",
        local_dir=workspace,
        shared_dir=workspace / "shared",
        global_shared_dir=workspace / "global-shared",
        team_resolver="agents_md",
        shared_remote_root="mock/shared/",
        global_shared_remote_root="mock/global/",
    )

    resolved = sync.resolve_shared_path("shared/projects/demo/")

    assert resolved.kind == "shared"
    assert resolved.local == workspace / "shared" / "projects" / "demo"
    assert resolved.remote == "mock/shared/projects/demo/"


def test_push_global_shared_rejects_read_only(tmp_path: Path) -> None:
    workspace = tmp_path / "workspace"
    global_dir = workspace / "global-shared" / "readme.md"
    global_dir.parent.mkdir(parents=True, exist_ok=True)
    global_dir.write_text("hello", encoding="utf-8")

    sync = FileSync(
        endpoint="http://minio:9000",
        access_key="minio",
        secret_key="password",
        bucket="agentteams-storage",
        worker_name="demo",
        local_dir=workspace,
        shared_dir=workspace / "shared",
        global_shared_dir=workspace / "global-shared",
        team_resolver="agents_md",
        shared_remote_root="mock/shared/",
        global_shared_remote_root="mock/global/",
    )

    try:
        sync.push_shared_path("global-shared/readme.md")
    except ValueError as exc:
        assert "read-only" in str(exc).lower()
    else:
        raise AssertionError("expected global-shared push to be rejected")
