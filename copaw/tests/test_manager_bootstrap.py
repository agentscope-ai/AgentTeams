"""Tests for CoPaw Manager bootstrap (Phase 9 G9.6)."""

from __future__ import annotations

import json
import sys
from unittest.mock import patch

import pytest

from copaw_worker.bridge import reset_bootstrap_state
from copaw_worker.manager_bootstrap import (
    configure_manager_dm_rooms,
    load_openclaw_json,
    materialize_manager_workspace,
    maybe_archive_legacy_manager_config,
    resolve_matrix_api_url,
)


@pytest.fixture(autouse=True)
def _reset_bootstrap():
    reset_bootstrap_state()
    yield
    reset_bootstrap_state()


def _openclaw_cfg() -> dict:
    return {
        "channels": {
            "matrix": {
                "enabled": True,
                "homeserver": "http://127.0.0.1:6167",
                "accessToken": "test-token",
            }
        },
        "models": {
            "providers": {
                "gw": {
                    "baseUrl": "http://aigw:8080/v1",
                    "apiKey": "key",
                    "models": [{"id": "qwen3.5-plus", "name": "qwen3.5-plus"}],
                }
            }
        },
        "agents": {"defaults": {"model": {"primary": "gw/qwen3.5-plus"}}},
    }


def test_maybe_archive_legacy_manager_config(tmp_path):
    copaw_dir = tmp_path / ".copaw"
    copaw_dir.mkdir()
    config = {
        "security": {"tool_guard": {"enabled": True}},
    }
    (copaw_dir / "config.json").write_text(json.dumps(config))

    maybe_archive_legacy_manager_config(copaw_dir)

    assert not (copaw_dir / "config.json").exists()
    assert (copaw_dir / ".config-migrated-v2").is_file()
    archives = list(copaw_dir.glob("config.json.legacy-*"))
    assert len(archives) == 1


def test_materialize_manager_uses_skills_symlink_not_active_skills(tmp_path):
    standard = tmp_path / "workspace"
    standard.mkdir()
    (standard / "SOUL.md").write_text("soul")
    skills = standard / "skills"
    skills.mkdir()
    (skills / "demo" / "SKILL.md").parent.mkdir(parents=True)
    (skills / "demo" / "SKILL.md").write_text("skill")

    copaw_dir = standard / ".copaw"
    materialize_manager_workspace(standard, copaw_dir, _openclaw_cfg())

    workspace = copaw_dir / "workspaces" / "default"
    skills_link = workspace / "skills"
    assert not (workspace / "active_skills").exists()
    if sys.platform == "win32":
        # Symlinks may require elevated privileges on Windows; layout falls back
        # to standard-space skills without copying to active_skills/.
        assert (standard / "skills" / "demo" / "SKILL.md").is_file()
    else:
        assert skills_link.is_symlink()
        assert skills_link.resolve() == skills.resolve()


def test_resolve_matrix_api_url_prefers_env(monkeypatch, tmp_path):
    monkeypatch.setenv("AGENTTEAMS_MATRIX_URL", "http://matrix.k8s.svc:6167")
    agent_data = {"channels": {"matrix": {"homeserver": "http://ignored:6167"}}}
    assert (
        resolve_matrix_api_url(agent_data, copaw_working_dir=tmp_path)
        == "http://matrix.k8s.svc:6167"
    )


def test_resolve_matrix_api_url_falls_back_to_agent_json(tmp_path, monkeypatch):
    monkeypatch.delenv("AGENTTEAMS_MATRIX_URL", raising=False)
    agent_data = {"channels": {"matrix": {"homeserver": "http://bridged:6167"}}}
    assert (
        resolve_matrix_api_url(agent_data, copaw_working_dir=tmp_path)
        == "http://bridged:6167"
    )


def test_resolve_matrix_api_url_falls_back_to_config_json(tmp_path, monkeypatch):
    monkeypatch.delenv("AGENTTEAMS_MATRIX_URL", raising=False)
    copaw_dir = tmp_path / ".copaw"
    copaw_dir.mkdir()
    (copaw_dir / "config.json").write_text(
        json.dumps({"channels": {"matrix": {"homeserver": "http://from-config:6167"}}})
    )
    assert (
        resolve_matrix_api_url(None, copaw_working_dir=copaw_dir)
        == "http://from-config:6167"
    )


