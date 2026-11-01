# -*- coding: utf-8 -*-
"""P1411: caller authentication on the bridge WebSocket, before accept().

Regression for the maintainer's P1: with login auth enabled and a
registered account, an unauthenticated bridge WebSocket was accepted and
forwarded to the backend (which then demanded the bridge bearer token),
while the SDK's HTTP middleware rejected the same caller with 401. The
SDK's HTTP auth middleware only covers HTTP scopes, so the bridge
authenticates the WS caller itself before ``accept()``.
"""

from __future__ import annotations

import asyncio
import json
import shutil
import subprocess
import threading
from pathlib import Path

import pytest

qwenpaw_auth = pytest.importorskip("qwenpaw.app.auth", reason="SDK not installed")

from fastapi import FastAPI  # noqa: E402
from starlette.testclient import TestClient  # noqa: E402
from starlette.websockets import WebSocketDisconnect  # noqa: E402

from qwen_code_bridge import web  # noqa: E402
from qwen_code_bridge.router import build_router  # noqa: E402

WS_PATH = "/api/qwen-code-bridge/web/sessions"
SERVE_TOKEN = "serve-token-secret"

# ---------------------------------------------------------------------------
# Fake upstream: mimics the websockets.connect() context manager the bridge
# opens after accept(). State is shared across threads (the TestClient runs
# the app on its own thread); ordering is made deterministic with Events.
# ---------------------------------------------------------------------------


class FakeUpstreamStream:
    def __init__(self, store: dict, outbound: list) -> None:
        self._store = store
        self._outbound = list(outbound)

    def __aiter__(self):
        return self._iterate()

    async def _iterate(self):
        for message in self._outbound:
            yield message
        # hold the upstream open until the bridge tears it down
        await asyncio.Event().wait()

    async def send(self, message):
        self._store["client_messages"].append(message)
        self._store["client_msg_event"].set()

    async def send_bytes(self, message):
        self._store["client_messages"].append(message)
        self._store["client_msg_event"].set()


class FakeUpstreamConnection:
    def __init__(self, store: dict, outbound: list) -> None:
        self._store = store
        self._outbound = outbound

    async def __aenter__(self):
        store = self._store
        store["upstream_target"] = store["target"]
        store["upstream_headers"] = store["connect_kwargs"].get("additional_headers")
        store["connected_event"].set()
        return FakeUpstreamStream(store, self._outbound)

    async def __aexit__(self, *exc_info) -> bool:
        return False


def make_fake_connect(store: dict, outbound: list = ()):
    def fake_connect(target, **kwargs):
        store["target"] = target
        store["connect_kwargs"] = kwargs
        return FakeUpstreamConnection(store, outbound)

    return fake_connect


def fresh_store() -> dict:
    return {
        "connected_event": threading.Event(),
        "client_msg_event": threading.Event(),
        "client_messages": [],
        "upstream_headers": None,
        "upstream_target": None,
        "target": None,
        "connect_kwargs": {},
    }


@pytest.fixture
def client(monkeypatch, tmp_path):
    monkeypatch.setenv("QWEN_CODE_BRIDGE_SERVE_AUTOSTART", "0")
    monkeypatch.setenv("QWEN_CODE_BRIDGE_STATE_DIR", str(tmp_path / "state"))
    monkeypatch.setenv(web.SERVE_URL_ENV, "http://127.0.0.1:9")
    monkeypatch.setenv(web.SERVE_TOKEN_ENV, SERVE_TOKEN)
    app = FastAPI()
    app.include_router(build_router(), prefix="/api/qwen-code-bridge")
    return TestClient(app)


def _login_active(monkeypatch, verify=lambda token: "alice") -> None:
    monkeypatch.setattr(qwenpaw_auth, "is_auth_enabled", lambda: True)
    monkeypatch.setattr(qwenpaw_auth, "has_registered_users", lambda: True)
    monkeypatch.setattr(qwenpaw_auth, "verify_token", verify)


