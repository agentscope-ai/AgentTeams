from __future__ import annotations

from pathlib import Path
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[5]
RUNTIME_ROOT = ROOT / "plugins" / "teamharness" / "remote" / "codex-cli"
TEAMHARNESS_ROOT = ROOT / "plugins" / "teamharness"
sys.path.insert(0, str(RUNTIME_ROOT))

from agentteams_codex_worker.codex_client import ExecutionResult  # noqa: E402
from agentteams_codex_worker.config import RuntimeConfig  # noqa: E402
from agentteams_codex_worker.matrix import AssignedTask  # noqa: E402
from agentteams_codex_worker.security import Redactor  # noqa: E402
from agentteams_codex_worker.worker import CodexWorkerBridge, StateStore  # noqa: E402


class FakeMatrix:
    def __init__(self) -> None:
        self.sent: list[tuple[str, str, str]] = []

    def send_text(self, room_id: str, text: str, *, transaction_id: str = "") -> str:
        self.sent.append((room_id, text, transaction_id))
        return "$reply"


class FakeCodex:
    def __init__(self) -> None:
        self.calls = 0

    def execute(self, **kwargs: object) -> ExecutionResult:
        self.calls += 1
        prior = str(kwargs.get("prior_thread_id") or "")
        thread_id = prior or "thread-1"
        callback = kwargs.get("on_thread_ready")
        if callable(callback):
            callback(thread_id)
        return ExecutionResult(thread_id, "turn-1", "completed", "TASK_COMPLETED: task-1", 0)

    def close(self) -> None:
        return


class WorkerBridgeTest(unittest.TestCase):
    def test_legacy_threads_migrate_to_shared_session_journal(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            state_dir = Path(temp) / "state"
            state_dir.mkdir()
            (state_dir / "state.json").write_text(
                '{"since":"cursor","threads":{"task-old":"thread-old"},"seenEvents":[]}\n',
                encoding="utf-8",
            )
            state = StateStore(state_dir)
            self.assertEqual(state.thread_for("task-old"), "thread-old")
            self.assertNotIn(
                "threads",
                (state_dir / "state.json").read_text(encoding="utf-8"),
            )
            self.assertIn(
                "thread-old",
                (state_dir / "sessions.json").read_text(encoding="utf-8"),
            )

    def test_executes_event_once_and_persists_only_non_secret_state(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            matrix = FakeMatrix()
            codex = FakeCodex()
            state = StateStore(root / "state")
            config = RuntimeConfig(
                team_name="demo",
                member_name="codex-worker",
                matrix_user_id="@codex:matrix.local",
                personal_room_id="",
                team_room_id="!team:matrix.local",
                leader_matrix_user_id="@leader:matrix.local",
                model="",
                matrix_token_env="AGENTTEAMS_WORKER_MATRIX_TOKEN",
            )
            bridge = CodexWorkerBridge(
                config=config,
                workspace=root,
                plugin_root=TEAMHARNESS_ROOT,
                matrix=matrix,  # type: ignore[arg-type]
                codex=codex,  # type: ignore[arg-type]
                state=state,
                redactor=Redactor(["super-secret-token"]),
            )
            task = AssignedTask(
                "$event",
                "!task:matrix.local",
                "@leader:matrix.local",
                "task-1",
                "@codex:matrix.local TASK_ASSIGNED: task-1",
            )

            result = bridge.process_task(task)
            duplicate = bridge.process_task(task)

            self.assertIsNotNone(result)
            self.assertIsNone(duplicate)
            self.assertEqual(codex.calls, 1)
            self.assertEqual(len(matrix.sent), 1)
            self.assertEqual(state.thread_for("task-1"), "thread-1")
            state_text = state.path.read_text(encoding="utf-8")
            self.assertNotIn("super-secret-token", state_text)
            self.assertNotIn("accessToken", state_text)

    def test_thread_is_persisted_before_turn_completion(self) -> None:
        class FailingCodex(FakeCodex):
            def execute(self, **kwargs: object) -> ExecutionResult:
                callback = kwargs.get("on_thread_ready")
                if callable(callback):
                    callback("thread-before-failure")
                raise OSError("simulated process exit")

        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            matrix = FakeMatrix()
            state = StateStore(root / "state")
            config = RuntimeConfig(
                team_name="demo",
                member_name="codex-worker",
                matrix_user_id="@codex:matrix.local",
                personal_room_id="",
                team_room_id="!team:matrix.local",
                leader_matrix_user_id="@leader:matrix.local",
                model="",
                matrix_token_env="AGENTTEAMS_WORKER_MATRIX_TOKEN",
            )
            bridge = CodexWorkerBridge(
                config=config,
                workspace=root,
                plugin_root=TEAMHARNESS_ROOT,
                matrix=matrix,  # type: ignore[arg-type]
                codex=FailingCodex(),  # type: ignore[arg-type]
                state=state,
                redactor=Redactor(),
            )
            task = AssignedTask(
                "$failure",
                "!task:matrix.local",
                "@leader:matrix.local",
                "task-2",
                "@codex:matrix.local TASK_ASSIGNED: task-2",
            )

            bridge.process_task(task)

            self.assertEqual(state.thread_for("task-2"), "thread-before-failure")
            self.assertTrue(state.has_seen("$failure"))
            self.assertIn("BLOCKED: task-2", matrix.sent[0][1])


if __name__ == "__main__":
    unittest.main()
