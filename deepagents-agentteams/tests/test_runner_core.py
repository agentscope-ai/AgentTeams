import json
import hashlib
import tempfile
import unittest
from pathlib import Path

from deepagents_agentteams.runner_core import RunnerService, UnknownExecutionResult


class RunnerServiceTests(unittest.TestCase):
    def test_repeated_request_id_returns_saved_result_without_reexecution(self) -> None:
        with tempfile.TemporaryDirectory() as workspace, tempfile.TemporaryDirectory() as state:
            service = RunnerService(workspace=Path(workspace), state_dir=Path(state))

            first = service.execute(
                request_id="req-1",
                command="printf x >> count.txt",
                timeout_seconds=5,
            )
            second = service.execute(
                request_id="req-1",
                command="printf x >> count.txt",
                timeout_seconds=5,
            )

            self.assertEqual(first, second)
            self.assertEqual(Path(workspace, "count.txt").read_text(), "x")

    def test_pending_request_is_reported_unknown_and_never_reexecuted(self) -> None:
        with tempfile.TemporaryDirectory() as workspace, tempfile.TemporaryDirectory() as state:
            Path(state, "req-unknown.json").write_text(
                json.dumps({"request_id": "req-unknown", "status": "pending"})
            )
            service = RunnerService(workspace=Path(workspace), state_dir=Path(state))

            with self.assertRaises(UnknownExecutionResult):
                service.execute(
                    request_id="req-unknown",
                    command="printf dangerous > should-not-exist.txt",
                    timeout_seconds=5,
                )

            self.assertFalse(Path(workspace, "should-not-exist.txt").exists())

    def test_rejects_request_id_path_traversal(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            workspace = Path(root, "workspace")
            state = Path(root, "state")
            service = RunnerService(workspace=workspace, state_dir=state)

            with self.assertRaises(ValueError):
                service.execute(
                    request_id="../escape",
                    command="true",
                    timeout_seconds=5,
                )

            self.assertFalse(Path(root, "escape.json").exists())

    def test_returns_changed_and_deleted_workspace_manifest(self) -> None:
        with tempfile.TemporaryDirectory() as workspace, tempfile.TemporaryDirectory() as state:
            Path(workspace, "changed.txt").write_text("old")
            Path(workspace, "deleted.txt").write_text("remove")
            service = RunnerService(workspace=Path(workspace), state_dir=Path(state))

            result = service.execute(
                request_id="req-changes",
                command=(
                    "printf new > changed.txt; "
                    "rm deleted.txt; "
                    "mkdir -p nested; printf added > nested/added.txt"
                ),
                timeout_seconds=5,
            )

            changes = {change.path: change for change in result.changes}
            self.assertEqual(changes["changed.txt"].sha256, hashlib.sha256(b"new").hexdigest())
            self.assertFalse(changes["changed.txt"].deleted)
            self.assertEqual(changes["nested/added.txt"].sha256, hashlib.sha256(b"added").hexdigest())
            self.assertTrue(changes["deleted.txt"].deleted)
            self.assertIsNone(changes["deleted.txt"].sha256)


if __name__ == "__main__":
    unittest.main()