# 1. login active, no token -> 4401 before accept, upstream never contacted.
# starlette >= 1.x TestClient runs the app inside
# WebSocketTestSession.__enter__ (the websocket_connect() call only builds
# the session), so the pre-accept close(4401) surfaces as WebSocketDisconnect
# at context entry.
def test_auth_enabled_no_token_rejected_before_upstream(monkeypatch, client):
    _login_active(monkeypatch)
    store = fresh_store()
    monkeypatch.setattr("websockets.connect", make_fake_connect(store))
    with pytest.raises(WebSocketDisconnect) as exc_info:
        with client.websocket_connect(WS_PATH):
            pass
    assert exc_info.value.code == web.WS_AUTH_DENY_CODE == 4401
    assert not store["connected_event"].is_set()


# 2. login active, invalid/expired token -> 4401, upstream never contacted
def test_auth_enabled_invalid_token_rejected_before_upstream(monkeypatch, client):
    _login_active(monkeypatch, verify=lambda token: None)
    store = fresh_store()
    monkeypatch.setattr("websockets.connect", make_fake_connect(store))
    with pytest.raises(WebSocketDisconnect) as exc_info:
        with client.websocket_connect(WS_PATH + "?token=bogus"):
            pass
    assert exc_info.value.code == 4401
    assert not store["connected_event"].is_set()


# 3. login active, valid token -> accepted; the backend token stays
#    server-side (upstream sees it as Bearer, the client never receives it)
def test_auth_enabled_valid_token_accepted_backend_token_stays_server_side(
    monkeypatch, client
):
    _login_active(monkeypatch, verify=lambda token: "alice" if token == "caller-token" else None)
    store = fresh_store()
    monkeypatch.setattr(
        "websockets.connect", make_fake_connect(store, outbound=["upstream-hello"])
    )
    with client.websocket_connect(WS_PATH + "?token=caller-token") as session:
        assert store["connected_event"].wait(5)
        assert store["upstream_headers"] == {
            "Authorization": f"Bearer {SERVE_TOKEN}"
        }
        session.send_text("ping")
        assert store["client_msg_event"].wait(5)
        assert store["client_messages"] == ["ping"]
        text = session.receive_text()
        assert text == "upstream-hello"
        assert SERVE_TOKEN not in text


# 4. login disabled -> accepted without a token (unchanged behaviour)
def test_auth_disabled_accepts_without_token(monkeypatch, client):
    monkeypatch.setattr(qwenpaw_auth, "is_auth_enabled", lambda: False)
    store = fresh_store()
    monkeypatch.setattr("websockets.connect", make_fake_connect(store))
    with client.websocket_connect(WS_PATH) as session:
        assert store["connected_event"].wait(5)
        # the backend token still rides the upstream Authorization header
        assert store["upstream_headers"] == {"Authorization": f"Bearer {SERVE_TOKEN}"}


# 5. runtime-boundary parity: the SDK boundary closes 4401 (same shape our
#    handler mirrors) when the runtime token header is missing, and passes
#    when it is present
def test_runtime_boundary_parity(monkeypatch, tmp_path):
    from qwenpaw.app.auth import RuntimeBoundaryMiddleware

    monkeypatch.setenv("QWENPAW_RUNTIME_INTERNAL_TOKEN", "rt-token")
    monkeypatch.setenv("QWEN_CODE_BRIDGE_SERVE_AUTOSTART", "0")
    monkeypatch.setenv("QWEN_CODE_BRIDGE_STATE_DIR", str(tmp_path / "state"))
    monkeypatch.setenv(web.SERVE_URL_ENV, "http://127.0.0.1:9")
    monkeypatch.setattr(qwenpaw_auth, "is_auth_enabled", lambda: False)
    app = FastAPI()
    app.add_middleware(RuntimeBoundaryMiddleware)
    app.include_router(build_router(), prefix="/api/qwen-code-bridge")
    client = TestClient(app)

    denied_store = fresh_store()
    monkeypatch.setattr("websockets.connect", make_fake_connect(denied_store))
    with pytest.raises(WebSocketDisconnect) as exc_info:
        with client.websocket_connect(WS_PATH):
            pass
    assert exc_info.value.code == web.WS_AUTH_DENY_CODE == 4401
    assert not denied_store["connected_event"].is_set()

    allowed_store = fresh_store()
    monkeypatch.setattr("websockets.connect", make_fake_connect(allowed_store))
    with client.websocket_connect(
        WS_PATH, headers={"x-qwenpaw-runtime-token": "rt-token"}
    ) as session:
        assert allowed_store["connected_event"].wait(5)


