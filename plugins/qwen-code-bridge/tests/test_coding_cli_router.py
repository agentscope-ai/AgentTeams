# -*- coding: utf-8 -*-
"""Unit tests for ``qwen_code_bridge.router``.

Ported from QwenPaw upstream PR 8156
(``tests/unit/app/routers/test_coding_cli_router.py``, 281 lines / 13
tests). Deltas vs upstream:

- ``~`` is redirected by pointing the ``HOME`` env var at a tmp dir
  (same effect as upstream's ``os.path.expanduser`` redirect, no
  patching).
- Install task state lives under ``QWENPAW_CODING_CLI_STATE_DIR`` (tmp).
- The router holds no ``app.state`` service; tests that drive the
  install state machine construct a ``CodingCliService()`` directly —
  same state dir, so task files written by the router's per-request
  service are visible to the test.
"""

# pylint: disable=protected-access,redefined-outer-name,unused-argument
from __future__ import annotations

import json
import os
import time
from types import SimpleNamespace
from typing import Generator
from unittest.mock import patch

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from qwen_code_bridge.router import router as coding_cli_router
from qwen_code_bridge.service import CodingCliService
from qwen_code_bridge.state import STATE_DIR_ENV

API = "/api/qwen-code-bridge"


@pytest.fixture
def app(tmp_path) -> Generator[FastAPI, None, None]:
    saved = {key: os.environ.get(key) for key in ("HOME", STATE_DIR_ENV)}
    os.environ["HOME"] = str(tmp_path)
    os.environ[STATE_DIR_ENV] = str(tmp_path / "state")
    application = FastAPI()
    application.include_router(coding_cli_router, prefix=API)
    yield application
    for key, value in saved.items():
        if value is None:
            os.environ.pop(key, None)
        else:
            os.environ[key] = value


@pytest.fixture
def client(app) -> TestClient:
    return TestClient(app)


@pytest.fixture
def no_bin() -> Generator[None, None, None]:
    """Make version probing report 'not installed'."""

    with patch(
        "qwen_code_bridge.service.CodingCliService._run",
        return_value=None,
    ):
        yield


def test_list_clis(client, no_bin) -> None:
    res = client.get(API)
    assert res.status_code == 200
    ids = {item["id"] for item in res.json()["clis"]}
    assert ids == {"qwen-code", "opencode", "qoder"}
    for item in res.json()["clis"]:
        assert item["installed"] is False
        assert item["auth"]["configured"] is False


def test_unknown_cli_404(client) -> None:
    assert client.get(f"{API}/claude-code").status_code == 404
    assert (
        client.put(
            f"{API}/claude-code/settings",
            json={"a": 1},
        ).status_code
        == 404
    )


def test_settings_roundtrip_redaction(client, tmp_path) -> None:
    qwen_dir = tmp_path / ".qwen"
    qwen_dir.mkdir()
    (qwen_dir / "settings.json").write_text(
        json.dumps(
            {
                "security": {
                    "auth": {
                        "selectedType": "openai",
                        "apiKey": "sk-secret",
                        "baseUrl": "http://h:7113/v1",
                    },
                },
                "model": {"name": "qwen3.6-27b-fp8"},
            },
        ),
    )
    res = client.get(f"{API}/qwen-code/settings")
    assert res.status_code == 200
    auth = res.json()["settings"]["security"]["auth"]
    assert auth["apiKey"] == {"redacted": True, "has_value": True}
    assert auth["baseUrl"] == "http://h:7113/v1"
    assert "sk-secret" not in res.text

    # PUT merge with *** preserves the secret.
    res = client.put(
        f"{API}/qwen-code/settings",
        json={
            "settings": {
                "security": {"auth": {"apiKey": "***"}},
                "tools": {"useBuiltinRipgrep": False},
            },
        },
    )
    assert res.status_code == 200
    on_disk = json.loads((qwen_dir / "settings.json").read_text())
    assert on_disk["security"]["auth"]["apiKey"] == "sk-secret"
    assert on_disk["tools"]["useBuiltinRipgrep"] is False
    assert on_disk["model"]["name"] == "qwen3.6-27b-fp8"


def test_settings_write_creates_file(client, tmp_path) -> None:
    res = client.put(
        f"{API}/opencode/settings",
        json={"settings": {"$schema": 1}},
    )
    assert res.status_code == 200
    assert (tmp_path / ".config/opencode/opencode.json").is_file()


def test_install_post_returns_202(client) -> None:
    res = client.post(
        f"{API}/qwen-code/install",
        json={"tag": "0.25.0"},
    )
    assert res.status_code == 202
    body = res.json()
    assert body["cli"] == "qwen-code"
    assert body["tag"] == "0.25.0"
    assert body["task_id"]


