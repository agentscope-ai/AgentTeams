from __future__ import annotations

from pathlib import Path
import sys
import tempfile
import textwrap
import unittest


ROOT = Path(__file__).resolve().parents[5]
RUNTIME_ROOT = ROOT / "plugins" / "teamharness" / "remote" / "codex-cli"
sys.path.insert(0, str(RUNTIME_ROOT))

from agentteams_codex_worker.codex_client import (  # noqa: E402
    CodexAppServer,
    CodexPermissionDenied,
    CodexTimeout,
)


FAKE_SERVER = r'''
import json
import sys

thread_id = "thread-1"
pending = None

def send(value):
    print(json.dumps(value), flush=True)

for raw in sys.stdin:
    message = json.loads(raw)
    method = message.get("method")
    request_id = message.get("id")
    if method == "initialize":
        send({"id": request_id, "result": {"userAgent": "fake"}})
    elif method == "initialized":
        pass
    elif method == "thread/start":
        send({"id": request_id, "result": {"thread": {"id": thread_id}}})
    elif method == "thread/resume":
        thread_id = message["params"]["threadId"]
        send({"id": request_id, "result": {"thread": {"id": thread_id}}})
    elif method == "turn/start":
        pending = (thread_id, "turn-1")
        send({"id": request_id, "result": {"turn": {"id": "turn-1"}}})
        send({
            "id": 900,
            "method": "item/commandExecution/requestApproval",
            "params": {"threadId": thread_id, "turnId": "turn-1", "itemId": "cmd-1", "startedAtMs": 1},
        })
    elif request_id == 900:
        if message.get("result", {}).get("decision") != "decline":
            raise SystemExit("approval was not denied")
        send({
            "method": "item/agentMessage/delta",
            "params": {
                "threadId": pending[0],
                "turnId": pending[1],
                "itemId": "msg-1",
                "delta": "done",
            },
        })
        send({
            "method": "turn/completed",
            "params": {
                "threadId": pending[0],
                "turn": {"id": pending[1], "status": "completed", "items": []},
            },
        })
        pending = None
    elif method == "turn/interrupt":
        send({"id": request_id, "result": {}})
'''


HANG_SERVER = r'''
import json
import sys

def send(value):
    print(json.dumps(value), flush=True)

for raw in sys.stdin:
    message = json.loads(raw)
    method = message.get("method")
    request_id = message.get("id")
    if method == "initialize":
        send({"id": request_id, "result": {}})
    elif method == "thread/start":
        send({"id": request_id, "result": {"thread": {"id": "thread-hang"}}})
    elif method == "turn/start":
        send({"id": request_id, "result": {"turn": {"id": "turn-hang"}}})
    elif method == "turn/interrupt":
        send({"id": request_id, "result": {}})
'''


PERMISSION_SERVER = r'''
import json
import sys

def send(value):
    print(json.dumps(value), flush=True)

for raw in sys.stdin:
    message = json.loads(raw)
    method = message.get("method")
    request_id = message.get("id")
    if method == "initialize":
        send({"id": request_id, "result": {}})
    elif method == "thread/start":
        send({"id": request_id, "result": {"thread": {"id": "thread-permission"}}})
    elif method == "turn/start":
        send({"id": request_id, "result": {"turn": {"id": "turn-permission"}}})
        send({
            "id": 901,
            "method": "item/permissions/requestApproval",
            "params": {
                "threadId": "thread-permission",
                "turnId": "turn-permission",
                "permissions": {"fileSystem": {"write": ["C:/outside"]}},
            },
        })
    elif request_id == 901:
        if message.get("result", {}).get("permissions") != {}:
            raise SystemExit("permissions were granted")
        send({
            "method": "turn/completed",
            "params": {
                "threadId": "thread-permission",
                "turn": {
                    "id": "turn-permission",
                    "status": "completed",
                    "items": [],
                },
            },
        })
'''


class CodexClientTest(unittest.TestCase):
    def _script(self, directory: Path, body: str) -> Path:
        path = directory / "fake_app_server.py"
        path.write_text(textwrap.dedent(body).lstrip(), encoding="utf-8")
        return path

    def test_streams_output_denies_approval_and_resumes_thread(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            script = self._script(root, FAKE_SERVER)
            client = CodexAppServer(
                launch_command=[sys.executable, str(script)],
                handshake_timeout=2,
                turn_timeout=2,
            )
            ready_threads: list[str] = []
            try:
                first = client.execute(
                    prompt="first",
                    workspace=root,
                    on_thread_ready=ready_threads.append,
                )
                second = client.execute(
                    prompt="second", workspace=root, prior_thread_id=first.thread_id
                )
            finally:
                client.close()

            self.assertEqual(first.output, "done")
            self.assertEqual(ready_threads, [first.thread_id])
            self.assertEqual(second.thread_id, first.thread_id)
            self.assertEqual(second.status, "completed")
            self.assertEqual(second.approvals_denied, 2)

    def test_turn_timeout_interrupts_hung_turn(self) -> None:
        interrupts: list[tuple[str, str]] = []

        class RecordingClient(CodexAppServer):
            def interrupt(self, thread_id: str, turn_id: str) -> None:
                interrupts.append((thread_id, turn_id))
                super().interrupt(thread_id, turn_id)

        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            script = self._script(root, HANG_SERVER)
            client = RecordingClient(
                launch_command=[sys.executable, str(script)],
                handshake_timeout=1,
                turn_timeout=0.15,
            )
            try:
                with self.assertRaises(CodexTimeout):
                    client.execute(prompt="hang", workspace=root)
            finally:
                client.close()

        self.assertEqual(interrupts, [("thread-hang", "turn-hang")])

    def test_workspace_permission_expansion_is_blocked(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            script = self._script(root, PERMISSION_SERVER)
            client = CodexAppServer(
                launch_command=[sys.executable, str(script)],
                handshake_timeout=1,
                turn_timeout=1,
            )
            try:
                with self.assertRaises(CodexPermissionDenied):
                    client.execute(prompt="write outside", workspace=root)
            finally:
                client.close()


if __name__ == "__main__":
    unittest.main()
