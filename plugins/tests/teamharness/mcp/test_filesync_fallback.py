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
