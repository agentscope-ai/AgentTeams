"""Tests for MatrixBootstrapClient (W8.5)."""

from __future__ import annotations

import json
from unittest.mock import MagicMock, patch

from copaw_worker.matrix_bootstrap import MatrixBootstrapClient


def _cfg(token: str = "old-token") -> dict:
    return {
        "channels": {
            "matrix": {
                "homeserver": "http://localhost:6167",
                "accessToken": token,
            }
        }
    }


def test_relogin_skips_without_password(tmp_path):
    sync = MagicMock()
    sync._prefix = "agents/alice"
    sync._cat.return_value = ""
    sync.local_dir = tmp_path

    client = MatrixBootstrapClient(sync, worker_name="alice")
    result = client.relogin(_cfg())

    assert result["channels"]["matrix"]["accessToken"] == "old-token"


def test_relogin_updates_token_and_disk(tmp_path):
    sync = MagicMock()
    sync._prefix = "agents/alice"
    sync._cat.return_value = "secret"
    sync.local_dir = tmp_path
    (tmp_path / "openclaw.json").write_text(json.dumps(_cfg()))

    login_resp = json.dumps({"access_token": "new-token", "device_id": "DEV"}).encode()

    class FakeResponse:
        def __enter__(self):
            return self

        def __exit__(self, *args):
            return False

        def read(self):
            return login_resp

    with patch("urllib.request.urlopen", return_value=FakeResponse()):
        client = MatrixBootstrapClient(sync, worker_name="alice")
        result = client.relogin(_cfg())

    assert result["channels"]["matrix"]["accessToken"] == "new-token"
    on_disk = json.loads((tmp_path / "openclaw.json").read_text())
    assert on_disk["channels"]["matrix"]["accessToken"] == "new-token"


def test_join_pending_invites_posts_join_for_each_invite():
    sync = MagicMock()
    sync.local_dir = MagicMock()

    cfg = _cfg("token")
    sync_data = {
        "rooms": {
            "invite": {
                "!room1:example.org": {},
                "!room2:example.org": {},
            }
        }
    }

    calls: list[str] = []

    class FakeResponse:
        def __init__(self, payload: bytes | None = None):
            self._payload = payload or b"{}"

        def __enter__(self):
            return self

        def __exit__(self, *args):
            return False

        def read(self):
            return self._payload

    def fake_urlopen(req, timeout=30):
        url = req.full_url if hasattr(req, "full_url") else req.get_full_url()
        if "/sync?" in url:
            return FakeResponse(json.dumps(sync_data).encode())
        if "/join/" in url:
            calls.append(url)
            return FakeResponse()
        raise AssertionError(f"unexpected url: {url}")

    with patch("urllib.request.urlopen", side_effect=fake_urlopen):
        client = MatrixBootstrapClient(sync, worker_name="alice")
        client.join_pending_invites(cfg)

    assert len(calls) == 2