def test_install_lifecycle(client) -> None:
    """Drive the install state machine deterministically on one loop."""
    import asyncio

    from qwen_code_bridge.registry import get_cli

    service = CodingCliService()
    spec = get_cli("qwen-code")

    async def drive() -> dict:
        task_id = await service.start_install(spec, "0.25.0")
        for _ in range(100):
            task = service.install_status(task_id)
            if task and task["state"] != "running":
                return task
            await asyncio.sleep(0.02)
        return service.install_status(task_id)

    with (
        patch(
            "qwen_code_bridge.service.CodingCliService._run",
            return_value=SimpleNamespace(
                returncode=0,
                stdout="qwen 0.25.0",
                stderr="",
            ),
        ),
        patch(
            "qwen_code_bridge.service.CodingCliService._installable",
            return_value=True,
        ),
    ):
        task = asyncio.run(drive())

    assert task["state"] == "success"
    assert task["version"] == "0.25.0"
    res = client.get(f"{API}/qwen-code/install/{task['task_id']}")
    assert res.status_code == 200
    assert res.json()["state"] == "success"


def test_install_nightly_tag_uses_dist_tag(client) -> None:
    """A dist-tag like ``nightly`` must be appended as ``pkg@nightly``,
    not silently dropped to latest."""
    import asyncio

    from qwen_code_bridge.registry import get_cli

    service = CodingCliService()
    spec = get_cli("qwen-code")
    calls: list = []

    def fake_run(cmd, timeout=30.0):
        calls.append(cmd)
        return SimpleNamespace(returncode=0, stdout="qwen 0.25.0", stderr="")

    async def drive() -> None:
        task_id = await service.start_install(spec, "nightly")
        for _ in range(100):
            task = service.install_status(task_id)
            if task and task["state"] != "running":
                return
            await asyncio.sleep(0.02)

    with (
        patch.object(
            CodingCliService,
            "_run",
            staticmethod(fake_run),
        ),
        patch.object(CodingCliService, "_installable", return_value=True),
    ):
        asyncio.run(drive())

    assert any(
        cmd[:3] == ("npm", "install", "-g")
        and cmd[-1] == "@qwen-code/qwen-code@nightly"
        for cmd in calls
    )


def test_install_invalid_tag_400(client) -> None:
    res = client.post(
        f"{API}/qwen-code/install",
        json={"tag": "latest; rm -rf /"},
    )
    assert res.status_code == 400


def test_opencode_auth_probe_without_settings_file(client, tmp_path) -> None:
    """opencode keeps credentials in auth.json; the probe must report
    configured=True even when opencode.json does not exist."""
    auth_dir = tmp_path / ".local/share/opencode"
    auth_dir.mkdir(parents=True)
    (auth_dir / "auth.json").write_text("{}")
    res = client.get(API)
    items = {item["id"]: item for item in res.json()["clis"]}
    assert items["opencode"]["auth"]["configured"] is True


def test_qoder_auth_probe_via_pat_env(client) -> None:
    """qoder's headless auth is the QODER_PERSONAL_ACCESS_TOKEN env var;
    the probe reports configured=True only while it is set, and never
    echoes the value into the response."""
    res = client.get(API)
    items = {item["id"]: item for item in res.json()["clis"]}
    assert items["qoder"]["auth"]["configured"] is False

    os.environ["QODER_PERSONAL_ACCESS_TOKEN"] = "pat-secret-value"
    try:
        res = client.get(API)
        items = {item["id"]: item for item in res.json()["clis"]}
        assert items["qoder"]["auth"]["configured"] is True
        assert "pat-secret-value" not in res.text
    finally:
        del os.environ["QODER_PERSONAL_ACCESS_TOKEN"]


def test_qoder_settings_roundtrip(client, tmp_path) -> None:
    """qoder settings PUT lands in ~/.qoder/settings.json and deep-merges."""
    res = client.put(
        f"{API}/qoder/settings",
        json={
            "settings": {"model": {"name": "auto"}, "ui": {"maxTurns": 2000}},
        },
    )
    assert res.status_code == 200
    doc = json.loads(
        (tmp_path / ".qoder/settings.json").read_text(encoding="utf-8"),
    )
    assert doc["model"]["name"] == "auto"
    assert doc["ui"]["maxTurns"] == 2000


def test_install_busy_409(client) -> None:
    """A running install task file for the CLI blocks new installs (409)."""
    service = CodingCliService()
    task_id = "b" * 32
    (service._install_dir / f"{task_id}.json").write_text(
        json.dumps(
            {
                "task_id": task_id,
                "cli": "qwen-code",
                "tag": "latest",
                "state": "running",
                "version": None,
                "log_tail": "",
                "started_at": time.time(),
                "finished_at": None,
            },
        ),
        encoding="utf-8",
    )
    res = client.post(
        f"{API}/qwen-code/install",
        json={"tag": "latest"},
    )
    assert res.status_code == 409


def test_install_task_404(client) -> None:
    assert (
        client.get(
            f"{API}/qwen-code/install/nope",
        ).status_code
        == 404
    )
