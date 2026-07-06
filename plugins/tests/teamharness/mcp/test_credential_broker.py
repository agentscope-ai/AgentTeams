from __future__ import annotations

import json
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any


REPO_ROOT = Path(__file__).resolve().parents[4]
MCP_SRC = REPO_ROOT / "plugins" / "teamharness" / "mcp"

if str(MCP_SRC) not in sys.path:
    sys.path.insert(0, str(MCP_SRC))

import server as mcp_server
from server import call_tool


def test_message_uses_credential_broker_matrix_credentials(tmp_path: Path, monkeypatch: Any) -> None:
    captured: dict[str, Any] = {}

    class MatrixHandler(BaseHTTPRequestHandler):
        def log_message(self, _format: str, *_args: object) -> None:
            return

        def do_PUT(self) -> None:
            length = int(self.headers.get("Content-Length", "0"))
            captured["path"] = self.path
            captured["auth"] = self.headers.get("Authorization")
            captured["body"] = json.loads(self.rfile.read(length).decode("utf-8") or "{}")
            body = json.dumps({"event_id": "$event1"}).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    matrix = ThreadingHTTPServer(("127.0.0.1", 0), MatrixHandler)
    matrix_thread = threading.Thread(target=matrix.serve_forever, daemon=True)
    matrix_thread.start()

    class BrokerHandler(BaseHTTPRequestHandler):
        def log_message(self, _format: str, *_args: object) -> None:
            return

        def do_GET(self) -> None:
            if self.headers.get("Authorization") != "Bearer local-capability":
                self.send_response(401)
                self.end_headers()
                return
            if self.path != "/v1/credentials/matrix":
                self.send_response(404)
                self.end_headers()
                return
            body = json.dumps(
                {
                    "homeserver": f"http://127.0.0.1:{matrix.server_port}",
                    "accessToken": "broker-matrix-token",
                    "userId": "@worker:example.test",
                    "teamRoomId": "!team:example.test",
                    "rooms": [{"id": "!team:example.test", "kind": "team"}],
                }
            ).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    broker = ThreadingHTTPServer(("127.0.0.1", 0), BrokerHandler)
    broker_thread = threading.Thread(target=broker.serve_forever, daemon=True)
    broker_thread.start()

    token_file = tmp_path / "credential-token"
    token_file.write_text("local-capability\n", encoding="utf-8")
    descriptor = tmp_path / "credential-broker.json"
    descriptor.write_text(
        json.dumps(
            {
                "version": 1,
                "endpoint": f"http://127.0.0.1:{broker.server_port}",
                "tokenFile": str(token_file),
                "workerPid": 123,
                "workerUuid": "worker-uuid-1",
            }
        ),
        encoding="utf-8",
    )
    monkeypatch.setenv("TEAMHARNESS_CREDENTIAL_BROKER_DESCRIPTOR", str(descriptor))
    monkeypatch.delenv("HICLAW_MATRIX_URL", raising=False)
    monkeypatch.delenv("HICLAW_WORKER_MATRIX_TOKEN", raising=False)

    try:
        result = call_tool(
            "message",
            {
                "action": "send",
                "channel": "matrix",
                "target": "room:!room:example.test",
                "text": "Broker path is ready.",
            },
        )
    finally:
        broker.shutdown()
        matrix.shutdown()
        broker_thread.join(timeout=3)
        matrix_thread.join(timeout=3)

    payload = json.loads(result["content"][0]["text"])
    assert payload["ok"] is True
    assert payload["messageId"] == "$event1"
    assert captured["auth"] == "Bearer broker-matrix-token"
    assert "/_matrix/client/v3/rooms/%21room%3Aexample.test/send/m.room.message/" in captured["path"]
    assert captured["body"]["body"] == "Broker path is ready."


