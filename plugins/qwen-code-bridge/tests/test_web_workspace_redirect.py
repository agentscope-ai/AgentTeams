# -*- coding: utf-8 -*-
"""Regression: the embedded shell must boot scoped to a workspace.

The iframe used to open ``/web/`` without any workspace selector, so the
shell's session list came up empty. Now:

  1. the frontend resolves the primary workspace from ``GET /workspace``
     and appends ``&workspace=`` to the iframe src (node harness, mirrors
     ``test_frontend_wrapper.py``);
  2. the backend 302s a workspace-less *HTML index* navigation back to
     the same URL carrying the primary workspace (covers stale bundles
     and direct opens) — requests that already carry ``?workspace=`` and
     non-HTML requests (XHR/fetch/curl) pass through untouched.
"""

# pylint: disable=redefined-outer-name
from __future__ import annotations

import os
import shutil
import subprocess
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlsplit

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from qwen_code_bridge import web
from qwen_code_bridge.router import build_router

FRONTEND = Path(__file__).resolve().parents[1] / "frontend" / "index.js"


class _Upstream(BaseHTTPRequestHandler):
    def do_GET(self) -> None:  # noqa: N802
        body = f"SERVED GET {self.path}".encode()
        self.send_response(200)
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


def _primary_state(tmp_path) -> str:
    """The /workspace value the test client must resolve to: no env
    workspaces and no dispatch records, so the state dir is primary."""
    return os.path.realpath(str(tmp_path / "state"))


def test_index_navigation_without_workspace_redirects_302(
    upstream, client, tmp_path
) -> None:
    """HTML navigation to /web/ without ?workspace= -> 302 back to the
    same URL carrying the primary workspace; the redirect target then
    proxies normally, and GET /workspace reports that same value."""
    ws = client.get("/api/qwen-code-bridge/workspace").json()
    assert ws == {"workspace": _primary_state(tmp_path)}

    resp = client.get(
        "/api/qwen-code-bridge/web/?theme=dark",
        headers={"Accept": "text/html"},
        follow_redirects=False,
    )
    assert resp.status_code == 302
    parts = urlsplit(resp.headers["location"])
    assert parts.path == "/api/qwen-code-bridge/web/"
    query = parse_qs(parts.query)
    assert query["theme"] == ["dark"]
    assert query["workspace"] == [_primary_state(tmp_path)]

    follow = client.get(
        parts.path + "?" + parts.query, follow_redirects=False
    )
    assert follow.status_code == 200
    assert "SERVED GET /?" in follow.text


def test_workspace_param_and_non_html_requests_pass_through(
    upstream, client
) -> None:
    """?workspace= (any Accept) and non-HTML root requests must NOT be
    redirected — the shell's XHR/fetch calls and curl probes keep
    working exactly as before."""
    # root navigation that already carries a workspace selector
    resp = client.get(
        "/api/qwen-code-bridge/web/?theme=dark&workspace=%2Ftmp%2Fproj",
        headers={"Accept": "text/html"},
        follow_redirects=False,
    )
    assert resp.status_code == 200
    assert resp.headers.get("location") is None
    assert "SERVED GET /?" in resp.text

    # root GET with the default client Accept (*/*) — the curl-probe shape
    plain = client.get("/api/qwen-code-bridge/web/", follow_redirects=False)
    assert plain.status_code == 200
    assert plain.headers.get("location") is None
    assert "SERVED GET /" in plain.text

    # non-HTML API request under /web
    api = client.get(
        "/api/qwen-code-bridge/web/workspaces",
        headers={"Accept": "application/json"},
        follow_redirects=False,
    )
    assert api.status_code == 200
    assert api.headers.get("location") is None
    assert "SERVED GET /workspaces" in api.text


