"""Tests for OpenClaw sync daemon semantics (Phase 6 Y6.4)."""

from __future__ import annotations

import subprocess
import time
from pathlib import Path

import pytest

from agentteams_sync import mc as mc_ops
from agentteams_sync.openclaw import (
    PULL_MARKER_NAME,
    FileSync,
    _has_files_newer_than_marker,
    pull_fallback_openclaw,
    push_local_openclaw,
)


@pytest.fixture
def openclaw_sync(tmp_path: Path, monkeypatch) -> FileSync:
    workspace = tmp_path / "agents" / "alice"
    shared = tmp_path / "shared"
    workspace.mkdir(parents=True)
    shared.mkdir()
    monkeypatch.setenv("AGENTTEAMS_ROOT", str(tmp_path))
    sync = FileSync(
        endpoint="http://minio:9000",
        access_key="minio",
        secret_key="password",
        bucket="agentteams-storage",
        worker_name="alice",
        local_dir=workspace,
        shared_dir=shared,
    )
    monkeypatch.setattr(sync, "_ensure_alias", lambda: None)
    return sync


def test_push_skips_when_nothing_newer_than_marker(openclaw_sync: FileSync) -> None:
    marker = openclaw_sync.pull_marker
    marker.touch()
    mtime = marker.stat().st_mtime
    (openclaw_sync.local_dir / "memory").mkdir()
    old_file = openclaw_sync.local_dir / "memory" / "note.md"
    old_file.write_text("stale")
    Path(old_file).touch()
    # Force mtime at or before marker
    import os

    os.utime(old_file, (mtime - 10, mtime - 10))

    assert _has_files_newer_than_marker(openclaw_sync.local_dir, marker) is False
    assert push_local_openclaw(openclaw_sync) == []


def test_push_runs_bulk_mirror_when_file_newer(openclaw_sync: FileSync, monkeypatch) -> None:
    marker = openclaw_sync.pull_marker
    marker.touch()
    time.sleep(0.05)
    (openclaw_sync.local_dir / "memory").mkdir()
    (openclaw_sync.local_dir / "memory" / "note.md").write_text("fresh")

    commands: list[tuple] = []

    def fake_mc(*args, **_kwargs):
        commands.append(args)
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr(mc_ops, "mc", fake_mc)

    pushed = push_local_openclaw(openclaw_sync)
    assert "bulk-mirror" in pushed
    mirror_cmds = [c for c in commands if c and c[0] == "mirror"]
    assert mirror_cmds
    assert "openclaw.json" in mirror_cmds[0]
    assert "SOUL.md" in mirror_cmds[0]


def test_push_prompt_files_only_when_newer_than_marker(
    openclaw_sync: FileSync, monkeypatch
) -> None:
    marker = openclaw_sync.pull_marker
    marker.touch()
    time.sleep(0.05)
    (openclaw_sync.local_dir / "SOUL.md").write_text("evolved soul")
    (openclaw_sync.local_dir / "AGENTS.md").write_text("package agents")

    import os

    os.utime(openclaw_sync.local_dir / "AGENTS.md", (marker.stat().st_mtime - 5, marker.stat().st_mtime - 5))

    commands: list[tuple] = []

    def fake_mc(*args, **_kwargs):
        commands.append(args)
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr(mc_ops, "mc", fake_mc)

    pushed = push_local_openclaw(openclaw_sync)
    assert "SOUL.md" in pushed
    assert "AGENTS.md" not in pushed
    cp_cmds = [c for c in commands if c and c[0] == "cp"]
    assert len(cp_cmds) == 1
    assert cp_cmds[0][1].endswith("SOUL.md")


def test_pull_fallback_touches_marker_and_merges_openclaw(
    openclaw_sync: FileSync, monkeypatch
) -> None:
    local_openclaw = openclaw_sync.local_dir / "openclaw.json"
    local_openclaw.write_text('{"channels":{"matrix":{"accessToken":"local"}}}')
    marker = openclaw_sync.pull_marker
    old_marker_mtime = marker.stat().st_mtime if marker.exists() else 0

    def fake_cat(key: str):
        if key.endswith("openclaw.json"):
            return '{"models":{},"channels":{"matrix":{"accessToken":"remote"}}}'
        return None

    monkeypatch.setattr(openclaw_sync, "_cat", fake_cat)

    def fake_mc(*args, **_kwargs):
        return subprocess.CompletedProcess(args, 0, stdout="", stderr="")

    monkeypatch.setattr(mc_ops, "mc", fake_mc)

    changed = pull_fallback_openclaw(openclaw_sync)
    assert "openclaw.json" in changed
    merged = local_openclaw.read_text()
    assert "local" in merged or "remote" in merged
    assert openclaw_sync.pull_marker.stat().st_mtime >= old_marker_mtime


def test_pull_marker_name_matches_entrypoint() -> None:
    assert PULL_MARKER_NAME == ".last-pull"
