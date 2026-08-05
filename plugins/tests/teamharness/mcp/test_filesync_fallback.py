from __future__ import annotations

import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest import mock


ROOT = Path(__file__).resolve().parents[4]
MCP_ROOT = ROOT / "plugins" / "teamharness" / "mcp"
sys.path.insert(0, str(MCP_ROOT))

import server  # noqa: E402


class FilesyncFallbackTest(unittest.TestCase):
    def test_windows_directory_pull_uses_recursive_copy(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            local = Path(temp) / "shared" / "tasks" / "task-1"
            command = server._filesync_directory_pull_command(
                "mock/shared/tasks/task-1/",
                local,
                windows=True,
            )
        self.assertEqual(
            command,
            [
                "mc",
                "cp",
                "--recursive",
                "mock/shared/tasks/task-1/",
                str(local),
            ],
        )

    def test_posix_directory_pull_keeps_mirror(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            command = server._filesync_directory_pull_command(
                "mock/shared/tasks/task-1/",
                Path(temp) / "shared" / "tasks" / "task-1",
                windows=False,
            )
        self.assertEqual(command[0:2], ["mc", "mirror"])

    def test_windows_unfiltered_directory_push_uses_recursive_copy(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            local = Path(temp) / "shared" / "tasks" / "task-1"
            command = server._filesync_directory_push_command(
                local,
                "mock/shared/tasks/task-1/",
                [],
                windows=True,
            )
        self.assertEqual(command[0:3], ["mc", "cp", "--recursive"])
        self.assertEqual(command[3], str(local) + "/")
        self.assertEqual(command[4:], ["mock/shared/tasks/"])

    def test_posix_directory_push_keeps_mirror(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            command = server._filesync_directory_push_command(
                Path(temp) / "shared" / "tasks" / "task-1",
                "mock/shared/tasks/task-1/",
                [],
                windows=False,
            )
        self.assertEqual(command[0:2], ["mc", "mirror"])

    def test_windows_filtered_push_uses_file_level_copies(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            local = Path(temp) / "shared" / "tasks" / "task-1"
            (local / "base").mkdir(parents=True)
            (local / "meta.json").write_text("{}", encoding="utf-8")
            (local / "result.md").write_text("done", encoding="utf-8")
            (local / "spec.md").write_text("spec", encoding="utf-8")
            (local / "base" / "old.txt").write_text("old", encoding="utf-8")

            commands = server._filesync_windows_filtered_push_commands(
                local,
                "mock/shared/tasks/task-1/",
                ["spec.md", "base/"],
            )

        self.assertEqual(len(commands), 2)
        self.assertEqual(
            [command[-1] for command in commands],
            [
                "mock/shared/tasks/task-1/meta.json",
                "mock/shared/tasks/task-1/result.md",
            ],
        )
        self.assertTrue(all(command[:2] == ["mc", "cp"] for command in commands))

    def test_missing_mc_returns_structured_error_without_stopping_server(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            arguments = {
                "action": "pull",
                "path": "shared/tasks/task-1/result.md",
                "workspaceDir": temp,
                "storage": {"sharedPrefix": "mock/shared"},
            }
            with mock.patch.object(
                server.subprocess,
                "run",
                side_effect=FileNotFoundError("mc"),
            ):
                result = server.call_tool("filesync", arguments)

        payload = json.loads(result["content"][0]["text"])
        self.assertFalse(payload["ok"])
        self.assertEqual(payload["error"], "mc command not found")
        self.assertEqual(payload["action"], "pull")


if __name__ == "__main__":
    unittest.main()
