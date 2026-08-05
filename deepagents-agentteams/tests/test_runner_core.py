import hashlib
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import MagicMock, patch

from deepagents_agentteams.runner_core import (
    InvalidWorkspacePath,
    RunnerService,
    UnknownExecutionResult,
)


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

    def test_caps_command_output_without_stopping_the_command(self) -> None:
        with tempfile.TemporaryDirectory() as workspace, tempfile.TemporaryDirectory() as state:
            service = RunnerService(
                workspace=Path(workspace),
                state_dir=Path(state),
                max_output_bytes=32,
            )

            result = service.execute(
                request_id="req-large-output",
                command="head -c 128 /dev/zero | tr '\\0' x",
                timeout_seconds=5,
            )

            self.assertEqual(result.output, "x" * 32)
            self.assertTrue(result.truncated)
            self.assertEqual(result.exit_code, 0)

    def test_timeout_is_a_terminal_idempotent_result(self) -> None:
        with tempfile.TemporaryDirectory() as workspace, tempfile.TemporaryDirectory() as state:
            service = RunnerService(workspace=Path(workspace), state_dir=Path(state))

            first = service.execute(
                request_id="req-timeout",
                command="sleep 5",
                timeout_seconds=1,
            )
            second = service.execute(
                request_id="req-timeout",
                command="printf should-not-run",
                timeout_seconds=1,
            )

            self.assertEqual(first, second)
            self.assertEqual(first.exit_code, 124)
            self.assertIn("timed out after 1 seconds", first.output)

    def test_upload_and_download_stay_under_workspace(self) -> None:
        with tempfile.TemporaryDirectory() as workspace, tempfile.TemporaryDirectory() as state:
            service = RunnerService(workspace=Path(workspace), state_dir=Path(state))

            service.upload_file(path="/workspace/nested/input.txt", content=b"safe")

            self.assertEqual(service.download_file(path="/workspace/nested/input.txt"), b"safe")
            self.assertEqual(Path(workspace, "nested/input.txt").read_bytes(), b"safe")

    def test_file_operations_reject_traversal_and_external_absolute_paths(self) -> None:
        with tempfile.TemporaryDirectory() as workspace, tempfile.TemporaryDirectory() as state:
            service = RunnerService(workspace=Path(workspace), state_dir=Path(state))

            for path in ("../escape.txt", "/etc/passwd", "/workspace/../escape.txt"):
                with self.subTest(path=path), self.assertRaises(InvalidWorkspacePath):
                    service.upload_file(path=path, content=b"unsafe")

    def test_command_environment_does_not_inherit_runner_or_platform_secrets(self) -> None:
        with tempfile.TemporaryDirectory() as workspace, tempfile.TemporaryDirectory() as state:
            service = RunnerService(workspace=Path(workspace), state_dir=Path(state))
            with patch.dict(
                "os.environ",
                {
                    "AGENTTEAMS_RUNNER_TOKEN": "runner-secret",
                    "AGENTTEAMS_WORKER_GATEWAY_KEY": "gateway-secret",
                },
                clear=False,
            ):
                result = service.execute(
                    request_id="req-sanitized-env",
                    command="env",
                    timeout_seconds=5,
                )

            self.assertNotIn("runner-secret", result.output)
            self.assertNotIn("gateway-secret", result.output)
            self.assertNotIn("AGENTTEAMS_RUNNER_TOKEN", result.output)

    def test_command_process_closes_descriptors_and_starts_a_process_group(self) -> None:
        process = MagicMock(pid=12345, returncode=0)
        with tempfile.TemporaryDirectory() as workspace, tempfile.TemporaryDirectory() as state:
            service = RunnerService(workspace=Path(workspace), state_dir=Path(state))
            with patch("deepagents_agentteams.runner_core.subprocess.Popen", return_value=process) as popen:
                service.execute(request_id="req-process-flags", command="true", timeout_seconds=5)

        _, kwargs = popen.call_args
        self.assertTrue(kwargs["close_fds"])
        self.assertTrue(kwargs["start_new_session"])


if __name__ == "__main__":
    unittest.main()
