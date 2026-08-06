from __future__ import annotations

from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import sys
import threading
import unittest


RUNTIME_ROOT = Path(__file__).resolve().parents[5] / "plugins" / "teamharness" / "remote" / "codex-cli"
sys.path.insert(0, str(RUNTIME_ROOT))

from agentteams_codex_worker.matrix import MatrixClient, MatrixError  # noqa: E402


class Handler(BaseHTTPRequestHandler):
    sent: list[dict[str, object]] = []
    reject = False

    def do_GET(self) -> None:
        if type(self).reject:
            self.send_response(401)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"error":"secret-token"}')
            return
        payload = {
            "next_batch": "s2",
            "rooms": {
                "join": {
                    "!task:matrix.local": {
                        "timeline": {
                            "events": [
                                {
                                    "type": "m.room.message",
                                    "event_id": "$assigned",
                                    "sender": "@leader:matrix.local",
                                    "content": {
                                        "body": "@codex:matrix.local TASK_ASSIGNED: task-01 fix it",
                                        "m.mentions": {"user_ids": ["@codex:matrix.local"]},
                                    },
                                },
                                {
                                    "type": "m.room.message",
                                    "event_id": "$other",
                                    "sender": "@leader:matrix.local",
                                    "content": {"body": "TASK_ASSIGNED: ignored"},
                                },
                            ]
                        }
                    }
                }
            },
        }
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps(payload).encode())

    def do_PUT(self) -> None:
        length = int(self.headers.get("Content-Length", "0"))
        body = json.loads(self.rfile.read(length))
        type(self).sent.append({"path": self.path, "body": body})
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b'{"event_id":"$reply"}')

    def log_message(self, *_: object) -> None:
        return


class MatrixClientTest(unittest.TestCase):
    def setUp(self) -> None:
        Handler.sent = []
        Handler.reject = False
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.client = MatrixClient(
            f"http://127.0.0.1:{self.server.server_port}", "secret-token", "@codex:matrix.local"
        )

    def tearDown(self) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)

    def test_filters_assignment_and_sends_current_room_reply(self) -> None:
        response = self.client.sync(timeout_ms=0)
        tasks = self.client.assigned_tasks(response)
        self.assertEqual([task.task_id for task in tasks], ["task-01"])

        event_id = self.client.send_text(
            tasks[0].room_id,
            "@leader:matrix.local TASK_COMPLETED: task-01",
            transaction_id="stable-txn",
        )

        self.assertEqual(event_id, "$reply")
        self.assertEqual(len(Handler.sent), 1)
        self.assertEqual(
            Handler.sent[0]["body"]["m.mentions"]["user_ids"],
            ["@leader:matrix.local"],
        )

    def test_http_error_does_not_expose_access_token(self) -> None:
        Handler.reject = True

        with self.assertRaises(MatrixError) as raised:
            self.client.sync(timeout_ms=0)

        self.assertNotIn("secret-token", str(raised.exception))
        self.assertIn("[REDACTED]", str(raised.exception))


if __name__ == "__main__":
    unittest.main()
