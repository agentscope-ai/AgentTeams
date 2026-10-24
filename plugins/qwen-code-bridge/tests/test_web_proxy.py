# -*- coding: utf-8 -*-
"""P3 tests: qwen serve status probe + reverse proxy (fake upstream)."""

# pylint: disable=redefined-outer-name
from __future__ import annotations

import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from qwen_code_bridge import web
from qwen_code_bridge.router import build_router


class _Upstream(BaseHTTPRequestHandler):
    def do_GET(self) -> None:  # noqa: N802
        body = f"SERVED GET {self.path}".encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self) -> None:  # noqa: N802
        length = int(self.headers.get("Content-Length") or 0)
        payload = self.rfile.read(length).decode("utf-8", "ignore")
        body = f"SERVED POST {self.path} {payload}".encode()
        self.send_response(201)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args) -> None:
        pass


@pytest.fixture
def upstream(monkeypatch):
    server = HTTPServer(("127.0.0.1", 0), _Upstream)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    url = f"http://127.0.0.1:{server.server_address[1]}"
    monkeypatch.setenv(web.SERVE_URL_ENV, url)
    yield url
    server.shutdown()


@pytest.fixture
def client(monkeypatch, tmp_path):
    monkeypatch.setenv("QWEN_CODE_BRIDGE_SERVE_AUTOSTART", "0")
    monkeypatch.setenv("QWEN_CODE_BRIDGE_STATE_DIR", str(tmp_path / "state"))
    app = FastAPI()
    app.include_router(build_router(), prefix="/api/qwen-code-bridge")
    return TestClient(app)


def test_status_reports_unreachable(monkeypatch, client) -> None:
    monkeypatch.setenv(web.SERVE_URL_ENV, "http://127.0.0.1:9")
    data = client.get("/api/qwen-code-bridge/web/status").json()
    assert data["reachable"] is False
    assert "error" in data


def test_status_ok(upstream, client) -> None:
    # the fake upstream answers any path, including /health
    data = client.get("/api/qwen-code-bridge/web/status").json()
    assert data["reachable"] is True
    assert data["status_code"] == 200


def test_proxy_get(upstream, client) -> None:
    resp = client.get("/api/qwen-code-bridge/web/sessions?limit=2")
    assert resp.status_code == 200
    assert "SERVED GET /sessions?limit=2" in resp.text


def test_proxy_post_with_body(upstream, client) -> None:
    resp = client.post("/api/qwen-code-bridge/web/prompt", json={"text": "hi"})
    assert resp.status_code == 201
    assert "SERVED POST /prompt" in resp.text
    assert 'hi' in resp.text and 'text' in resp.text


def test_proxy_unreachable_returns_502(monkeypatch, client) -> None:
    monkeypatch.setenv(web.SERVE_URL_ENV, "http://127.0.0.1:9")
    resp = client.get("/api/qwen-code-bridge/web/x")
    assert resp.status_code == 502
    assert resp.json()["error"] == "serve-unreachable"


def test_token_from_env(monkeypatch) -> None:
    monkeypatch.setenv(web.SERVE_TOKEN_ENV, "tok-1")
    assert web.serve_token() == "tok-1"
    monkeypatch.delenv(web.SERVE_TOKEN_ENV)
    assert isinstance(web.serve_token(), str)


class _HtmlUpstream(BaseHTTPRequestHandler):
    def do_GET(self) -> None:  # noqa: N802
        if self.path.startswith("/assets/"):
            body = b"console.log(1)"
            ctype = "application/javascript"
        else:
            body = (
                b"<html><head></head><body>"
                b'<script src="/assets/app.js"></script>'
                b'<link href="/manifest.webmanifest"></body></html>'
            )
            ctype = "text/html"
        self.send_response(200)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args) -> None:
        pass


def test_html_rewrite_and_asset_passthrough(monkeypatch, client) -> None:
    server = HTTPServer(("127.0.0.1", 0), _HtmlUpstream)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    monkeypatch.setenv(web.SERVE_URL_ENV, f"http://127.0.0.1:{server.server_address[1]}")
    try:
        html = client.get("/api/qwen-code-bridge/web/").text
        assert '="/api/qwen-code-bridge/web/assets/app.js"' in html
        assert '="/api/qwen-code-bridge/web/manifest.webmanifest"' in html
        assert "XMLHttpRequest.prototype.open" in html  # prefix shim injected
        js = client.get("/api/qwen-code-bridge/web/assets/app.js")
        assert js.text == "console.log(1)"
        assert "XMLHttpRequest" not in js.text
    finally:
        server.shutdown()


