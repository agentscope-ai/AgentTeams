"""The config validation matrix (task book gate 3).

Covers: full file, missing-credential reporting (each of the three),
empty values, file permissions (0644 refused with a chmod hint),
env-over-file precedence, env-only configuration, the built-in
defaults, and fail-closed validation (bad URL, relative socket path).
"""

from __future__ import annotations

import json
import os

import pytest

from sidecar import ConfigError, load_config

FULL = {
    "homeserver_url": "https://matrix.example.com",
    "agent_user": "@agent:example.com",
    "agent_access_token": "syt_secret_token",
    "socket_path": "/tmp/unit/sidecar.sock",
    "agent_name": "unit-agent",
}


def write_config(path, payload, mode=0o600):
    path.write_text(json.dumps(payload) + "\n", encoding="utf-8")
    os.chmod(path, mode)
    return str(path)


# --- the file path -----------------------------------------------------------


def test_full_file_is_loaded(tmp_path):
    path = write_config(tmp_path / "sidecar-config.json", FULL)
    cfg = load_config(path)
    assert cfg.homeserver_url == FULL["homeserver_url"]
    assert cfg.agent_user == FULL["agent_user"]
    assert cfg.agent_access_token == FULL["agent_access_token"]
    assert cfg.socket_path == FULL["socket_path"]
    assert cfg.agent_name == FULL["agent_name"]


def test_missing_all_credentials_lists_every_one(tmp_path):
    # no config file, no env (the fixture clears it): all three required
    # fields must be reported in one error, each with its env name
    with pytest.raises(ConfigError) as exc:
        load_config(str(tmp_path / "no-such-file.json"))
    message = str(exc.value)
    for key, env in (
        ("homeserver_url", "AGENTTEAMS_MATRIX_URL"),
        ("agent_user", "AGENTTEAMS_MATRIX_USER"),
        ("agent_access_token", "AGENTTEAMS_MATRIX_TOKEN"),
    ):
        assert key in message, message
        assert env in message, message


def test_missing_token_only_is_reported_with_its_env_name(tmp_path, monkeypatch):
    data = {k: v for k, v in FULL.items() if k != "agent_access_token"}
    monkeypatch.setenv("AGENTTEAMS_MATRIX_URL", "https://env.example.com")  # url from env
    path = write_config(tmp_path / "sidecar-config.json", data)
    with pytest.raises(ConfigError) as exc:
        load_config(path)
    message = str(exc.value)
    assert "agent_access_token" in message
    assert "AGENTTEAMS_MATRIX_TOKEN" in message
    # the other two were satisfied (file/env) and are not reported
    assert "homeserver_url" not in message
    assert "agent_user" not in message


def test_missing_user_is_reported(tmp_path, monkeypatch):
    data = {k: v for k, v in FULL.items() if k != "agent_user"}
    monkeypatch.setenv("AGENTTEAMS_MATRIX_USER", "")  # empty = unset (lib convention)
    path = write_config(tmp_path / "sidecar-config.json", data)
    with pytest.raises(ConfigError) as exc:
        load_config(path)
    assert "agent_user" in str(exc.value)
    assert "AGENTTEAMS_MATRIX_USER" in str(exc.value)


def test_empty_string_value_in_file_is_rejected(tmp_path):
    path = write_config(tmp_path / "sidecar-config.json", dict(FULL, socket_path=""))
    with pytest.raises(ConfigError) as exc:
        load_config(path)
    assert "socket_path" in str(exc.value)


def test_unknown_key_is_rejected(tmp_path):
    path = write_config(tmp_path / "sidecar-config.json", dict(FULL, bogus="x"))
    with pytest.raises(ConfigError) as exc:
        load_config(path)
    assert "bogus" in str(exc.value)


def test_non_string_value_is_rejected(tmp_path):
    path = write_config(tmp_path / "sidecar-config.json", dict(FULL, agent_name=42))
    with pytest.raises(ConfigError) as exc:
        load_config(path)
    assert "agent_name" in str(exc.value)


# --- permissions: the file holds credentials ---------------------------------


def test_world_readable_file_is_refused_with_chmod_hint(tmp_path):
    path = write_config(tmp_path / "sidecar-config.json", FULL, mode=0o644)
    with pytest.raises(ConfigError) as exc:
        load_config(path)
    message = str(exc.value)
    assert "0600" in message
    assert "chmod 600" in message
    # and the secret never leaks into the error
    assert "syt_secret_token" not in message


