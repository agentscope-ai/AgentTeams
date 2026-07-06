"""Tests for hermes skill-pruning behavior: FileSync.pull_all and the
Worker startup _sync_skills path (Milestone 1 Step 9)."""

from __future__ import annotations

import asyncio
import logging
import subprocess
import time

import pytest

from hermes_worker import sync as sync_module
from hermes_worker.config import WorkerConfig
from hermes_worker.sync import FileSync, push_local
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


def test_push_local_bootstrap_scan_with_since_zero_finds_pre_existing_files(
    monkeypatch, tmp_path
):
    """push_local(since=0) must pick up files written before the loop started
    (e.g. bootstrap AGENTS.md/SOUL.md), whose mtime is always in the past
    relative to time.time() at push_loop startup. This guards against
    push_loop initializing last_push_time to time.time() and silently
    skipping bootstrap files forever (issue #13)."""
    fs = _make_sync(tmp_path)

    bootstrap_file = fs.local_dir / "AGENTS.md"
    bootstrap_file.write_text("bootstrap content", encoding="utf-8")

    monkeypatch.setattr(fs, "_cat", lambda _key: None)  # remote has nothing yet
    monkeypatch.setattr(sync_module, "_mc", lambda *_a, **_k: None)

    pushed = push_local(fs, since=0)

    assert "AGENTS.md" in pushed


def test_push_local_with_recent_since_skips_pre_existing_files(monkeypatch, tmp_path):
    """Sanity check: a ``since`` set to "now" (the old, buggy behavior) does
    skip files written moments earlier, demonstrating why bootstrapping the
    push loop with ``time.time()`` instead of ``0.0`` is wrong."""
    fs = _make_sync(tmp_path)

    bootstrap_file = fs.local_dir / "AGENTS.md"
    bootstrap_file.write_text("bootstrap content", encoding="utf-8")

    monkeypatch.setattr(fs, "_cat", lambda _key: None)
    monkeypatch.setattr(sync_module, "_mc", lambda *_a, **_k: None)

    since_now = time.time() + 1  # simulate the buggy "now" bootstrap value
    pushed = push_local(fs, since=since_now)

    assert "AGENTS.md" not in pushed


def test_push_local_detects_changed_binary_file(monkeypatch, tmp_path):
    """A changed binary file whose lossy-decoded text happens to collide
    with the remote's lossy-decoded text must still be detected as changed
    when compared as raw bytes (issue: binary read false-equal)."""
    fs = _make_sync(tmp_path)

    # A genuine lossy-decode collision: the local bytes (an invalid UTF-8 byte
    # 0x80) decode under errors="replace" to exactly the remote's text (which
    # already contains U+FFFD), even though the raw bytes differ. Under the old
    # read_text(errors="replace") comparison these would look EQUAL and the
    # changed file would be wrongly skipped; the byte comparison must catch it.
    remote_text = "blob-�-marker"
    local_bytes = b"blob-\x80-marker"
    assert local_bytes.decode("utf-8", "replace") == remote_text  # collision precondition

    local_file = fs.local_dir / "blob.bin"
    local_file.write_bytes(local_bytes)

    # Simulate mc cat returning the remote content as already-decoded text.
    monkeypatch.setattr(fs, "_cat", lambda _key: remote_text)

    pushed_paths = []
    monkeypatch.setattr(
        sync_module,
        "_mc",
        lambda *args, **_k: pushed_paths.append(args) or subprocess.CompletedProcess(args, 0),
    )

    pushed = push_local(fs, since=0)

    assert "blob.bin" in pushed
    assert pushed_paths, "expected mc cp to be invoked for the changed binary file"


def test_stop_cancels_sync_and_push_tasks():
    """Worker.stop() must cancel and await the sync_loop/push_loop tasks it
    started, not just the gateway task (issue #21)."""

    async def _run():
        config = WorkerConfig(
            worker_name="tt",
            minio_endpoint="minio:9000",
            minio_access_key="tt",
            minio_secret_key="secret",
        )
        worker = Worker(config)

        async def _never_ending():
            await asyncio.Event().wait()

        worker._sync_task = asyncio.create_task(_never_ending())
        worker._push_task = asyncio.create_task(_never_ending())

        await worker.stop()

        assert worker._sync_task.cancelled() or worker._sync_task.done()
        assert worker._push_task.cancelled() or worker._push_task.done()

    asyncio.run(_run())