def test_rebuilt_dist_served_statically(monkeypatch, tmp_path) -> None:
    """A rebuilt (base-prefixed) shell in <state>/web-shell/dist wins over proxy."""
    state = tmp_path / "state"
    dist = state / "web-shell" / "dist"
    (dist / "assets").mkdir(parents=True)
    (dist / "index.html").write_text(
        "<html><head></head><body>REBUILT-SHELL</body></html>",
        encoding="utf-8",
    )
    (dist / "assets" / "a.js").write_text("console.log(1)", encoding="utf-8")
    monkeypatch.setenv("QWEN_CODE_BRIDGE_STATE_DIR", str(state))
    monkeypatch.setenv("QWEN_CODE_BRIDGE_SERVE_AUTOSTART", "0")
    monkeypatch.setenv(web.SERVE_URL_ENV, "http://127.0.0.1:9")
    app = FastAPI()
    app.include_router(build_router(), prefix="/api/qwen-code-bridge")
    http = TestClient(app)
    html = http.get("/api/qwen-code-bridge/web/").text
    assert "REBUILT-SHELL" in html
    assert "window.__QWEN_DAEMON_BASE__" in html
    js = http.get("/api/qwen-code-bridge/web/assets/a.js")
    assert js.text == "console.log(1)"


def test_workspace_path_normalization() -> None:
    """The shell emits /workspaces//tmp/sessions; the daemon needs one segment."""
    assert (
        web._fix_workspace_path("workspaces//tmp/sessions")
        == "workspaces/%2Ftmp/sessions"
    )
    assert (
        web._fix_workspace_path("/workspaces//app/x/sessions/live-state")
        == "/workspaces/%2Fapp%2Fx/sessions/live-state"
    )
    assert (
        web._fix_workspace_path("workspace//tmp/session-groups")
        == "workspace/%2Ftmp/session-groups"
    )
    assert web._fix_workspace_path("capabilities") == "capabilities"
    assert (
        web._fix_workspace_path("workspaces/e9671acd244849c5/sessions")
        == "workspaces/e9671acd244849c5/sessions"
    )


def test_client_routes_serve_spa_not_management_404(monkeypatch, tmp_path) -> None:
    """/web/settings must not be swallowed by /{cli_id}/settings."""
    state = tmp_path / "state"
    dist = state / "web-shell" / "dist"
    dist.mkdir(parents=True)
    (dist / "index.html").write_text(
        "<html><head></head><body>REBUILT</body></html>",
        encoding="utf-8",
    )
    monkeypatch.setenv("QWEN_CODE_BRIDGE_STATE_DIR", str(state))
    monkeypatch.setenv("QWEN_CODE_BRIDGE_SERVE_AUTOSTART", "0")
    monkeypatch.setenv(web.SERVE_URL_ENV, "http://127.0.0.1:9")
    app = FastAPI()
    app.include_router(build_router(), prefix="/api/qwen-code-bridge")
    http = TestClient(app)
    for path in ("/web/settings", "/web/session/abc", "/web/threads"):
        resp = http.get(
            "/api/qwen-code-bridge" + path,
            headers={"Accept": "text/html"},
        )
        assert resp.status_code == 200, path
        assert "REBUILT" in resp.text, path


class _SpaUpstream(BaseHTTPRequestHandler):
    """/ → HTML index, other client routes → 404 JSON (like qwen serve)."""

    def do_GET(self) -> None:  # noqa: N802
        if self.path in ("/", ""):
            body = b"<html><head></head><body>UPSTREAM-SHELL</body></html>"
            code, ctype = 200, "text/html"
        else:
            body = b'{"detail":"Not Found"}'
            code, ctype = 404, "application/json"
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args) -> None:
        pass


def test_client_route_falls_back_without_rebuilt_dist(monkeypatch, tmp_path, client) -> None:
    """No rebuilt shell: client-route 404s must still boot the daemon's SPA."""
    server = HTTPServer(("127.0.0.1", 0), _SpaUpstream)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    monkeypatch.setenv(web.SERVE_URL_ENV, f"http://127.0.0.1:{server.server_address[1]}")
    monkeypatch.setenv("QWEN_CODE_BRIDGE_STATE_DIR", str(tmp_path / "no-dist"))
    try:
        resp = client.get(
            "/api/qwen-code-bridge/web/session/abc",
            headers={"Accept": "text/html"},
        )
        assert resp.status_code == 200
        assert "UPSTREAM-SHELL" in resp.text
        assert '{"detail"' not in resp.text
    finally:
        server.shutdown()


def test_drop_nested_workspaces():
    from qwen_code_bridge.web import _drop_nested_workspaces

    assert _drop_nested_workspaces(
        ["/tmp", "/tmp/qcb-persist", "/a", "/a/b", "/c", "/c"]
    ) == ["/tmp", "/a", "/c"]
    assert _drop_nested_workspaces(["/x/y", "/x"]) == ["/x"]
