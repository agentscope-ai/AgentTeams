from __future__ import annotations

import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[5]
RUNTIME_ROOT = ROOT / "plugins" / "teamharness" / "remote" / "codex-cli"
sys.path.insert(0, str(RUNTIME_ROOT))

from agentteams_codex_runtime.app_server import ExecutionResult  # noqa: E402
from agentteams_codex_runtime.broker_runner import ManagerRunner  # noqa: E402
from agentteams_codex_runtime.journal import SessionJournal  # noqa: E402


class FakeBroker:
    def __init__(self) -> None:
        self.requests = [
            {"executionId": "exec-1", "sessionKey": "room-1", "prompt": "plan it"},
            {"executionId": "exec-2", "sessionKey": "room-1", "prompt": "continue"},
        ]
        self.completions: list[tuple[str, str, str]] = []

    def lease(self):
        return self.requests.pop(0) if self.requests else None

    def complete(self, execution_id, *, output="", error_text=""):
        self.completions.append((execution_id, output, error_text))


class FakeCodex:
    def __init__(self) -> None:
        self.calls = []

    def execute(self, **kwargs):
        self.calls.append(kwargs)
        thread_id = kwargs["prior_thread_id"] or "manager-thread-1"
        kwargs["on_thread_ready"](thread_id)
        return ExecutionResult(thread_id, "turn-1", "completed", "coordinated", 0)

    def close(self):
        pass


class ManagerRunnerTest(unittest.TestCase):
    def test_manager_uses_read_only_sandbox_and_resumes_session(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            broker = FakeBroker()
            codex = FakeCodex()
            runner = ManagerRunner(
                broker=broker,
                codex=codex,
                workspace=root,
                journal=SessionJournal(root / "state"),
            )
            self.assertTrue(runner.run_once())
            self.assertTrue(runner.run_once())

            self.assertEqual(codex.calls[0]["sandbox"], "read-only")
            self.assertEqual(codex.calls[0]["approval_policy"], "never")
            self.assertEqual(codex.calls[0]["prior_thread_id"], "")
            self.assertEqual(codex.calls[1]["prior_thread_id"], "manager-thread-1")
            self.assertEqual(
                broker.completions,
                [("exec-1", "coordinated", ""), ("exec-2", "coordinated", "")],
            )


if __name__ == "__main__":
    unittest.main()
