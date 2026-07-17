"""Tests for:
  - #21 supervised background tasks (sync_loop / push_loop) on Worker
  - #15 readiness matrix probe not blocking the event loop
  - active_skills: Manager-pushed skills are no longer copied into the
    legacy .copaw/active_skills/ path (copaw/AGENTS.md rule 5)
"""

from __future__ import annotations

import asyncio

import pytest

from copaw_worker.config import WorkerConfig
from copaw_worker.worker import Worker
from copaw_worker.workspace_layout import (
    WorkspaceLayout,
    install_fake_copaw_skills_modules,
)


@pytest.fixture
def anyio_backend():
    return "asyncio"


def _config(tmp_path):
    return WorkerConfig(
        worker_name="alice",
        minio_endpoint="http://minio:9000",
        minio_access_key="minio",
        minio_secret_key="password",
        install_dir=tmp_path,
    )


# ---------------------------------------------------------------------------
# #21 — background tasks are supervised (tracked + logged + cancellable)
# ---------------------------------------------------------------------------


@pytest.mark.anyio
async def test_spawn_bg_task_tracks_task_on_self(tmp_path):
    worker = Worker(_config(tmp_path))

    async def _noop():
        await asyncio.Event().wait()

    task = worker._spawn_bg_task(_noop(), name="test-task")

    assert task in worker._bg_tasks
    await worker._stop_bg_tasks()
    assert task.cancelled() or task.done()
    assert worker._bg_tasks == []


@pytest.mark.anyio
async def test_spawn_bg_task_logs_exception_instead_of_swallowing(tmp_path, caplog):
    worker = Worker(_config(tmp_path))

    async def _boom():
        raise RuntimeError("background failure")

    with caplog.at_level("ERROR", logger="copaw_worker.worker"):
        task = worker._spawn_bg_task(_boom(), name="boom-task")
        # Let the task run to completion and its done-callback fire.
        with pytest.raises(RuntimeError):
            await task

    assert any("boom-task" in rec.message for rec in caplog.records)
    assert any("background task" in rec.message for rec in caplog.records)


@pytest.mark.anyio
async def test_stop_bg_tasks_cancels_and_awaits_running_tasks(tmp_path):
    worker = Worker(_config(tmp_path))
    started = asyncio.Event()
    cancelled = asyncio.Event()

    async def _runs_forever():
        started.set()
        try:
            await asyncio.Event().wait()
        except asyncio.CancelledError:
            cancelled.set()
            raise

    worker._spawn_bg_task(_runs_forever(), name="forever-task")
    await started.wait()

    await worker._stop_bg_tasks()

    assert cancelled.is_set()
    assert worker._bg_tasks == []


@pytest.mark.anyio
async def test_stop_is_safe_without_prior_start(tmp_path):
    """stop() must not blow up if start() was never called (no bg tasks)."""
    worker = Worker(_config(tmp_path))
    await worker.stop()
    assert worker._bg_tasks == []


# NOTE: #15 (the readiness handler offloading its blocking console-socket and
# Matrix-homeserver probes via asyncio.to_thread) is covered by code review, not
# a unit test here: _readiness is an inner closure of _run_copaw_with_console and
# is not directly callable without starting the console/API server. Both probes
# are wrapped in `await asyncio.to_thread(...)` in worker.py.


# ---------------------------------------------------------------------------
# active_skills: Manager-pushed skills are not copied into active_skills/
# ---------------------------------------------------------------------------


def _install_fake_copaw_skills_modules(monkeypatch, tmp_path):
    install_fake_copaw_skills_modules(monkeypatch, tmp_path)


class _FakeSync:
    def __init__(self, local_dir):
        self.local_dir = local_dir

    def list_skills(self):
        return ["github"]


def test_sync_skills_does_not_copy_manager_pushed_skills_into_active_skills(
    tmp_path, monkeypatch
):
    _install_fake_copaw_skills_modules(monkeypatch, tmp_path)

    local_dir = tmp_path / "alice"
    layout = WorkspaceLayout(local_dir, tmp_path / "alice" / ".copaw")
    layout.copaw_working_dir.mkdir(parents=True)

    standard_skill_dir = local_dir / "skills" / "github"
    standard_skill_dir.mkdir(parents=True)
    (standard_skill_dir / "SKILL.md").write_text("Use GitHub.")

    layout.sync_skills(list_skills=lambda: ["github"], worker_name="alice")

    active_skills_dir = layout.copaw_working_dir / "active_skills"
    # Manager-pushed skill must NOT be copied into the legacy active_skills/ path.
    assert not (active_skills_dir / "github").exists()
    # Standard-space copy (already pulled by mirror_all/pull_all) is untouched.
    assert (standard_skill_dir / "SKILL.md").read_text() == "Use GitHub."


def test_sync_skills_removes_stale_manager_skill_left_in_active_skills(
    tmp_path, monkeypatch
):
    """Self-heals worker installs that still have an old copy under
    active_skills/ from before this fix."""
    _install_fake_copaw_skills_modules(monkeypatch, tmp_path)

    local_dir = tmp_path / "alice"
    layout = WorkspaceLayout(local_dir, tmp_path / "alice" / ".copaw")
    active_skills_dir = layout.copaw_working_dir / "active_skills"
    stale_dir = active_skills_dir / "github"
    stale_dir.mkdir(parents=True)
    (stale_dir / "SKILL.md").write_text("stale copy")

    layout.sync_skills(list_skills=lambda: ["github"], worker_name="alice")

    assert not stale_dir.exists()
