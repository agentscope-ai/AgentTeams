# -*- coding: utf-8 -*-
"""Regression tests for #1699: credential files must stay 0600.

The plugin writes three families of credential files:

- ``<state_dir>/serve.json`` (the qwen serve bearer token) via
  ``web._write_config``;
- the CLIs' settings files (``~/.qwen/settings.json`` holds
  ``security.auth.apiKey``) via
  ``service.CodingCliService.write_settings``;
- the CLIs' extra credential files (opencode's ``auth.json``), written
  by the CLI itself but tightened on every settings write.

All of them must end up mode 0600 regardless of the process umask.
"""

import asyncio
import json
import os
import stat

import pytest

from qwen_code_bridge import state


def _mode(path) -> int:
    return stat.S_IMODE(os.stat(str(path)).st_mode)


@pytest.fixture
def open_umask():
    """Worst-case umask (0): new files would be 0666 without an
    explicit chmod."""
    old = os.umask(0)
    try:
        yield
    finally:
        os.umask(old)


# 1. the shared helper --------------------------------------------------


def test_write_credential_file_creates_0600(tmp_path, open_umask):
    dest = tmp_path / "credentials" / "secret.json"
    state._write_credential_file(dest, b'{"token": "test-value"}')
    assert _mode(dest) == 0o600
    assert json.loads(dest.read_text(encoding="utf-8")) == {
        "token": "test-value"
    }


def test_write_credential_file_keeps_0600_on_rewrite(tmp_path, open_umask):
    """A previously-0600 file rewritten through the helper stays 0600.

    The old tmp + os.replace pair kept the temp file's umask-default
    permissions, so every rewrite re-loosened a 0600 file back to 0644.
    """
    dest = tmp_path / "settings.json"
    state._write_credential_file(dest, b'{"a": 1}')
    dest.chmod(0o600)  # e.g. the prepare_cli_home chmod pass
    state._write_credential_file(dest, b'{"a": 2}')
    assert _mode(dest) == 0o600
    assert json.loads(dest.read_text(encoding="utf-8")) == {"a": 2}


def test_write_credential_file_leaves_no_temp_files(tmp_path):
    dest = tmp_path / "serve.json"
    state._write_credential_file(dest, b"{}")
    state._write_credential_file(dest, b"{}")
    assert [p.name for p in tmp_path.iterdir()] == ["serve.json"]


def test_write_credential_file_removes_temp_on_failure(tmp_path, monkeypatch):
    dest = tmp_path / "secret.json"
    state._write_credential_file(dest, b"{}")

    def _boom(src, dst):
        raise OSError("simulated rename failure")

    monkeypatch.setattr(os, "replace", _boom)
    with pytest.raises(OSError, match="simulated"):
        state._write_credential_file(dest, b"{}")
    assert [p.name for p in tmp_path.iterdir()] == ["secret.json"]


# 2. web._write_config (serve.json holds the qwen serve token) ----------


def test_web_write_config_writes_0600_serve_json(tmp_path, monkeypatch, open_umask):
    from qwen_code_bridge import web

    monkeypatch.setenv(state.STATE_DIR_ENV, str(tmp_path / "state"))
    web._write_config("http://127.0.0.1:9", "test-token-value")
    dest = tmp_path / "state" / "serve.json"
    assert _mode(dest) == 0o600
    data = json.loads(dest.read_text(encoding="utf-8"))
    assert data["url"] == "http://127.0.0.1:9"
    assert data["token"] == "test-token-value"


# 3. service.write_settings (settings.json holds security.auth.apiKey) --


def test_write_settings_keeps_settings_file_0600(tmp_path, monkeypatch, open_umask):
    from qwen_code_bridge.registry import get_cli
    from qwen_code_bridge.service import CodingCliService

    monkeypatch.setenv("HOME", str(tmp_path))
    service = CodingCliService(install_dir=tmp_path / "install")
    spec = get_cli("qwen-code")
    result = asyncio.run(
        service.write_settings(spec, {"security": {"auth": {"apiKey": "test-api-key"}}})
    )
    dest = tmp_path / ".qwen" / "settings.json"
    assert _mode(dest) == 0o600
    data = json.loads(dest.read_text(encoding="utf-8"))
    assert data["security"]["auth"]["apiKey"] == "test-api-key"
    # The returned view redacts credentials — never raw values.
    assert result["security"]["auth"]["apiKey"] == {
        "redacted": True,
        "has_value": True,
    }


def test_write_settings_tightens_existing_extra_auth_files(tmp_path, monkeypatch, open_umask):
    from qwen_code_bridge.registry import get_cli
    from qwen_code_bridge.service import CodingCliService

    monkeypatch.setenv("HOME", str(tmp_path))
    service = CodingCliService(install_dir=tmp_path / "install")
    spec = get_cli("opencode")
    # CLI-written credential file exists at the umask-default mode.
    auth_dir = tmp_path / ".local" / "share" / "opencode"
    auth_dir.mkdir(parents=True)
    auth_file = auth_dir / "auth.json"
    auth_file.write_text('{"anthropic": {}}', encoding="utf-8")
    auth_file.chmod(0o644)

    asyncio.run(service.write_settings(spec, {"$schema": "https://example.invalid/config.json"}))

    assert _mode(auth_file) == 0o600
    settings = tmp_path / ".config" / "opencode" / "opencode.json"
    assert _mode(settings) == 0o600


def test_write_settings_preserves_existing_credentials(tmp_path, monkeypatch, open_umask):
    """``***`` (PRESERVE) keeps a previously written credential and the
    file stays 0600 across the merge."""
    from qwen_code_bridge.registry import get_cli, PRESERVE
    from qwen_code_bridge.service import CodingCliService

    monkeypatch.setenv("HOME", str(tmp_path))
    service = CodingCliService(install_dir=tmp_path / "install")
    spec = get_cli("qwen-code")
    asyncio.run(service.write_settings(spec, {"security": {"auth": {"apiKey": "test-api-key"}}}))
    asyncio.run(service.write_settings(spec, {"security": {"auth": {"apiKey": PRESERVE}}, "model": {"name": "test-model"}}))
    dest = tmp_path / ".qwen" / "settings.json"
    data = json.loads(dest.read_text(encoding="utf-8"))
    assert data["security"]["auth"]["apiKey"] == "test-api-key"
    assert data["model"] == {"name": "test-model"}
    assert _mode(dest) == 0o600

