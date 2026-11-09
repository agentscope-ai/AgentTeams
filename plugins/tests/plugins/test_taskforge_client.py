"""TaskForge client reservation tests (1.2.7, issue #1510).

Drives the stdlib-only client at
``plugins/teamharness/mcp/lifecycle/taskforge_client.py`` against a local
``http.server`` stub, plus a silent socket and a closed port for the
transport failures. Coverage:

1. GET success — Authorization and X-Request-Id are correct
   (caller-supplied id, and the uuid4 default);
2. POST passes If-Match and Idempotency-Key through, and sends neither
   when the caller omits them (pass-through, never self-generated);
3. HTTP 503 -> TaskForgeUnavailable, single attempt (no auto-retry);
4. HTTP 409 -> TaskForgeConflict with the server ``code`` retrievable;
5. connection refused -> TaskForgeUnavailable;
6. request timeout -> TaskForgeUnavailable;
7. no file appears in the working directory around the calls (the client
   has no local-file fallback).
"""

from __future__ import annotations

import importlib.util
import json
import socket
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

import pytest

MCP_DIR = Path(__file__).resolve().parents[2] / "teamharness" / "mcp"
CLIENT_PATH = MCP_DIR / "lifecycle" / "taskforge_client.py"


def _load_client() -> Any:
    spec = importlib.util.spec_from_file_location(
        "teamharness_taskforge_client", CLIENT_PATH
    )
    assert spec is not None and spec.loader is not None, (
        f"missing client module: {CLIENT_PATH}"
    )
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


tf = _load_client()


class _StubHandler(BaseHTTPRequestHandler):
    """Records each request (method/path/headers/body) and routes by path."""

    protocol_version = "HTTP/1.1"

    def log_message(self, format: str, *args: Any) -> None:
        pass  # keep pytest output clean

    def _record(self) -> None:
        length = int(self.headers.get("Content-Length") or 0)
        self.server.requests.append(
            {
                "method": self.command,
                "path": self.path,
                "headers": {key: value for key, value in self.headers.items()},
                "body": self.rfile.read(length) if length else b"",
            }
        )

    def _respond(self, status: int, payload: dict[str, Any] | None) -> None:
        body = b"" if payload is None else json.dumps(payload).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if body:
            self.wfile.write(body)

    def do_GET(self) -> None:
        self._record()
        if self.path.startswith("/unavailable"):
            self._respond(503, {"error": "maintenance"})
        else:
            self._respond(200, {"ok": True, "path": self.path})

    def do_POST(self) -> None:
        self._record()
        if self.path.startswith("/unavailable"):
            self._respond(503, {"error": "maintenance"})
        elif self.path.startswith("/conflict"):
            self._respond(409, {"code": "etag_mismatch", "message": "stale etag"})
        else:
            self._respond(201, {"created": True, "path": self.path})


@pytest.fixture()
def stub():
    server = ThreadingHTTPServer(("127.0.0.1", 0), _StubHandler)
    server.requests = []  # recorded by _StubHandler._record
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield server
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


def _base(server: ThreadingHTTPServer) -> str:
    return f"http://127.0.0.1:{server.server_address[1]}"


def _client(server: ThreadingHTTPServer) -> Any:
    return tf.TaskForgeClient(_base(server), token="tok-test", timeout=5.0)


def _free_port() -> int:
    probe = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    probe.bind(("127.0.0.1", 0))
    port = probe.getsockname()[1]
    probe.close()
    return port


@pytest.fixture()
def silent_port():
    """A port that accepts the TCP connection but never answers."""
    listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    listener.bind(("127.0.0.1", 0))
    listener.listen(1)
    port = listener.getsockname()[1]

    def _hold() -> None:
        conn, _ = listener.accept()
        try:
            time.sleep(2.0)
        finally:
            conn.close()

    thread = threading.Thread(target=_hold, daemon=True)
    thread.start()
    yield port
    listener.close()
    thread.join(timeout=5)


def _snapshot(workdir: Path) -> list[str]:
    return sorted(path.relative_to(workdir).as_posix() for path in workdir.rglob("*"))


# 1) GET success: Authorization / X-Request-Id are correct.

