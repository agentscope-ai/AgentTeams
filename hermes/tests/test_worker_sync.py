"""Tests for hermes skill-pruning behavior: FileSync.pull_all and the
Worker startup _sync_skills path (Milestone 1 Step 9)."""

from __future__ import annotations

import logging
import subprocess

from hermes_worker import sync as sync_module
from hermes_worker.config import WorkerConfig
from hermes_worker.sync import FileSync
from hermes_worker.worker import Worker


def _make_sync(tmp_path):
    return FileSync(
        endpoint="minio:9000",
        access_key="tt",
        secret_key="secret",
        bucket="hiclaw",
        worker_name="tt",
        local_dir=tmp_path / "worker",
    )


def test_pull_all_skips_local_skill_absent_from_minio(monkeypatch, tmp_path, caplog):
    """A self-installed skill dir (present locally, absent in MinIO) must
    survive pull_all instead of being rmtree'd, and the skip should be
    logged exactly once even across repeated pull_all calls."""
    fs = _make_sync(tmp_path)

    local_only_skill = fs.local_dir / "skills" / "self-installed"
    local_only_skill.mkdir(parents=True, exist_ok=True)
    marker = local_only_skill / "SKILL.md"
    marker.write_text("hello", encoding="utf-8")

    monkeypatch.setattr(fs, "_cat", lambda _key: None)
    monkeypatch.setattr(fs, "_ls", lambda _prefix: [])
    monkeypatch.setattr(
        sync_module,
        "_mc",
        lambda *_args, **_kwargs: subprocess.CompletedProcess(_args, 0, stdout="", stderr=""),
    )
    caplog.set_level(logging.INFO)

    changed = fs.pull_all()

    assert not any("self-installed" in c and "removed" in c for c in changed)
    assert local_only_skill.is_dir()
    assert marker.read_text(encoding="utf-8") == "hello"
    assert caplog.text.count("self-installed") == 1

    # A second pull_all should not re-log or prune.
    caplog.clear()
    fs.pull_all()
    assert local_only_skill.is_dir()
    assert "self-installed" not in caplog.text


class _FakeSync:
    """Minimal stand-in for FileSync, only exposing what _sync_skills needs."""

    def __init__(self, local_dir):
        self.local_dir = local_dir

    def list_skills(self) -> list[str]:
        return []


def _make_worker(tmp_path):
    config = WorkerConfig(
        worker_name="tt",
        minio_endpoint="minio:9000",
        minio_access_key="tt",
        minio_secret_key="secret",
        install_dir=tmp_path / "agents",
    )
    worker = Worker(config)
    worker.sync = _FakeSync(local_dir=tmp_path / "agents" / "tt")
    return worker


def test_sync_skills_skips_local_skill_absent_from_minio(tmp_path, caplog):
    """A self-installed skill dir (present locally, absent in MinIO) must
    survive Hermes startup _sync_skills instead of being rmtree'd, and the
    skip should be logged exactly once even across repeated calls."""
    worker = _make_worker(tmp_path)

    local_only_skill = worker._hermes_home / "skills" / "self-installed"
    local_only_skill.mkdir(parents=True, exist_ok=True)
    marker = local_only_skill / "SKILL.md"
    marker.write_text("hello", encoding="utf-8")

    caplog.set_level(logging.INFO)

    worker._sync_skills()

    assert local_only_skill.is_dir()
    assert marker.read_text(encoding="utf-8") == "hello"
    assert caplog.text.count("self-installed") == 1

    # A second call must not re-log or prune.
    caplog.clear()
    worker._sync_skills()
    assert local_only_skill.is_dir()
    assert "self-installed" not in caplog.text
