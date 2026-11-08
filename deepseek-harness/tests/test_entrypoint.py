import os
from pathlib import Path
import subprocess
import unittest


ENTRYPOINT = Path(__file__).resolve().parents[1] / "scripts" / "deepseek-harness-worker-entrypoint.sh"
LAUNCHER = Path(__file__).resolve().parents[1] / "scripts" / "agentteams-dsh"


@unittest.skipIf(os.name == "nt", "requires POSIX bash")
class DeepSeekHarnessEntrypointTest(unittest.TestCase):
    def test_e2ee_configuration_fails_before_worker_startup(self) -> None:
        completed = subprocess.run(
            ["bash", str(ENTRYPOINT)],
            env={**os.environ, "AGENTTEAMS_MATRIX_E2EE": "1"},
            text=True,
            capture_output=True,
            timeout=10,
        )

        self.assertNotEqual(completed.returncode, 0)
        self.assertIn(
            "DeepSeek Harness does not support Matrix E2EE",
            completed.stdout + completed.stderr,
        )

    def test_e2ee_true_configuration_fails_before_worker_startup(self) -> None:
        completed = subprocess.run(
            ["bash", str(ENTRYPOINT)],
            env={**os.environ, "AGENTTEAMS_MATRIX_E2EE": "true"},
            text=True,
            capture_output=True,
            timeout=10,
        )

        self.assertNotEqual(completed.returncode, 0)
        self.assertIn(
            "DeepSeek Harness does not support Matrix E2EE",
            completed.stdout + completed.stderr,
        )

    def test_missing_llm_credential_fails_before_worker_startup(self) -> None:
        environment = {
            key: value
            for key, value in os.environ.items()
            if key not in {"DEEPSEEK_API_KEY", "AGENTTEAMS_WORKER_GATEWAY_KEY"}
        }
        completed = subprocess.run(
            ["bash", str(ENTRYPOINT)],
            env=environment,
            text=True,
            capture_output=True,
            timeout=10,
        )

        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("no LLM credential available", completed.stdout + completed.stderr)


class DeepSeekHarnessLauncherTest(unittest.TestCase):
    def test_extensionless_shell_launcher_uses_lf_line_endings(self) -> None:
        self.assertNotIn(b"\r\n", LAUNCHER.read_bytes())


if __name__ == "__main__":
    unittest.main()
