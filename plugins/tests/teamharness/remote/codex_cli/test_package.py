from __future__ import annotations

import hashlib
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[5]
SCRIPT = (
    ROOT
    / "plugins"
    / "teamharness"
    / "remote"
    / "codex-cli"
    / "scripts"
    / "package-runtime.py"
)


class RuntimePackageTest(unittest.TestCase):
    def _build(self, output: Path) -> Path:
        result = subprocess.run(
            [sys.executable, str(SCRIPT), "--output", str(output)],
            cwd=ROOT,
            text=True,
            capture_output=True,
            check=False,
        )
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
        return Path(result.stdout.strip().splitlines()[-1])

    def test_runtime_package_is_reproducible_and_contains_no_credentials(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            first = self._build(root / "one")
            second = self._build(root / "two")
            self.assertEqual(hashlib.sha256(first.read_bytes()).digest(), hashlib.sha256(second.read_bytes()).digest())

            with tarfile.open(first, "r:gz") as archive:
                names = archive.getnames()
                payload = b"".join(
                    handle.read()
                    for member in archive.getmembers()
                    if member.isfile() and (handle := archive.extractfile(member)) is not None
                )

            self.assertTrue(any(name.endswith("runtime/run.py") for name in names))
            self.assertTrue(any(name.endswith("teamharness/mcp/server.py") for name in names))
            self.assertTrue(any(name.endswith("teamharness/adapters/codex-cli/README.md") for name in names))
            self.assertFalse(any(name.endswith("auth.json") for name in names))
            self.assertNotIn(b"AGENTTEAMS_WORKER_MATRIX_TOKEN=", payload)


if __name__ == "__main__":
    unittest.main()