def test_get_success_sends_auth_and_request_id(stub):
    client = _client(stub)
    data = client.get(tf.PROJECTS, params={"limit": 2}, request_id="req-123")
    assert data == {"ok": True, "path": "/projects?limit=2"}
    request = stub.requests[-1]
    assert request["method"] == "GET"
    assert request["headers"].get("Authorization") == "Bearer tok-test"
    assert request["headers"].get("X-Request-Id") == "req-123"
    assert request["headers"].get("Content-Type") == "application/json"


def test_get_default_request_id_is_uuid4(stub):
    client = _client(stub)
    client.get(tf.PROJECTS)
    request = stub.requests[-1]
    value = request["headers"].get("X-Request-Id", "")
    assert value, "X-Request-Id must be generated when the caller omits it"
    assert uuid.UUID(value).version == 4


# 2) POST passes If-Match / Idempotency-Key through; never self-generates.

def test_post_passes_if_match_and_idempotency_key(stub):
    client = _client(stub)
    data = client.post(
        tf.task_action("t-1", "complete"),
        payload={"state": "done"},
        if_match='"v7"',
        idempotency_key="idem-1",
    )
    assert data["created"] is True
    assert data["path"] == "/tasks/t-1/actions/complete"
    request = stub.requests[-1]
    assert request["method"] == "POST"
    assert request["headers"].get("If-Match") == '"v7"'
    assert request["headers"].get("Idempotency-Key") == "idem-1"
    assert json.loads(request["body"]) == {"state": "done"}


def test_post_without_optional_headers_sends_neither(stub):
    client = _client(stub)
    client.post(tf.task_sub("t-1", "events"), payload={"n": 1})
    request = stub.requests[-1]
    assert "If-Match" not in request["headers"]
    assert "Idempotency-Key" not in request["headers"]


# 3) 503 -> TaskForgeUnavailable, single attempt (no auto-retry).

def test_503_raises_unavailable_without_retry(stub):
    client = _client(stub)
    attempts_before = len(stub.requests)
    with pytest.raises(tf.TaskForgeUnavailable) as excinfo:
        client.get("/unavailable")
    assert excinfo.value.status == 503
    assert len(stub.requests) == attempts_before + 1, "no automatic retry"


# 4) 409 -> TaskForgeConflict, code retrievable.

def test_409_raises_conflict_with_code(stub):
    client = _client(stub)
    with pytest.raises(tf.TaskForgeConflict) as excinfo:
        client.post("/conflict", payload={"x": 1}, if_match='"v7"')
    error = excinfo.value
    assert error.code == "etag_mismatch"
    assert error.status == 409
    assert isinstance(error, tf.TaskForgeError)


def test_error_hierarchy():
    assert issubclass(tf.TaskForgeUnavailable, tf.TaskForgeError)
    assert issubclass(tf.TaskForgeConflict, tf.TaskForgeError)


# 5) connection refused -> TaskForgeUnavailable.

def test_connection_refused_raises_unavailable():
    client = tf.TaskForgeClient(
        f"http://127.0.0.1:{_free_port()}", token="tok-test", timeout=2.0
    )
    with pytest.raises(tf.TaskForgeUnavailable) as excinfo:
        client.get(tf.TASKS)
    assert excinfo.value.status is None


# 6) timeout -> TaskForgeUnavailable.

def test_timeout_raises_unavailable(silent_port):
    client = tf.TaskForgeClient(
        f"http://127.0.0.1:{silent_port}", token="tok-test", timeout=0.4
    )
    with pytest.raises(tf.TaskForgeUnavailable):
        client.get(tf.LOOPS)


# 7) no local file fallback: nothing written around the calls.

def test_no_local_file_fallback(tmp_path, monkeypatch, stub):
    workdir = tmp_path / "work"
    workdir.mkdir()
    monkeypatch.chdir(workdir)
    client = _client(stub)
    before = _snapshot(workdir)
    client.get(tf.PROJECTS)
    with pytest.raises(tf.TaskForgeUnavailable):
        client.get("/unavailable")
    after = _snapshot(workdir)
    assert before == after == []
    assert not (workdir / "meta.json").exists()
