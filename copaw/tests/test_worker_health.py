"""Worker startup / health-related unit tests for the current Worker API.

Matrix bootstrap (relogin / join invites) lives in ``MatrixBootstrapClient``
and is covered by ``test_matrix_bootstrap.py``. Component probes live in
``test_health.py``. This module covers WorkerConfig ports and the slim
startup / run paths that remain on ``Worker``.
"""

from __future__ import annotations

import asyncio
import sys
import types

import pytest

from copaw_worker.config import WorkerConfig
from copaw_worker.worker import Worker


class _FakeWorkerAPIServer:
    instances = []

    def __init__(self, *, host, port, liveness_handler, readiness_handler):
        self.host = host
        self.port = port
        self.liveness_handler = liveness_handler
        self.readiness_handler = readiness_handler
        self.started = False
        self.stopped = False
        self.__class__.instances.append(self)

    async def start(self):
        self.started = True

    async def stop(self):
        self.stopped = True


@pytest.fixture(autouse=True)
def fake_worker_api(monkeypatch):
    _FakeWorkerAPIServer.instances = []
    monkeypatch.setattr("copaw_worker.worker.WorkerAPIServer", _FakeWorkerAPIServer)
    return _FakeWorkerAPIServer


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


def test_worker_port_defaults_to_console_port_plus_one(tmp_path):
    config = WorkerConfig(
        worker_name="alice",
        minio_endpoint="http://minio:9000",
        minio_access_key="minio",
        minio_secret_key="password",
        install_dir=tmp_path,
        console_port=18088,
    )

    assert config.worker_port == 18089


def test_worker_port_can_be_explicit(tmp_path):
    config = WorkerConfig(
        worker_name="alice",
        minio_endpoint="http://minio:9000",
        minio_access_key="minio",
        minio_secret_key="password",
        install_dir=tmp_path,
        console_port=18088,
        worker_port=19090,
    )

    assert config.worker_port == 19090


async def _finished_push_loop(*_args, **_kwargs):
    return None


async def _finished_pull_loop(*_args, **_kwargs):
    return None


@pytest.mark.anyio
async def test_worker_start_succeeds_after_mirror_and_bridge(
    tmp_path, monkeypatch, fake_worker_api
):
    push_loop_args = {}
    pull_loop_args = {}

    async def wait_forever():
        await asyncio.Event().wait()

    def capture_push_loop(*_args, **kwargs):
        push_loop_args.update(kwargs)
        return wait_forever()

    def capture_pull_loop(*_args, **kwargs):
        pull_loop_args.update(kwargs)
        return wait_forever()

    monkeypatch.setattr(Worker, "_ensure_mc", lambda _self: None)
    monkeypatch.setattr("copaw_worker.sync.FileSync.mirror_all", lambda _self: None)
    monkeypatch.setattr(
        "copaw_worker.sync.FileSync.get_config",
        lambda _self: {
            "channels": {
                "matrix": {
                    "homeserver": "http://matrix:6167",
                    "accessToken": "tok",
                }
            },
            "models": {"providers": {}},
        },
    )
    monkeypatch.setattr("copaw_worker.sync.FileSync.list_skills", lambda _self: [])
    monkeypatch.setattr(
        "copaw_worker.matrix_bootstrap.MatrixBootstrapClient.relogin",
        lambda _self, cfg: cfg,
    )
    monkeypatch.setattr(
        "copaw_worker.matrix_bootstrap.MatrixBootstrapClient.join_pending_invites",
        lambda _self, _cfg: None,
    )
    monkeypatch.setattr(
        "copaw_worker.workspace_layout.WorkspaceLayout.materialize",
        lambda *_args, **_kwargs: None,
    )
    monkeypatch.setattr(
        "copaw_worker.workspace_layout.WorkspaceLayout.sync_skills",
        lambda *_args, **_kwargs: None,
    )
    monkeypatch.setattr("copaw_worker.worker.push_loop", capture_push_loop)
    monkeypatch.setattr("copaw_worker.worker.sync_loop", capture_pull_loop)
    monkeypatch.setattr("copaw_worker.worker.install_llm_usage_hooks", lambda: None)
    monkeypatch.setattr(
        "copaw_worker.worker.configure_llm_usage_from_openclaw",
        lambda _cfg: None,
    )

    worker = Worker(_config(tmp_path))

    assert await worker.start() is True
    await worker.stop()

    assert pull_loop_args["interval"] == 60
    assert pull_loop_args["on_pull"] == worker._on_files_pulled
    assert worker._layout is not None
    assert worker._copaw_working_dir == tmp_path / "alice" / ".copaw"


@pytest.mark.anyio
async def test_worker_start_fails_when_startup_mirror_fails(tmp_path, monkeypatch):
    def fail_mirror(_self):
        raise RuntimeError("minio unavailable")

    monkeypatch.setattr(Worker, "_ensure_mc", lambda _self: None)
    monkeypatch.setattr("copaw_worker.sync.FileSync.mirror_all", fail_mirror)

    worker = Worker(_config(tmp_path))

    assert await worker.start() is False
    assert worker._layout is None


@pytest.mark.anyio
async def test_worker_start_fails_when_materialize_fails(tmp_path, monkeypatch):
    def fail_materialize(*_args, **_kwargs):
        raise ValueError("bad openclaw config")

    monkeypatch.setattr(Worker, "_ensure_mc", lambda _self: None)
    monkeypatch.setattr("copaw_worker.sync.FileSync.mirror_all", lambda _self: None)
    monkeypatch.setattr("copaw_worker.sync.FileSync.get_config", lambda _self: {})
    monkeypatch.setattr(
        "copaw_worker.matrix_bootstrap.MatrixBootstrapClient.relogin",
        lambda _self, cfg: cfg,
    )
    monkeypatch.setattr(
        "copaw_worker.matrix_bootstrap.MatrixBootstrapClient.join_pending_invites",
        lambda _self, _cfg: None,
    )
    monkeypatch.setattr(
        "copaw_worker.workspace_layout.WorkspaceLayout.materialize",
        fail_materialize,
    )

    worker = Worker(_config(tmp_path))

    assert await worker.start() is False


@pytest.mark.anyio
async def test_worker_installs_hooks_before_copaw_runner(tmp_path, monkeypatch):
    calls = []

    fake_hooks = types.ModuleType("copaw_worker.hooks")
    fake_hooks.install_tool_hooks = lambda: calls.append("hooks")
    monkeypatch.setitem(sys.modules, "copaw_worker.hooks", fake_hooks)

    config = _config(tmp_path)
    config.console_port = None
    worker = Worker(config)

    async def fake_headless():
        calls.append("headless")

    monkeypatch.setattr(worker, "_run_copaw_headless", fake_headless)
    monkeypatch.setattr(worker, "_spawn_controller_ready_loop", lambda: None)

    await worker._run_copaw()

    assert calls == ["hooks", "headless"]