def test_configure_manager_dm_rooms_uses_env_matrix_url(tmp_path, monkeypatch):
    monkeypatch.setenv("AGENTTEAMS_MATRIX_URL", "http://k8s-matrix:6167")
    copaw_dir = tmp_path / ".copaw"
    workspace = copaw_dir / "workspaces" / "default"
    workspace.mkdir(parents=True)
    agent_path = workspace / "agent.json"
    agent_path.write_text(
        json.dumps(
            {
                "channels": {
                    "matrix": {
                        "access_token": "tok",
                        "homeserver": "http://127.0.0.1:6167",
                        "groups": {},
                    }
                }
            }
        )
    )

    seen_api: list[str] = []

    def fake_joined(api, _token):
        seen_api.append(api)
        return ["!dm:domain"]

    def fake_count(_api, _token, room_id):
        return 2 if room_id == "!dm:domain" else 5

    with patch(
        "copaw_worker.manager_bootstrap._fetch_joined_rooms",
        side_effect=fake_joined,
    ), patch(
        "copaw_worker.manager_bootstrap._fetch_member_count",
        side_effect=fake_count,
    ):
        configure_manager_dm_rooms(copaw_dir, max_retries=1)

    assert seen_api == ["http://k8s-matrix:6167"]
    data = json.loads(agent_path.read_text())
    assert data["channels"]["matrix"]["groups"]["!dm:domain"]["autoReply"] is True


def test_configure_manager_dm_rooms_uses_env_token_fallback(tmp_path, monkeypatch):
    monkeypatch.setenv("AGENTTEAMS_MANAGER_MATRIX_TOKEN", "env-tok")
    copaw_dir = tmp_path / ".copaw"
    workspace = copaw_dir / "workspaces" / "default"
    workspace.mkdir(parents=True)
    agent_path = workspace / "agent.json"
    agent_path.write_text(
        json.dumps({"channels": {"matrix": {"groups": {}}}})
    )

    seen_token: list[str] = []

    def fake_joined(_api, token):
        seen_token.append(token)
        return []

    with patch(
        "copaw_worker.manager_bootstrap._fetch_joined_rooms",
        side_effect=fake_joined,
    ):
        configure_manager_dm_rooms(copaw_dir, matrix_api="http://matrix:8080", max_retries=1)

    assert seen_token == ["env-tok"]


def test_configure_manager_dm_rooms_merges_two_member_rooms(tmp_path):
    copaw_dir = tmp_path / ".copaw"
    workspace = copaw_dir / "workspaces" / "default"
    workspace.mkdir(parents=True)
    agent_path = workspace / "agent.json"
    agent_path.write_text(
        json.dumps(
            {
                "channels": {
                    "matrix": {
                        "access_token": "tok",
                        "groups": {"!existing:domain": {"requireMention": True}},
                    }
                }
            }
        )
    )

    def fake_joined(_api, _token):
        return ["!dm:domain", "!group:domain"]

    def fake_count(_api, _token, room_id):
        return 2 if room_id == "!dm:domain" else 5

    with patch(
        "copaw_worker.manager_bootstrap._fetch_joined_rooms",
        side_effect=fake_joined,
    ), patch(
        "copaw_worker.manager_bootstrap._fetch_member_count",
        side_effect=fake_count,
    ):
        configure_manager_dm_rooms(copaw_dir, max_retries=1)

    data = json.loads(agent_path.read_text())
    groups = data["channels"]["matrix"]["groups"]
    assert groups["!existing:domain"]["requireMention"] is True
    assert groups["!dm:domain"] == {"requireMention": False, "autoReply": True}
    assert "!group:domain" not in groups


def test_load_openclaw_json(tmp_path):
    path = tmp_path / "openclaw.json"
    path.write_text(json.dumps({"agents": {}}))
    assert load_openclaw_json(path) == {"agents": {}}
