"""Phase 1 (B1.*) regression tests for CoPaw worker latent bug fixes."""

from __future__ import annotations

import asyncio
import json
import os
from pathlib import Path
from unittest.mock import MagicMock

import pytest

from copaw_worker.bridge import bridge_openclaw_to_copaw, propagate_prompts
from copaw_worker.config import WorkerConfig
from copaw_worker.sync import FileSync
from copaw_worker.worker import Worker
from copaw_worker.workspace_layout import WorkspaceLayout


def _openclaw_cfg() -> dict:
    return {
        "channels": {
            "matrix": {
                "enabled": True,
                "homeserver": "http://localhost:6167",
                "accessToken": "tok",
            }
        },
        "models": {
            "providers": {
                "gw": {
                    "baseUrl": "http://aigw:8080/v1",
                    "apiKey": "key123",
                    "models": [{"id": "qwen3.5-plus", "name": "qwen3.5-plus"}],
                }
            }
        },
        "agents": {"defaults": {"model": {"primary": "gw/qwen3.5-plus"}}},
    }


def test_propagate_prompts_copies_l2_files_to_workspace(tmp_path):
    local_dir = tmp_path / "alice"
    local_dir.mkdir()
    (local_dir / "SOUL.md").write_text("soul-content")
    (local_dir / "AGENTS.md").write_text("agents-content")

    copaw_dir = tmp_path / "alice" / ".copaw"
    propagate_prompts(local_dir, copaw_dir)

    workspace = copaw_dir / "workspaces" / "default"
    assert (workspace / "SOUL.md").read_text() == "soul-content"
    assert (workspace / "AGENTS.md").read_text() == "agents-content"


def test_worker_filesync_receives_worker_cr_name(tmp_path, monkeypatch):
    monkeypatch.setenv("AGENTTEAMS_WORKER_CR_NAME", "team-worker-cr")
    config = WorkerConfig(
        worker_name="display-name",
        minio_endpoint="http://minio:9000",
        minio_access_key="minio",
        minio_secret_key="password",
        install_dir=tmp_path,
    )

    worker = Worker(config)
    monkeypatch.setattr(worker, "_ensure_mc", lambda: None)
    monkeypatch.setattr(
        FileSync,
        "mirror_all",
        lambda self: None,
    )
    monkeypatch.setattr(
        FileSync,
        "get_config",
        lambda self: _openclaw_cfg(),
    )

    class _BootstrapStub:
        def relogin(self, cfg):
            return cfg

        def join_pending_invites(self, cfg):
            return None

    monkeypatch.setattr(
        "copaw_worker.worker.MatrixBootstrapClient",
        lambda sync, worker_name: _BootstrapStub(),
    )
    monkeypatch.setattr(
        WorkspaceLayout,
        "sync_skills",
        lambda self, **kwargs: None,
    )
    monkeypatch.setattr(worker, "_spawn_bg_task", lambda coro, name: MagicMock())

    asyncio.run(worker.start())

    assert worker.sync is not None
    assert worker.sync.worker_cr_name == "team-worker-cr"


def test_minio_port_does_not_set_gateway_port(tmp_path, monkeypatch):
    monkeypatch.delenv("AGENTTEAMS_PORT_GATEWAY", raising=False)
    config = WorkerConfig(
        worker_name="alice",
        minio_endpoint="http://minio:9000",
        minio_access_key="minio",
        minio_secret_key="password",
        install_dir=tmp_path,
    )

    worker = Worker(config)
    monkeypatch.setattr(worker, "_ensure_mc", lambda: None)
    monkeypatch.setattr(
        FileSync,
        "mirror_all",
        lambda self: None,
    )
    monkeypatch.setattr(
        FileSync,
        "get_config",
        lambda self: _openclaw_cfg(),
    )

    class _BootstrapStub:
        def relogin(self, cfg):
            return cfg

        def join_pending_invites(self, cfg):
            return None

    monkeypatch.setattr(
        "copaw_worker.worker.MatrixBootstrapClient",
        lambda sync, worker_name: _BootstrapStub(),
    )
    monkeypatch.setattr(
        WorkspaceLayout,
        "sync_skills",
        lambda self, **kwargs: None,
    )
    monkeypatch.setattr(worker, "_spawn_bg_task", lambda coro, name: MagicMock())

    asyncio.run(worker.start())

    assert os.environ.get("AGENTTEAMS_PORT_GATEWAY") is None


@pytest.mark.asyncio
async def test_on_files_pulled_rebridge_uses_local_prompts(tmp_path, monkeypatch):
    config = WorkerConfig(
        worker_name="alice",
        minio_endpoint="http://minio:9000",
        minio_access_key="minio",
        minio_secret_key="password",
        install_dir=tmp_path,
    )
    worker = Worker(config)
    local_dir = tmp_path / "alice"
    local_dir.mkdir(parents=True)
    (local_dir / "SOUL.md").write_text("local-soul")
    (local_dir / "AGENTS.md").write_text("local-agents")
    (local_dir / "openclaw.json").write_text(json.dumps(_openclaw_cfg()))

    copaw_dir = local_dir / ".copaw"
    copaw_dir.mkdir()
    worker._layout = WorkspaceLayout(local_dir, copaw_dir, profile="worker")
    worker.sync = FileSync(
        endpoint=config.minio_endpoint,
        access_key=config.minio_access_key,
        secret_key=config.minio_secret_key,
        bucket=config.minio_bucket,
        worker_name=config.worker_name,
        worker_cr_name=config.worker_cr_name,
        local_dir=local_dir,
    )
    monkeypatch.setattr(worker.sync, "get_config", lambda: _openclaw_cfg())

    await worker._on_files_pulled(["openclaw.json"])

    workspace = copaw_dir / "workspaces" / "default"
    assert (workspace / "SOUL.md").read_text() == "local-soul"
    assert (workspace / "AGENTS.md").read_text() == "local-agents"
    assert (workspace / "agent.json").exists()


def test_bridge_fails_when_agent_json_template_missing(tmp_path, monkeypatch):
    working_dir = tmp_path / "agent"
    original_is_file = Path.is_file

    def fake_is_file(self):
        if self.name == "agent.worker.json" and "templates" in self.parts:
            return False
        return original_is_file(self)

    monkeypatch.setattr(Path, "is_file", fake_is_file)

    with pytest.raises(RuntimeError, match="agent.worker.json not found"):
        bridge_openclaw_to_copaw(_openclaw_cfg(), working_dir, profile="worker")


def test_bridge_fails_when_agent_json_template_copy_fails(tmp_path, monkeypatch):
    working_dir = tmp_path / "agent"
    monkeypatch.setattr(
        "copaw_worker.bridge.shutil.copy2",
        lambda *_args, **_kwargs: (_ for _ in ()).throw(OSError("disk full")),
    )

    with pytest.raises(RuntimeError, match="cannot materialize agent.json"):
        bridge_openclaw_to_copaw(_openclaw_cfg(), working_dir, profile="worker")
