"""Tests for OpenClaw Matrix bootstrap (O13.5)."""

from __future__ import annotations

import json
import subprocess
from pathlib import Path
from unittest.mock import patch

import pytest

from agentteams_sync.openclaw_matrix import (
    bootstrap_openclaw_matrix,
    relogin_matrix,
    resolve_workspace,
    wipe_matrix_crypto,
)


def test_resolve_workspace_prefers_home(monkeypatch, tmp_path: Path) -> None:
    monkeypatch.setenv("HOME", str(tmp_path / "ws"))
    assert resolve_workspace() == tmp_path / "ws"


def test_resolve_workspace_falls_back_to_agentteams_root(monkeypatch, tmp_path: Path) -> None:
    monkeypatch.delenv("HOME", raising=False)
    monkeypatch.setenv("AGENTTEAMS_ROOT", str(tmp_path))
    monkeypatch.setenv("AGENTTEAMS_WORKER_NAME", "alice")
    assert resolve_workspace() == tmp_path / "agents" / "alice"


def test_wipe_matrix_crypto_removes_directory(tmp_path: Path, capsys) -> None:
    matrix_dir = tmp_path / ".openclaw" / "matrix"
    matrix_dir.mkdir(parents=True)
    (matrix_dir / "store.db").write_text("data")

    wipe_matrix_crypto(tmp_path)

    assert not matrix_dir.exists()
    out = capsys.readouterr().out
    assert "Cleaned Matrix crypto storage" in out


def test_wipe_matrix_crypto_idempotent(tmp_path: Path) -> None:
    wipe_matrix_crypto(tmp_path)
    wipe_matrix_crypto(tmp_path)


def test_relogin_skips_without_password(tmp_path: Path, monkeypatch, capsys) -> None:
    cfg = {"channels": {"matrix": {"homeserver": "http://matrix:6167", "accessToken": "old"}}}
    (tmp_path / "openclaw.json").write_text(json.dumps(cfg))
    monkeypatch.setenv("AGENTTEAMS_STORAGE_PREFIX", "hiclaw/bucket")

    def fake_mc(*args, **_kwargs):
        return subprocess.CompletedProcess(args, 1, stdout="", stderr="missing")

    monkeypatch.setattr("agentteams_sync.openclaw_matrix.mc_ops.mc", fake_mc)

    assert relogin_matrix(worker_name="alice", workspace=tmp_path) is False
    assert json.loads((tmp_path / "openclaw.json").read_text())["channels"]["matrix"]["accessToken"] == "old"
    assert "No Matrix password found in MinIO" in capsys.readouterr().out


def test_relogin_skips_without_homeserver(tmp_path: Path, monkeypatch, capsys) -> None:
    (tmp_path / "openclaw.json").write_text(json.dumps({"channels": {"matrix": {}}}))
    monkeypatch.setenv("AGENTTEAMS_STORAGE_PREFIX", "hiclaw/bucket")

    def fake_mc(*args, **_kwargs):
        return subprocess.CompletedProcess(args, 0, stdout="secret\n", stderr="")

    monkeypatch.setattr("agentteams_sync.openclaw_matrix.mc_ops.mc", fake_mc)

    assert relogin_matrix(worker_name="alice", workspace=tmp_path) is False
    assert "Missing homeserver URL" in capsys.readouterr().out


def test_relogin_updates_token_on_success(tmp_path: Path, monkeypatch) -> None:
    cfg = {"channels": {"matrix": {"homeserver": "http://matrix:6167", "accessToken": "old"}}}
    (tmp_path / "openclaw.json").write_text(json.dumps(cfg))
    monkeypatch.setenv("AGENTTEAMS_STORAGE_PREFIX", "hiclaw/bucket")

    def fake_mc(*args, **_kwargs):
        return subprocess.CompletedProcess(args, 0, stdout="secret", stderr="")

    monkeypatch.setattr("agentteams_sync.openclaw_matrix.mc_ops.mc", fake_mc)

    login_resp = json.dumps({"access_token": "new-token", "device_id": "DEV123"}).encode()

    class FakeResponse:
        def __enter__(self):
            return self

        def __exit__(self, *args):
            return False

        def read(self):
            return login_resp

    with patch("urllib.request.urlopen", return_value=FakeResponse()):
        assert relogin_matrix(worker_name="alice", workspace=tmp_path) is True

    on_disk = json.loads((tmp_path / "openclaw.json").read_text())
    assert on_disk["channels"]["matrix"]["accessToken"] == "new-token"


def test_relogin_logs_failure_response(tmp_path: Path, monkeypatch, capsys) -> None:
    cfg = {"channels": {"matrix": {"homeserver": "http://matrix:6167", "accessToken": "old"}}}
    (tmp_path / "openclaw.json").write_text(json.dumps(cfg))
    monkeypatch.setenv("AGENTTEAMS_STORAGE_PREFIX", "hiclaw/bucket")

    def fake_mc(*args, **_kwargs):
        return subprocess.CompletedProcess(args, 0, stdout="secret", stderr="")

    monkeypatch.setattr("agentteams_sync.openclaw_matrix.mc_ops.mc", fake_mc)

    login_resp = json.dumps({"errcode": "M_FORBIDDEN"}).encode()

    class FakeResponse:
        def __enter__(self):
            return self

        def __exit__(self, *args):
            return False

        def read(self):
            return login_resp

    with patch("urllib.request.urlopen", return_value=FakeResponse()):
        assert relogin_matrix(worker_name="alice", workspace=tmp_path) is False

    out = capsys.readouterr().out
    assert "Matrix re-login failed" in out
    assert "M_FORBIDDEN" in out


def test_bootstrap_wipes_then_relogins(tmp_path: Path, monkeypatch) -> None:
    matrix_dir = tmp_path / ".openclaw" / "matrix"
    matrix_dir.mkdir(parents=True)
    (matrix_dir / "store.db").write_text("x")
    cfg = {"channels": {"matrix": {"homeserver": "http://matrix:6167", "accessToken": "old"}}}
    (tmp_path / "openclaw.json").write_text(json.dumps(cfg))
    monkeypatch.setenv("AGENTTEAMS_WORKER_NAME", "alice")
    monkeypatch.setenv("AGENTTEAMS_STORAGE_PREFIX", "hiclaw/bucket")

    def fake_mc(*args, **_kwargs):
        return subprocess.CompletedProcess(args, 0, stdout="secret", stderr="")

    monkeypatch.setattr("agentteams_sync.openclaw_matrix.mc_ops.mc", fake_mc)

    login_resp = json.dumps({"access_token": "fresh", "device_id": "D1"}).encode()

    class FakeResponse:
        def __enter__(self):
            return self

        def __exit__(self, *args):
            return False

        def read(self):
            return login_resp

    with patch("urllib.request.urlopen", return_value=FakeResponse()):
        bootstrap_openclaw_matrix(worker_name="alice", workspace=tmp_path)

    assert not matrix_dir.exists()
    assert json.loads((tmp_path / "openclaw.json").read_text())["channels"]["matrix"]["accessToken"] == "fresh"


def test_main_rejects_non_openclaw_contract() -> None:
    from agentteams_sync.openclaw_matrix import main

    with pytest.raises(SystemExit):
        main(["openclaw-matrix", "--contract=copaw"])
