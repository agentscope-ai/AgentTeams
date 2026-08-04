from __future__ import annotations

import sys
from pathlib import Path
import tempfile
import unittest


RUNTIME_ROOT = Path(__file__).resolve().parents[5] / "plugins" / "teamharness" / "remote" / "codex-cli"
sys.path.insert(0, str(RUNTIME_ROOT))

from agentteams_codex_worker.config import ConfigError, RuntimeConfig  # noqa: E402


class RuntimeConfigTest(unittest.TestCase):
    def test_loads_member_runtime_yaml_without_inline_secrets(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "runtime.yaml"
            path.write_text(
                """
team:
  name: demo
  teamRoomId: "!team:matrix.local"
  leaderMatrixUserId: "@leader:matrix.local"
member:
  name: codex-worker
  matrixUserId: "@codex-worker:matrix.local"
desired:
  model:
    model: gpt-5.1-codex
credentials:
  matrixTokenEnv: AGENTTEAMS_WORKER_MATRIX_TOKEN
""".lstrip(),
                encoding="utf-8",
            )

            config = RuntimeConfig.from_path(path)

            self.assertEqual(config.team_name, "demo")
            self.assertEqual(config.member_name, "codex-worker")
            self.assertEqual(config.leader_matrix_user_id, "@leader:matrix.local")
            self.assertEqual(config.model, "gpt-5.1-codex")

    def test_rejects_inline_token(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "runtime.json"
            path.write_text(
                '{"team":{"name":"demo"},'
                '"member":{"name":"worker","matrixUserId":"@w:m"},'
                '"accessToken":"secret-value-123"}',
                encoding="utf-8",
            )
            with self.assertRaises(ConfigError):
                RuntimeConfig.from_path(path)


if __name__ == "__main__":
    unittest.main()
