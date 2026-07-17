from __future__ import annotations

import json
from pathlib import Path

from openhuman_worker.bridge import (
    bridge_openclaw_file,
    bridge_openclaw_to_openhuman,
    render_config_toml,
    write_config_toml,
)


SAMPLE_OPENCLAW = {
    "channels": {
        "matrix": {
            "homeserver": "http://matrix:8080",
            "accessToken": "syt_worker_token",
            "userId": "@alice:matrix.local",
            "dm": {"allowFrom": ["@admin:matrix.local", "@manager:matrix.local"]},
            "groupAllowFrom": ["@manager:matrix.local", "@bob:matrix.local"],
        }
    },
    "models": {
        "providers": {
            "hiclaw-gateway": {
                "baseUrl": "http://gateway:8080/v1",
                "apiKey": "gw-key-123",
                "models": [{"id": "qwen-plus"}],
            }
        }
    },
    "agents": {
        "defaults": {
            "model": {"primary": "hiclaw-gateway/qwen-plus"},
        }
    },
}


def test_bridge_merges_allowlists_and_strips_model_prefix() -> None:
    result = bridge_openclaw_to_openhuman(
        SAMPLE_OPENCLAW,
        env={
            "MATRIX_HOME_ROOM_ID": "!room:matrix.local",
            "MATRIX_DEVICE_ID": "DEVICE123",
            "AGENTTEAMS_PORT_GATEWAY": "8080",
        },
    )

    assert result.llm is not None
    assert result.llm.base_url == "http://gateway:8080/v1"
    assert result.llm.api_key == "gw-key-123"
    assert result.llm.default_model == "qwen-plus"

    assert 'homeserver = "http://matrix:8080"' in result.config_toml
    assert 'access_token = "syt_worker_token"' in result.config_toml
    assert 'room_id = "!room:matrix.local"' in result.config_toml
    assert 'user_id = "@alice:matrix.local"' in result.config_toml
    assert 'device_id = "DEVICE123"' in result.config_toml
    assert "@admin:matrix.local" in result.config_toml
    assert "@bob:matrix.local" in result.config_toml
    assert result.config_toml.count("@manager:matrix.local") == 1


def test_bridge_env_fallback_when_openclaw_missing() -> None:
    result = bridge_openclaw_to_openhuman(
        {},
        env={
            "MATRIX_HOMESERVER_URL": "http://homeserver:8080",
            "MATRIX_ACCESS_TOKEN": "env-token",
            "MATRIX_HOME_ROOM_ID": "!env-room:matrix.local",
            "MATRIX_USER_ID": "@env-user:matrix.local",
            "MATRIX_ALLOWED_USERS": "@one:matrix.local,@two:matrix.local",
            "AGENTTEAMS_AI_GATEWAY_URL": "http://aigw:8080",
            "AGENTTEAMS_WORKER_GATEWAY_KEY": "env-gw-key",
            "AGENTTEAMS_DEFAULT_MODEL": "qwen3.5-plus",
        },
    )

    assert result.llm is not None
    assert result.llm.base_url == "http://aigw:8080/v1"
    assert result.llm.default_model == "qwen3.5-plus"
    assert "@one:matrix.local" in result.config_toml
    assert "@two:matrix.local" in result.config_toml


def test_bridge_port_remap_on_host_dev() -> None:
    result = bridge_openclaw_to_openhuman(
        {
            "channels": {"matrix": {"homeserver": "http://172.17.0.2:8080"}},
            "models": {
                "providers": {
                    "hiclaw-gateway": {
                        "baseUrl": "http://172.17.0.2:8080/v1",
                        "apiKey": "k",
                    }
                }
            },
            "agents": {"defaults": {"model": {"primary": "hiclaw-gateway/qwen-plus"}}},
        },
        env={"AGENTTEAMS_PORT_GATEWAY": "18080"},
    )

    assert result.llm is not None
    assert "18080" in result.llm.base_url
    assert "18080" in result.config_toml


def test_write_config_toml_roundtrip(tmp_path: Path) -> None:
    result = bridge_openclaw_to_openhuman(
        SAMPLE_OPENCLAW,
        env={"MATRIX_HOME_ROOM_ID": "!room:matrix.local"},
    )
    path = write_config_toml(tmp_path, result)
    assert path.is_file()
    assert path.read_text(encoding="utf-8") == result.config_toml


def test_bridge_openclaw_file(tmp_path: Path) -> None:
    openclaw = tmp_path / "openclaw.json"
    openclaw.write_text(json.dumps(SAMPLE_OPENCLAW), encoding="utf-8")
    result = bridge_openclaw_file(openclaw, env={"MATRIX_HOME_ROOM_ID": "!room:matrix.local"})
    assert result.llm is not None
    assert render_config_toml({"allowed_users": []}).startswith("# Auto-generated")