HARNESS = r"""
const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');

let registered = null;
const hooks = [];
let callIndex = 0;
const ReactMock = {
  useState(initial) {
    const i = callIndex++;
    if (hooks[i] === undefined) {
      hooks[i] = { value: typeof initial === "function" ? initial() : initial };
    }
    const slot = hooks[i];
    return [
      slot.value,
      function (next) {
        slot.value = typeof next === "function" ? next(slot.value) : next;
      },
    ];
  },
  useEffect(fn) { try { fn(); } catch (e) { throw e; } },
  useRef(value) { return { current: value }; },
  createElement(type, props) { return { type, props }; },
};
const windowMock = {
  location: { pathname: '/qwen-code', search: '' },
  localStorage: { getItem() { return null; } },
  matchMedia() { return { matches: false, addEventListener() {}, removeEventListener() {} }; },
  addEventListener() {},
  removeEventListener() {},
  setInterval() { return 1; },
  clearInterval() {},
  QwenPaw: {
    host: { React: ReactMock },
    React: ReactMock,
    registerRoutes(name, routes) { registered = routes; },
    route: { wrap() {} },
  },
};
global.window = windowMock;
global.localStorage = windowMock.localStorage;
global.fetch = function (url) {
  if (String(url).indexOf('/api/qwen-code-bridge/workspace') !== -1) {
    return Promise.resolve({
      ok: true,
      json: () => Promise.resolve({ workspace: '/tmp/ws-x' }),
    });
  }
  return Promise.resolve({ ok: false, json: () => Promise.resolve(null) });
};
global.matchMedia = windowMock.matchMedia;
global.setInterval = function () { return 1; };
global.clearInterval = function () {};
vm.runInThisContext(src, { filename: 'frontend-index.js' });

if (!registered || !registered.length || !registered[0].component) {
  console.error('routes not captured'); process.exit(2);
}
const Component = registered[0].component;
function render() { callIndex = 0; return Component(); }

(async function () {
  let first;
  try {
    first = render();
  } catch (e) {
    console.error('first render threw:', e && e.stack ? e.stack : e);
    process.exit(3);
  }
  // While the workspace is still resolving the iframe is held back so
  // the shell boots exactly once, already scoped.
  if (first !== null) {
    console.error(
      'first render should return null while resolving:',
      JSON.stringify(first),
    );
    process.exit(4);
  }
  await new Promise(function (r) { setTimeout(r, 50); });
  let el;
  try {
    el = render();
  } catch (e) {
    console.error('second render threw:', e && e.stack ? e.stack : e);
    process.exit(5);
  }
  if (!el || el.type !== 'iframe') {
    console.error('second render did not produce the iframe:', JSON.stringify(el));
    process.exit(6);
  }
  const expected =
    '/api/qwen-code-bridge/web/?theme=light&workspace=' +
    encodeURIComponent('/tmp/ws-x');
  if (el.props.src !== expected) {
    console.error('iframe src mismatch: got ' + el.props.src + ' want ' + expected);
    process.exit(7);
  }
  console.log('FRONTPASS-OK ' + el.props.src);
  process.exit(0);
})().catch(function (e) {
  console.error('harness failed:', e && e.stack ? e.stack : e);
  process.exit(9);
});
"""


def test_frontend_iframe_passes_workspace() -> None:
    """The /qwen-code page must hold the iframe back while the primary
    workspace resolves, then render it with &workspace= encoded into
    the src (the session list is scoped on first load)."""
    node = shutil.which("node")
    if node is None:
        pytest.skip("node not available")
    harness = Path(__file__).with_suffix(".harness.js")
    harness.write_text(HARNESS, encoding="utf-8")
    try:
        out = subprocess.run(
            [node, str(harness), str(FRONTEND)],
            capture_output=True,
            text=True,
            timeout=20,
        )
    finally:
        harness.unlink(missing_ok=True)
    assert out.returncode == 0, out.stderr + out.stdout
    assert "FRONTPASS-OK" in out.stdout
    assert "workspace=%2Ftmp%2Fws-x" in out.stdout