def test_group_writable_file_is_refused(tmp_path):
    path = write_config(tmp_path / "sidecar-config.json", FULL, mode=0o660)
    with pytest.raises(ConfigError):
        load_config(path)


# --- env precedence ------------------------------------------------------------


def test_env_overrides_file(tmp_path, monkeypatch):
    path = write_config(tmp_path / "sidecar-config.json", FULL)
    monkeypatch.setenv("AGENTTEAMS_MATRIX_TOKEN", "env-token")
    monkeypatch.setenv("AGENTTEAMS_MATRIX_AGENT", "env-agent")
    cfg = load_config(path)
    assert cfg.agent_access_token == "env-token"
    assert cfg.agent_name == "env-agent"
    # the rest came from the file
    assert cfg.homeserver_url == FULL["homeserver_url"]
    assert cfg.socket_path == FULL["socket_path"]


def test_env_only_no_file_uses_builtin_defaults(tmp_path, monkeypatch):
    # no config file anywhere: the default path is under HOME (tmp_path)
    monkeypatch.setenv("AGENTTEAMS_MATRIX_URL", "https://env.example.com")
    monkeypatch.setenv("AGENTTEAMS_MATRIX_USER", "@env:example.com")
    monkeypatch.setenv("AGENTTEAMS_MATRIX_TOKEN", "env-token")
    monkeypatch.setenv("AGENTTEAMS_MATRIX_SOCKET", str(tmp_path / "run" / "sidecar.sock"))
    cfg = load_config()
    assert cfg.homeserver_url == "https://env.example.com"
    assert cfg.agent_user == "@env:example.com"
    assert cfg.socket_path == str(tmp_path / "run" / "sidecar.sock")
    assert cfg.agent_name == "agentteams"  # built-in default


def test_default_socket_path_under_matrix_state_dir(tmp_path, monkeypatch):
    monkeypatch.setenv("AGENTTEAMS_MATRIX_URL", "https://env.example.com")
    monkeypatch.setenv("AGENTTEAMS_MATRIX_USER", "@env:example.com")
    monkeypatch.setenv("AGENTTEAMS_MATRIX_TOKEN", "env-token")
    cfg = load_config()
    assert cfg.socket_path == str(tmp_path / ".agentteams" / "matrix" / "sidecar.sock")


def test_cli_path_wins_over_env_and_default(tmp_path, monkeypatch):
    explicit = write_config(tmp_path / "explicit.json", FULL)
    monkeypatch.setenv("AGENTTEAMS_MATRIX_SIDECAR_CONFIG", str(tmp_path / "other.json"))
    cfg = load_config(explicit)
    assert cfg.agent_name == "unit-agent"  # from the CLI-named file, not other.json


# --- fail-closed validation ----------------------------------------------------


def test_homeserver_url_must_be_http_or_https(tmp_path, monkeypatch):
    monkeypatch.setenv("AGENTTEAMS_MATRIX_URL", "matrix.example.com")
    monkeypatch.setenv("AGENTTEAMS_MATRIX_USER", "@env:example.com")
    monkeypatch.setenv("AGENTTEAMS_MATRIX_TOKEN", "env-token")
    with pytest.raises(ConfigError) as exc:
        load_config(str(tmp_path / "absent.json"))
    assert "http://" in str(exc.value)


def test_socket_path_must_be_absolute(tmp_path, monkeypatch):
    monkeypatch.setenv("AGENTTEAMS_MATRIX_URL", "https://env.example.com")
    monkeypatch.setenv("AGENTTEAMS_MATRIX_USER", "@env:example.com")
    monkeypatch.setenv("AGENTTEAMS_MATRIX_TOKEN", "env-token")
    monkeypatch.setenv("AGENTTEAMS_MATRIX_SOCKET", "relative/sidecar.sock")
    with pytest.raises(ConfigError) as exc:
        load_config(str(tmp_path / "absent.json"))
    assert "absolute" in str(exc.value)


# --- secret hygiene --------------------------------------------------------------


def test_repr_masks_the_token(tmp_path):
    path = write_config(tmp_path / "sidecar-config.json", FULL)
    cfg = load_config(path)
    assert "syt_secret_token" not in repr(cfg)
    assert "<masked>" in repr(cfg)