# 6. SDK auth module unavailable + fail-closed switch -> 4401
def test_require_ws_auth_switch_fail_closed_when_sdk_missing(monkeypatch, client):
    monkeypatch.setattr(web, "_load_sdk_auth", lambda: None)
    monkeypatch.setenv(web.REQUIRE_WS_AUTH_ENV, "1")
    store = fresh_store()
    monkeypatch.setattr("websockets.connect", make_fake_connect(store))
    with pytest.raises(WebSocketDisconnect) as exc_info:
        with client.websocket_connect(WS_PATH):
            pass
    assert exc_info.value.code == 4401
    assert not store["connected_event"].is_set()


# 7. the injected shell shim appends the stored console token to WS URLs
#    (both the daemon-proxy shim and the rebuilt-shell shim); URLs without
#    a stored token, or with a token already in the query, are untouched.

# The shim source and scenarios arrive on stdin as one JSON object.
HARNESS = r"""
const fs = require('fs');
const vm = require('vm');

const input = JSON.parse(fs.readFileSync(0, 'utf8'));
const shimSrc = input.shim;
const scenarios = input.scenarios;

function makeContext(storedToken, daemonBase) {
  const seen = [];
  class InnerWebSocket {
    constructor(u, p) { seen.push(String(u)); }
  }
  const windowObj = {
    location: { pathname: '/console', origin: 'http://testserver', host: 'testserver' },
    fetch: function () {},
    WebSocket: InnerWebSocket,
    EventSource: class EventSource {},
    Worker: class Worker {},
    localStorage: {
      getItem(key) {
        return key === 'qwenpaw_auth_token' ? storedToken : null;
      },
    },
  };
  if (daemonBase) windowObj.__QWEN_DAEMON_BASE__ = daemonBase;
  const sandbox = {
    console,
    URL: URL, // browsers expose URL as a global; the shim relies on it
    window: windowObj,
    location: windowObj.location,
    XMLHttpRequest: { prototype: { open: function () {} } },
    __seen: seen,
  };
  return vm.createContext(sandbox);
}

let failed = false;
scenarios.forEach((s, i) => {
  const ctx = makeContext(s.stored, s.daemonBase || null);
  vm.runInContext(shimSrc, ctx, { filename: 'shim-' + i + '.js' });
  const patched = ctx.window.WebSocket;
  if (typeof patched !== 'function' || patched.name === 'InnerWebSocket') {
    console.error('scenario ' + i + ': window.WebSocket was not patched');
    failed = true;
    return;
  }
  patched(s.url);
  const seen = ctx.__seen;
  if (seen.length !== 1) {
    console.error('scenario ' + i + ': expected 1 inner call, got ' + JSON.stringify(seen));
    failed = true;
    return;
  }
  if (seen[0] !== s.expect) {
    console.error('scenario ' + i + ': got ' + seen[0] + ' want ' + s.expect);
    failed = true;
    return;
  }
});
if (failed) process.exit(1);
console.log('SHIM-OK');
"""

PREFIX_BASE = "ws://testserver/api/qwen-code-bridge/web/sessions"
DAEMON_BASE = "ws://testserver/sessions"

