"""build_client factory tests for the core matrix-nio transport (M4a).

The factory must be equivalent to the QwenPaw channel's legacy client
construction: same ``AsyncClient`` kwargs, same ``AsyncClientConfig``,
same store-path handling.  The query-token ``send`` compatibility is
opt-in via ``TransportConfig.query_token`` (off by default — modern
Synapse rejects the ``Authorization`` header + query-token mix) and is
tested on a ``query_token=True`` client.  Constructing the client is
fully local — no network I/O is performed (the parent
``AsyncClient.send`` is stubbed where ``send`` is tested).
"""

from __future__ import annotations

import asyncio
from typing import Any, Dict

import pytest

pytest.importorskip("nio")

from nio import AsyncClient  # noqa: E402

from core.transport import (  # noqa: E402
    MIN_SYNC_TIMEOUT_MS,
    QwenPawMatrixClient,
    TransportConfig,
    build_client,
)


def _config(**overrides: Any) -> TransportConfig:
    kwargs: Dict[str, Any] = {
        "homeserver": "https://homeserver.example/",
        "user_id": "",
        "access_token": "",
        "device_id": "",
        "store_path": None,
        "encryption": False,
        "sync_timeout_ms": None,
        "query_token": False,
    }
    kwargs.update(overrides)
    return TransportConfig(**kwargs)


def _run(scenario):
    """Run an async scenario coroutine to completion (no pytest-asyncio)."""
    return asyncio.run(scenario)


def test_build_client_default_returns_plain_async_client():
    """The default (``query_token=False``) must be a plain nio
    ``AsyncClient``: matrix-nio already sends the ``Authorization``
    header, and appending the access token to the query as well is
    rejected by modern Synapse ("mixing Authorization headers and
    access_token query parameters")."""
    client = build_client(_config())
    assert type(client) is AsyncClient
    assert not isinstance(client, QwenPawMatrixClient)


def test_build_client_query_token_returns_qwenpaw_matrix_client():
    client = build_client(_config(query_token=True))
    assert isinstance(client, QwenPawMatrixClient)
    assert isinstance(client, AsyncClient)


def test_build_client_passthrough_attributes():
    client = build_client(
        _config(
            homeserver="https://hs.example.com/",
            user_id="@bot:hs.example.com",
            device_id="DEVICE123",
            access_token="syt_token123",
        )
    )
    assert client.homeserver == "https://hs.example.com/"
    assert client.user == "@bot:hs.example.com"
    assert client.device_id == "DEVICE123"
    assert client.access_token == "syt_token123"
    # No on-disk store requested → nio store path stays empty.
    assert client.store_path == ""


def test_build_client_store_path_created(tmp_path):
    store = tmp_path / "matrix_crypto_store"
    assert not store.exists()
    client = build_client(_config(store_path=store))
    assert store.is_dir()
    assert client.store_path == str(store)


def test_build_client_sync_timeout_below_floor_clamped():
    """500 ms is below the 1 s floor: the factory must clamp it to
    ``MIN_SYNC_TIMEOUT_MS`` so a misconfigured value cannot turn the
    sync loop into a busy-spin."""
    client = build_client(_config(sync_timeout_ms=500))
    assert client.sync_timeout_ms == MIN_SYNC_TIMEOUT_MS
    assert client.sync_timeout_ms == 1000


def test_build_client_sync_timeout_negative_clamped():
    client = build_client(_config(sync_timeout_ms=-5000))
    assert client.sync_timeout_ms == MIN_SYNC_TIMEOUT_MS


def test_build_client_sync_timeout_none_resolves_to_floor():
    """``sync_timeout_ms=None`` means "no opinion": it resolves to the
    floor, never below it."""
    client = build_client(_config(sync_timeout_ms=None))
    assert client.sync_timeout_ms == MIN_SYNC_TIMEOUT_MS


def test_build_client_sync_timeout_at_or_above_floor_unchanged():
    client = build_client(_config(sync_timeout_ms=30000))
    assert client.sync_timeout_ms == 30000
    exact = build_client(_config(sync_timeout_ms=1000))
    assert exact.sync_timeout_ms == 1000


def test_build_client_config_fields():
    client = build_client(_config(sync_timeout_ms=60_000))
    assert client.config.store_sync_tokens is False
    assert client.config.encryption_enabled is False
    # 60 s long-poll → request_timeout = max(60 + 30, 60) = 90.
    assert client.config.request_timeout == 90


def test_build_client_config_encryption_flag():
    from nio import crypto as _nio_crypto

    if not getattr(_nio_crypto, "ENCRYPTION_ENABLED", False):
        pytest.skip("E2E encryption dependencies (olm) not installed")
    encrypted = build_client(_config(encryption=True))
    assert encrypted.config.encryption_enabled is True


def _capture_parent_send(monkeypatch) -> Dict[str, Any]:
    seen: Dict[str, Any] = {}

    async def fake_parent_send(
        self,
        method,
        path,
        data=None,
        headers=None,
        trace_context=None,
        timeout=None,
    ):
        seen["method"] = method
        seen["path"] = path
        return "sent"

    monkeypatch.setattr(AsyncClient, "send", fake_parent_send)
    return seen


def test_build_client_query_token_send_compat(monkeypatch):
    """The opt-in client must keep query-token auth for homeservers/
    proxies that drop auth headers: the access token is appended to the
    request query by ``send`` before reaching the parent implementation
    (stubbed here — no network)."""
    seen = _capture_parent_send(monkeypatch)
    client = build_client(
        _config(access_token="syt_token123", query_token=True)
    )
    result = _run(client.send("GET", "/_matrix/client/v3/rooms"))
    assert result == "sent"
    assert seen["method"] == "GET"
    assert seen["path"].startswith("/_matrix/client/v3/rooms?")
    assert "access_token=syt_token123" in seen["path"]


def test_build_client_send_does_not_duplicate_existing_token(monkeypatch):
    seen = _capture_parent_send(monkeypatch)
    client = build_client(
        _config(access_token="syt_token123", query_token=True)
    )
    _run(
        client.send(
            "GET",
            "/_matrix/client/v3/rooms?access_token=syt_token123",
        )
    )
    assert seen["path"].count("access_token=") == 1


def test_build_client_send_without_token_untouched(monkeypatch):
    seen = _capture_parent_send(monkeypatch)
    client = build_client(_config())
    _run(client.send("GET", "/_matrix/client/v3/rooms"))
    assert seen["path"] == "/_matrix/client/v3/rooms"


def test_build_client_default_send_omits_query_token(monkeypatch):
    """Regression guard for the real-world failure: the default plain
    client must not append the access token to the query — modern
    Synapse rejects the ``Authorization`` header + query-token mix
    (``M_MISSING_TOKEN``), which broke the E2EE key upload."""
    seen = _capture_parent_send(monkeypatch)
    client = build_client(_config(access_token="syt_token123"))
    _run(client.send("GET", "/_matrix/client/v3/rooms"))
    assert seen["path"] == "/_matrix/client/v3/rooms"