def test_filesync_uses_credential_broker_storage_credentials(tmp_path: Path, monkeypatch: Any) -> None:
    oss_root = tmp_path / "oss"
    bucket = "hiclaw-demo"
    remote_file = oss_root / bucket / "shared/tasks/t-001/result.md"
    remote_file.parent.mkdir(parents=True)
    remote_file.write_text("# Remote Result\n", encoding="utf-8")
    remote_global_file = oss_root / bucket / "global/shared/docs/site/index.md"
    remote_global_file.parent.mkdir(parents=True)
    remote_global_file.write_text("# Global Result\n", encoding="utf-8")

    class BrokerHandler(BaseHTTPRequestHandler):
        def log_message(self, _format: str, *_args: object) -> None:
            return

        def _json(self, status: int, payload: dict[str, Any]) -> None:
            body = json.dumps(payload).encode("utf-8")
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_GET(self) -> None:
            if self.headers.get("Authorization") != "Bearer local-capability":
                self._json(401, {"error": "unauthorized"})
                return
            if self.path == "/v1/runtime/context":
                self._json(
                    200,
                    {
                        "version": 1,
                        "storage": {
                            "provider": "oss",
                            "sharedPrefix": "shared",
                            "globalSharedPrefix": "global/shared",
                            "memberPrefix": "agents/claude-dev",
                        },
                    },
                )
                return
            if self.path == "/v1/credentials/storage":
                self._json(
                    200,
                    {
                        "provider": "oss",
                        "endpoint": f"file://{oss_root}",
                        "bucket": bucket,
                        "accessKeyId": "sts-ak",
                        "accessKeySecret": "sts-sk",
                        "securityToken": "sts-token",
                        "expiration": "2026-06-10T12:00:00Z",
                        "expiresInSec": 3600,
                    },
                )
                return
            self._json(404, {"error": "not found"})

    broker = ThreadingHTTPServer(("127.0.0.1", 0), BrokerHandler)
    broker_thread = threading.Thread(target=broker.serve_forever, daemon=True)
    broker_thread.start()

    token_file = tmp_path / "credential-token"
    token_file.write_text("local-capability\n", encoding="utf-8")
    descriptor = tmp_path / "credential-broker.json"
    descriptor.write_text(
        json.dumps(
            {
                "version": 1,
                "endpoint": f"http://127.0.0.1:{broker.server_port}",
                "tokenFile": str(token_file),
                "workerPid": 123,
                "workerUuid": "worker-uuid-1",
            }
        ),
        encoding="utf-8",
    )
    monkeypatch.setenv("TEAMHARNESS_CREDENTIAL_BROKER_DESCRIPTOR", str(descriptor))
    for name in ("HICLAW_FS_ENDPOINT", "HICLAW_FS_ACCESS_KEY", "HICLAW_FS_SECRET_KEY", "HICLAW_STORAGE_PREFIX"):
        monkeypatch.delenv(name, raising=False)

    workspace = tmp_path / "workspace"

    def payload(args: dict[str, Any]) -> dict[str, Any]:
        result = call_tool("filesync", {"workspaceDir": str(workspace), **args})
        data = json.loads(result["content"][0]["text"])
        assert isinstance(data, dict)
        return data

    try:
        pulled = payload({"action": "pull", "path": "shared/tasks/t-001/result.md"})
        assert pulled["ok"] is True
        assert pulled["backend"] == "oss-sdk"
        assert (workspace / "shared/tasks/t-001/result.md").read_text(encoding="utf-8") == "# Remote Result\n"

        listed = payload({"action": "list", "path": "shared/tasks/t-001"})
        assert listed["ok"] is True
        assert listed["entries"] == ["shared/tasks/t-001/result.md"]

        stat = payload({"action": "stat", "path": "shared/tasks/t-001/result.md"})
        assert stat["ok"] is True
        assert stat["exists"] is True

        pulled_global = payload({"action": "pull", "path": "global-shared/docs/site/index.md"})
        assert pulled_global["ok"] is True
        assert (workspace / "global-shared/docs/site/index.md").read_text(encoding="utf-8") == "# Global Result\n"

        local_new = workspace / "shared/tasks/t-002/result.md"
        local_new.parent.mkdir(parents=True)
        local_new.write_text("# Local Result\n", encoding="utf-8")
        pushed = payload({"action": "push", "path": "shared/tasks/t-002/result.md"})
        assert pushed["ok"] is True
        assert pushed["backend"] == "oss-sdk"
        assert (oss_root / bucket / "shared/tasks/t-002/result.md").read_text(encoding="utf-8") == "# Local Result\n"
    finally:
        broker.shutdown()
        broker_thread.join(timeout=3)


def test_filesync_oss_bucket_falls_back_to_public_endpoint(monkeypatch: Any) -> None:
    calls: list[tuple[str, int | None]] = []

    class FakeAuth:
        def __init__(self, access_key: str, secret_key: str, token: str) -> None:
            self.access_key = access_key
            self.secret_key = secret_key
            self.token = token

    class FakeBucket:
        def __init__(
            self,
            _auth: FakeAuth,
            endpoint: str,
            _bucket: str,
            connect_timeout: int | None = None,
        ) -> None:
            calls.append((endpoint, connect_timeout))
            if endpoint == "https://oss-cn-hangzhou-internal.aliyuncs.com":
                raise TimeoutError("internal endpoint unreachable")
            assert endpoint == "https://oss-cn-hangzhou.aliyuncs.com"
            self.endpoint = endpoint

    monkeypatch.setitem(sys.modules, "oss2", type("FakeOss2", (), {"StsAuth": FakeAuth, "Bucket": FakeBucket}))

    bucket = mcp_server._oss_bucket_from_credentials(
        {
            "endpoint": "https://oss-cn-hangzhou-internal.aliyuncs.com",
            "bucket": "hiclaw-demo",
            "accessKeyId": "sts-ak",
            "accessKeySecret": "sts-sk",
            "securityToken": "sts-token",
        }
    )

    assert bucket.endpoint == "https://oss-cn-hangzhou.aliyuncs.com"
    assert calls == [
        ("https://oss-cn-hangzhou-internal.aliyuncs.com", 5),
        ("https://oss-cn-hangzhou.aliyuncs.com", 5),
    ]
