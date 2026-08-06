from __future__ import annotations

import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest import mock


ROOT = Path(__file__).resolve().parents[4]
MCP_ROOT = ROOT / "plugins" / "teamharness" / "mcp"
sys.path.insert(0, str(MCP_ROOT))

import server  # noqa: E402


class _Response:
    def __enter__(self) -> "_Response":
        return self

    def __exit__(self, *_: object) -> None:
        return None

    def read(self) -> bytes:
        return b'{"event_id":"$assignment"}'


class ManagerTaskflowTest(unittest.TestCase):
    def test_manager_token_precedes_worker_compatibility_token(self) -> None:
        environment = {
            "AGENTTEAMS_AGENT_ROLE": "manager",
            "AGENTTEAMS_MANAGER_MATRIX_TOKEN": "manager-token",
            "AGENTTEAMS_WORKER_MATRIX_TOKEN": "worker-token",
        }
        with mock.patch.dict(os.environ, environment, clear=True):
            self.assertEqual(server._matrix_access_token(), "manager-token")

    def test_manager_can_delegate_with_worker_visible_marker(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            workspace = Path(temp)
            project_dir = workspace / "shared" / "projects" / "project-1"
            project_dir.mkdir(parents=True)
            (project_dir / "meta.json").write_text(
                json.dumps(
                    {
                        "project_id": "project-1",
                        "source_room_id": "!human:matrix.local",
                        "tasks": [
                            {
                                "task_id": "project-1-task-1",
                                "title": "Run the E2E task",
                                "assigned_to": "@worker:matrix.local",
                                "status": "ready",
                            }
                        ],
                    }
                ),
                encoding="utf-8",
            )
            arguments = {
                "workspaceDir": str(workspace),
                "storage": {},
                "action": "delegate_task",
                "role": "manager",
                "payload": {
                    "projectId": "project-1",
                    "taskId": "project-1-task-1",
                    "roomId": "!task:matrix.local",
                    "assignedTo": "@worker:matrix.local",
                    "title": "Run the E2E task",
                    "spec": "Write the result and submit it.",
                },
            }
            environment = {
                "AGENTTEAMS_AGENT_ROLE": "manager",
                "AGENTTEAMS_MATRIX_URL": "http://matrix.local",
                "AGENTTEAMS_MANAGER_MATRIX_TOKEN": "manager-token",
            }
            with (
                mock.patch.dict(os.environ, environment, clear=True),
                mock.patch.object(
                    server,
                    "_validate_assignee_membership",
                    return_value={"ok": True, "member": True},
                ),
                mock.patch.object(server, "_sync_task", return_value=True),
                mock.patch.object(
                    server.urllib.request,
                    "urlopen",
                    return_value=_Response(),
                ) as send,
            ):
                result = server._taskflow(arguments)

        self.assertTrue(result["ok"])
        self.assertEqual(result["task"]["status"], "assigned")
        request = send.call_args.args[0]
        content = json.loads(request.data.decode("utf-8"))
        self.assertIn("TASK_ASSIGNED: project-1-task-1", content["body"])
        self.assertEqual(
            content["m.mentions"]["user_ids"],
            ["@worker:matrix.local"],
        )

    def test_manager_can_check_and_cancel_tasks(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            workspace = Path(temp)
            task_dir = workspace / "shared" / "tasks" / "project-1-task-1"
            task_dir.mkdir(parents=True)
            (task_dir / "meta.json").write_text(
                json.dumps(
                    {
                        "task_id": "project-1-task-1",
                        "project_id": "project-1",
                        "status": "assigned",
                    }
                ),
                encoding="utf-8",
            )
            base = {
                "workspaceDir": str(workspace),
                "storage": {},
                "role": "manager",
            }
            with (
                mock.patch.object(server, "_pull_task", return_value=True),
                mock.patch.object(server, "_sync_task", return_value=True),
            ):
                checked = server._taskflow(
                    {
                        **base,
                        "action": "check_task",
                        "payload": {"taskId": "project-1-task-1"},
                    }
                )
                cancelled = server._taskflow(
                    {
                        **base,
                        "action": "cancel_task",
                        "payload": {
                            "taskId": "project-1-task-1",
                            "reason": "test cleanup",
                        },
                    }
                )

        self.assertTrue(checked["ok"])
        self.assertTrue(cancelled["ok"])
        self.assertEqual(cancelled["task"]["status"], "cancelled")


if __name__ == "__main__":
    unittest.main()
