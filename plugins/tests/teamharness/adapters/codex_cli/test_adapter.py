from __future__ import annotations

import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[5]
INSTALL = ROOT / "plugins" / "teamharness" / "adapters" / "codex-cli" / "install.sh"
UNINSTALL = ROOT / "plugins" / "teamharness" / "adapters" / "codex-cli" / "uninstall.sh"


def _bash_command() -> str | None:
    if os.name != "nt":
        return "bash"
    for candidate in (
        Path(os.environ.get("ProgramFiles", "C:/Program Files")) / "Git" / "bin" / "bash.exe",
        Path(os.environ.get("ProgramFiles", "C:/Program Files")) / "Git" / "usr" / "bin" / "bash.exe",
    ):
        if candidate.is_file():
            return str(candidate)
    return None


def _shell_path(path: Path) -> str:
    return path.as_posix() if os.name == "nt" else str(path)


class CodexAdapterTest(unittest.TestCase):
    def test_install_and_uninstall_write_only_non_secret_marker(self) -> None:
        bash = _bash_command()
        if not bash:
            self.skipTest("Bash is not available")
        with tempfile.TemporaryDirectory() as temp:
            project = Path(temp)
            binary = project / "bin"
            binary.mkdir()
            fake = binary / "codex"
            fake.write_text("#!/usr/bin/env sh\nexit 0\n", encoding="utf-8")
            fake.chmod(0o755)
            env = {
                **os.environ,
                "PATH": f"{_shell_path(binary)}:{os.environ.get('PATH', '')}",
                "AGENTTEAMS_PROJECT_DIR": _shell_path(project),
                "AGENTTEAMS_PLUGIN_DIR": _shell_path(ROOT / "plugins" / "teamharness"),
            }

            installed = subprocess.run(
                [bash, _shell_path(INSTALL)],
                env=env,
                text=True,
                encoding="utf-8",
                errors="replace",
                capture_output=True,
                check=False,
            )
            self.assertEqual(installed.returncode, 0, installed.stderr + installed.stdout)
            marker = project / ".agentteams" / "codex-cli" / "adapter.json"
            data = json.loads(marker.read_text(encoding="utf-8"))
            self.assertEqual(data["adapter"], "codex-cli")
            self.assertFalse(data["credentialsStored"])
            self.assertNotIn("token", marker.read_text(encoding="utf-8").lower())

            removed = subprocess.run(
                [bash, _shell_path(UNINSTALL)],
                env=env,
                text=True,
                encoding="utf-8",
                errors="replace",
                capture_output=True,
                check=False,
            )
            self.assertEqual(removed.returncode, 0, removed.stderr + removed.stdout)
            self.assertFalse(marker.exists())


if __name__ == "__main__":
    unittest.main()