PREFIX_SCENARIOS = [
    {"stored": "tok-123", "url": PREFIX_BASE, "expect": PREFIX_BASE + "?token=tok-123"},
    {
        "stored": "tok-123",
        "url": PREFIX_BASE + "?limit=2",
        "expect": PREFIX_BASE + "?limit=2&token=tok-123",
    },
    {
        "stored": "tok-123",
        "url": PREFIX_BASE + "?token=other",
        "expect": PREFIX_BASE + "?token=other",
    },
    {"stored": None, "url": PREFIX_BASE, "expect": PREFIX_BASE},
    # P1411 round 2: the login token never leaves the bridge destination.
    # Cross-origin and unrelated same-origin URLs are preserved untouched.
    {
        "stored": "tok-123",
        "url": "wss://evil.example/socket",
        "expect": "wss://evil.example/socket",
    },
    {
        "stored": "tok-123",
        "url": "ws://testserver/other-service/ws",
        "expect": "ws://testserver/other-service/ws",
    },
    {"stored": "tok-123", "url": "/other-service/ws", "expect": "/other-service/ws"},
    # The shell's own root-absolute WebSocket route is mapped onto the mount
    # and stays authenticated (the existing success behaviour).
    {
        "stored": "tok-123",
        "url": "/sessions/live-state?x=1",
        "expect": (
            "ws://testserver/api/qwen-code-bridge/web/sessions/live-state"
            "?x=1&token=tok-123"
        ),
    },
]

DAEMON_SCENARIOS = [
    {
        "stored": "tok-123",
        "daemonBase": "/api/qwen-code-bridge/web",
        "url": DAEMON_BASE,
        "expect": PREFIX_BASE + "?token=tok-123",
    },
    {
        "stored": "tok-123",
        "daemonBase": "/api/qwen-code-bridge/web",
        "url": DAEMON_BASE + "?limit=2",
        "expect": PREFIX_BASE + "?limit=2&token=tok-123",
    },
    {
        "stored": "tok-123",
        "daemonBase": "/api/qwen-code-bridge/web",
        "url": DAEMON_BASE + "?token=other",
        "expect": PREFIX_BASE + "?token=other",
    },
    {
        "stored": None,
        "daemonBase": "/api/qwen-code-bridge/web",
        "url": DAEMON_BASE,
        "expect": PREFIX_BASE,
    },
    # P1411 round 2: cross-origin and unrelated same-origin URLs are never
    # rewritten and never receive the login token.
    {
        "stored": "tok-123",
        "daemonBase": "/api/qwen-code-bridge/web",
        "url": "wss://evil.example/socket",
        "expect": "wss://evil.example/socket",
    },
    {
        "stored": "tok-123",
        "daemonBase": "/api/qwen-code-bridge/web",
        "url": "ws://testserver/other-service/ws",
        "expect": "ws://testserver/other-service/ws",
    },
    {
        "stored": "tok-123",
        "daemonBase": "/api/qwen-code-bridge/web",
        "url": "/other-service/ws",
        "expect": "/other-service/ws",
    },
    {
        "stored": "tok-123",
        "daemonBase": "/api/qwen-code-bridge/web",
        "url": "/sessions/live-state?x=1",
        "expect": (
            "ws://testserver/api/qwen-code-bridge/web/sessions/live-state"
            "?x=1&token=tok-123"
        ),
    },
]


def _run_shim(node: str, harness: Path, shim_src: str, scenarios: list) -> None:
    out = subprocess.run(
        [node, str(harness)],
        capture_output=True,
        text=True,
        timeout=20,
        input=json.dumps({"shim": shim_src, "scenarios": scenarios}),
    )
    assert out.returncode == 0, out.stderr + out.stdout
    assert "SHIM-OK" in out.stdout


def test_shell_patch_appends_stored_token():
    node = shutil.which("node")
    if node is None:
        pytest.skip("node not available")
    harness = Path(__file__).with_name("test_web_ws_auth.harness.js")
    harness.write_text(HARNESS, encoding="utf-8")
    try:
        for shim_src, scenarios in (
            (web._PREFIX_SHIM, PREFIX_SCENARIOS),
            (web._DAEMON_WS_SHIM, DAEMON_SCENARIOS),
        ):
            _run_shim(node, harness, shim_src, scenarios)
    finally:
        harness.unlink(missing_ok=True)
