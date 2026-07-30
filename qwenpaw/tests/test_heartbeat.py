import asyncio
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import threading

import pytest

from qwenpaw_worker.heartbeat import (
    ControllerHeartbeatReporter,
    WorkerHeartbeat,
    check_qwenpaw_heartbeat,
    get_qwenpaw_last_active_at,
    run_worker_heartbeat_loop,
)


def test_worker_heartbeat_persists_qwenpaw_process_status(tmp_path: Path) -> None:
    heartbeat = WorkerHeartbeat(tmp_path / "heartbeat.json")

    heartbeat.update("not_ready", "qwenpaw app is starting")

    data = json.loads((tmp_path / "heartbeat.json").read_text(encoding="utf-8"))
    assert data["status"] == "not_ready"
    assert data["message"] == "qwenpaw app is starting"
    assert data["details"] == {}

    heartbeat.update("ready", "qwenpaw app reachable", {"url": "http://127.0.0.1:8088/api/version"})

    data = json.loads((tmp_path / "heartbeat.json").read_text(encoding="utf-8"))
    assert data["status"] == "ready"
    assert data["message"] == "qwenpaw app reachable"
    assert data["details"]["url"].endswith("/api/version")


def test_qwenpaw_health_probe_uses_native_version_endpoint() -> None:
    server, requests = _start_server(
        {
            "GET /api/version": (200, {"version": "0.1.0"}),
        }
    )
    try:
        status, message, details = check_qwenpaw_heartbeat(server.server_port)
    finally:
        server.shutdown()
        server.server_close()

    assert status == "ready"
    assert message == "qwenpaw API reachable"
    assert details["url"].endswith("/api/version")
    assert requests[0]["path"] == "/api/version"


def test_qwenpaw_last_active_uses_agent_status_endpoint() -> None:
    server, requests = _start_server(
        {
            "GET /api/agents/default/agent-status": (
                200,
                {
                    "status": "idle",
                    "running_task_count": 0,
                    "last_run_at": "2026-05-13T00:00:00Z",
                    "last_finish_at": "2026-05-13T00:03:00+00:00",
                },
            ),
        }
    )
    try:
        last_active = get_qwenpaw_last_active_at(server.server_port)
    finally:
        server.shutdown()
        server.server_close()

    assert last_active == "2026-05-13T00:03:00Z"
    assert requests[0]["path"] == "/api/agents/default/agent-status"


def test_controller_reporter_posts_ready_with_auth(monkeypatch: pytest.MonkeyPatch) -> None:
    server, requests = _start_server(
        {
            "POST /api/v1/workers/worker-a/ready": (204, None),
        }
    )
    monkeypatch.setenv("AGENTTEAMS_CONTROLLER_URL", f"http://127.0.0.1:{server.server_port}")
    monkeypatch.setenv("AGENTTEAMS_AUTH_TOKEN", "worker-token")

    try:
        reporter = ControllerHeartbeatReporter.from_env("worker-a")
        assert reporter.report_ready("2026-05-13T00:00:00Z") is True
    finally:
        server.shutdown()
        server.server_close()

    request = requests[0]
    headers = {key.lower(): value for key, value in request["headers"].items()}
    assert request["method"] == "POST"
    assert request["path"] == "/api/v1/workers/worker-a/ready"
    assert headers["authorization"] == "Bearer worker-token"
    assert "x-agentteams-cluster-id" not in headers
    assert json.loads(request["body"]) == {"lastActiveAt": "2026-05-13T00:00:00Z"}


