import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import threading

import pytest

from copaw_worker.controller_report import ControllerReadyReporter


def test_controller_reporter_posts_ready_with_llm_calls(monkeypatch):
    server, requests = _start_server(
        {
            "POST /api/v1/workers/worker-a/ready": (204, None),
        }
    )
    monkeypatch.setenv("AGENTTEAMS_CONTROLLER_URL", f"http://127.0.0.1:{server.server_port}")
    monkeypatch.setenv("AGENTTEAMS_AUTH_TOKEN", "worker-token")

    try:
        reporter = ControllerReadyReporter.from_env("worker-a")
        assert reporter.report_ready(7) is True
    finally:
        server.shutdown()
        server.server_close()

    request = requests[0]
    headers = {key.lower(): value for key, value in request["headers"].items()}
    assert request["method"] == "POST"
    assert request["path"] == "/api/v1/workers/worker-a/ready"
    assert headers["authorization"] == "Bearer worker-token"
    assert json.loads(request["body"]) == {"llmCallsSinceLastHeartbeat": 7}


def test_controller_reporter_empty_body_when_llm_calls_omitted(monkeypatch):
    server, requests = _start_server(
        {
            "POST /api/v1/workers/worker-a/ready": (204, None),
        }
    )
    monkeypatch.setenv("AGENTTEAMS_CONTROLLER_URL", f"http://127.0.0.1:{server.server_port}")

    try:
        reporter = ControllerReadyReporter.from_env("worker-a")
        assert reporter.report_ready(None) is True
    finally:
        server.shutdown()
        server.server_close()

    request = requests[0]
    assert request["body"] == ""


def _start_server(routes):
    requests = []

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, format, *args):
            return

        def do_POST(self):
            length = int(self.headers.get("Content-Length", "0"))
            body = self.rfile.read(length).decode("utf-8") if length else ""
            requests.append(
                {
                    "method": "POST",
                    "path": self.path,
                    "headers": dict(self.headers.items()),
                    "body": body,
                }
            )
            route = routes.get(f"POST {self.path}")
            if route is None:
                self.send_response(404)
                self.end_headers()
                return
            status, payload = route
            self.send_response(status)
            self.end_headers()
            if payload is not None:
                self.wfile.write(json.dumps(payload).encode("utf-8"))

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server, requests
