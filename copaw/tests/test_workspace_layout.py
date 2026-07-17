"""Phase 8 workspace layout and bridge split tests."""

from __future__ import annotations

import json
import os
import tempfile
from pathlib import Path
from unittest.mock import patch

import pytest

from copaw_worker.bridge import (
    bootstrap_copaw_runtime,
    bridge_config,
    bridge_openclaw_to_copaw,
    reset_bootstrap_state,
)
from copaw_worker.workspace_layout import WorkspaceLayout, install_fake_copaw_skills_modules


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


@pytest.fixture(autouse=True)
def _reset_bootstrap():
    reset_bootstrap_state()
    yield
    reset_bootstrap_state()


def test_bridge_config_writes_json_without_env_side_effects(tmp_path, monkeypatch):
    monkeypatch.delenv("COPAW_WORKING_DIR", raising=False)
    working_dir = tmp_path / "agent"

    bridge_config(_openclaw_cfg(), working_dir, profile="worker")

    assert (working_dir / "config.json").exists()
    assert (working_dir / "workspaces" / "default" / "agent.json").exists()
    assert (working_dir / "providers.json").exists()
    assert os.environ.get("COPAW_WORKING_DIR") is None


def test_bootstrap_copaw_runtime_is_idempotent(tmp_path):
    working_dir = tmp_path / "agent"
    bridge_config(_openclaw_cfg(), working_dir, profile="worker")

    with patch("copaw_worker.bridge._patch_copaw_paths") as patch_paths:
        bootstrap_copaw_runtime(working_dir)
        bootstrap_copaw_runtime(working_dir)
        patch_paths.assert_called_once()


def test_rebridge_uses_bridge_config_only(tmp_path):
    working_dir = tmp_path / "agent"
    local_dir = tmp_path / "standard"
    local_dir.mkdir()
    (local_dir / "SOUL.md").write_text("soul")

    layout = WorkspaceLayout(local_dir, working_dir, profile="worker")
    layout.materialize(_openclaw_cfg(), bootstrap=True)

    with patch("copaw_worker.workspace_layout.bootstrap_copaw_runtime") as bootstrap:
        layout.rebridge(_openclaw_cfg())
        bootstrap.assert_not_called()


def test_materialize_creates_skills_symlink(tmp_path):
    local_dir = tmp_path / "alice"
    local_dir.mkdir()
    (local_dir / "skills" / "github").mkdir(parents=True)
    (local_dir / "skills" / "github" / "SKILL.md").write_text("github skill")

    layout = WorkspaceLayout(local_dir, local_dir / ".copaw", profile="worker")
    layout.materialize(_openclaw_cfg(), bootstrap=False)
    link = layout.ensure_skills_symlink()

    if link.is_symlink():
        assert (link / "github" / "SKILL.md").read_text() == "github skill"
    else:
        # Windows dev hosts may lack symlink privilege; skills still on disk.
        assert (local_dir / "skills" / "github" / "SKILL.md").exists()


def test_persist_edits_copies_runtime_prompts(tmp_path):
    local_dir = tmp_path / "standard"
    workspace = local_dir / ".copaw" / "workspaces" / "default"
    workspace.mkdir(parents=True)
    outer = local_dir / "AGENTS.md"
    inner = workspace / "AGENTS.md"
    outer.write_text("outer")
    inner.write_text("inner-newer")
    os.utime(outer, (1, 1))
    os.utime(inner, (2, 2))

    layout = WorkspaceLayout(local_dir, local_dir / ".copaw")
    layout.persist_edits()

    assert outer.read_text() == "inner-newer"


def test_layout_sync_skills_does_not_copy_manager_skills(tmp_path, monkeypatch):
    install_fake_copaw_skills_modules(monkeypatch, tmp_path)

    local_dir = tmp_path / "alice"
    copaw_dir = local_dir / ".copaw"
    standard_skill = local_dir / "skills" / "github"
    standard_skill.mkdir(parents=True)
    (standard_skill / "SKILL.md").write_text("Use GitHub.")

    layout = WorkspaceLayout(local_dir, copaw_dir)
    layout.sync_skills(list_skills=lambda: ["github"], worker_name="alice")

    assert not (copaw_dir / "active_skills" / "github").exists()


def test_bridge_openclaw_to_copaw_still_bootstraps(tmp_path, monkeypatch):
    monkeypatch.delenv("COPAW_WORKING_DIR", raising=False)
    working_dir = tmp_path / "agent"

    with patch("copaw_worker.bridge.bootstrap_copaw_runtime") as bootstrap:
        bridge_openclaw_to_copaw(_openclaw_cfg(), working_dir, profile="worker")
        bootstrap.assert_called_once_with(working_dir)


def test_materialize_startup_sequence(tmp_path):
    """Golden startup: prompts + agent.json + config.json after materialize."""
    local_dir = tmp_path / "alice"
    local_dir.mkdir()
    (local_dir / "SOUL.md").write_text("worker-soul")
    (local_dir / "AGENTS.md").write_text("worker-agents")

    layout = WorkspaceLayout(local_dir, local_dir / ".copaw", profile="worker")
    layout.materialize(_openclaw_cfg(), bootstrap=False)

    workspace = local_dir / ".copaw" / "workspaces" / "default"
    assert (workspace / "SOUL.md").read_text() == "worker-soul"
    agent = json.loads((workspace / "agent.json").read_text())
    assert agent["channels"]["matrix"]["access_token"] == "tok"

    with tempfile.TemporaryDirectory() as other:
        other_dir = Path(other) / "agent"
        bridge_config(_openclaw_cfg(), other_dir, profile="worker")
        config = json.loads((other_dir / "config.json").read_text())
        assert config["channels"]["matrix"]["access_token"] == "tok"