def test_controller_reporter_rereads_token_file_for_heartbeat(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    server, requests = _start_server(
        {
            "POST /api/v1/workers/worker-a/heartbeat": (204, None),
        }
    )
    token_file = tmp_path / "token"
    token_file.write_text("file-token\n", encoding="utf-8")
    monkeypatch.delenv("AGENTTEAMS_AUTH_TOKEN", raising=False)
    monkeypatch.setenv("AGENTTEAMS_AUTH_TOKEN_FILE", str(token_file))
    monkeypatch.setenv("AGENTTEAMS_CONTROLLER_URL", f"http://127.0.0.1:{server.server_port}")

    try:
        reporter = ControllerHeartbeatReporter.from_env("worker-a")
        assert reporter.report_heartbeat() is True
        token_file.write_text("file-token-2\n", encoding="utf-8")
        assert reporter.report_heartbeat() is True
    finally:
        server.shutdown()
        server.server_close()

    assert [request["path"] for request in requests] == [
        "/api/v1/workers/worker-a/heartbeat",
        "/api/v1/workers/worker-a/heartbeat",
    ]
    auth_headers = [
        {key.lower(): value for key, value in request["headers"].items()}["authorization"]
        for request in requests
    ]
    assert auth_headers == ["Bearer file-token", "Bearer file-token-2"]
    assert [request["body"] for request in requests] == ["", ""]


def test_controller_reporter_skips_auth_cluster_header(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    server, requests = _start_server(
        {
            "POST /api/v1/workers/worker-a/heartbeat": (204, None),
        }
    )
    monkeypatch.setenv("AGENTTEAMS_CONTROLLER_URL", f"http://127.0.0.1:{server.server_port}")
    monkeypatch.setenv("AGENTTEAMS_AUTH_TOKEN", "worker-token")

    try:
        reporter = ControllerHeartbeatReporter.from_env("worker-a")
        assert reporter.report_heartbeat() is True
    finally:
        server.shutdown()
        server.server_close()

    headers = {key.lower(): value for key, value in requests[0]["headers"].items()}
    assert headers["authorization"] == "Bearer worker-token"
    assert "x-agentteams-cluster-id" not in headers


@pytest.mark.anyio
async def test_worker_heartbeat_loop_reports_ready_and_heartbeat(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    server, requests = _start_server(
        {
            "GET /api/agents/default/agent-status": (
                200,
                {
                    "status": "idle",
                    "running_task_count": 0,
                    "last_run_at": "2026-05-13T00:00:00Z",
                    "last_finish_at": "2026-05-13T00:04:00Z",
                },
            ),
            "POST /api/v1/workers/worker-a/ready": (204, None),
            "POST /api/v1/workers/worker-a/heartbeat": (204, None),
        }
    )
    heartbeat = WorkerHeartbeat(tmp_path / "heartbeat.json")
    ticks = 0

    def check(_port):
        return "ready", "qwenpaw ready", {}

    async def cancel_after_tick(_seconds):
        nonlocal ticks
        ticks += 1
        raise asyncio.CancelledError

    monkeypatch.setenv("AGENTTEAMS_CONTROLLER_URL", f"http://127.0.0.1:{server.server_port}")
    monkeypatch.setattr("qwenpaw_worker.heartbeat.check_qwenpaw_heartbeat", check)
    monkeypatch.setattr("qwenpaw_worker.heartbeat.asyncio.sleep", cancel_after_tick)

    try:
        with pytest.raises(asyncio.CancelledError):
                await run_worker_heartbeat_loop(
                    heartbeat,
                    worker_name="worker-a",
                    port=server.server_port,
                    local_interval=0.01,
                    report_interval=60,
                )
    finally:
        server.shutdown()
        server.server_close()

    data = json.loads((tmp_path / "heartbeat.json").read_text(encoding="utf-8"))
    assert data["status"] == "ready"
    post_requests = [request for request in requests if request["method"] == "POST"]
    assert [request["path"] for request in post_requests] == [
        "/api/v1/workers/worker-a/ready",
        "/api/v1/workers/worker-a/heartbeat",
    ]
    assert [json.loads(request["body"]) for request in post_requests] == [
        {"lastActiveAt": "2026-05-13T00:04:00Z"},
        {"lastActiveAt": "2026-05-13T00:04:00Z"},
    ]


def _start_server(routes):
    requests = []

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            self._handle()

        def do_POST(self):
            self._handle()

        def log_message(self, *_args):
            return

        def _handle(self):
            length = int(self.headers.get("Content-Length", "0") or "0")
            body = self.rfile.read(length).decode("utf-8") if length else ""
            requests.append(
                {
                    "method": self.command,
                    "path": self.path,
                    "headers": dict(self.headers),
                    "body": body,
                }
            )
            status, payload = routes.get(f"{self.command} {self.path}", (404, {"message": "not found"}))
            data = b"" if payload is None else json.dumps(payload).encode("utf-8")
            self.send_response(status)
            if payload is not None:
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            if data:
                self.wfile.write(data)

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server, requests
