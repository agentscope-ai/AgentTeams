"""Recovery must preserve a skill already distributed through the public API."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / "manager/agent/skills/worker-management/scripts/push-worker-skills.sh"


class WorkerSkillRecovery(unittest.TestCase):
    def test_recovery_preserves_existing_copy_and_only_restores_missing(self):
        for exists in (True, False):
            with self.subTest(exists=exists), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                source = root / "worker-skills/example"
                source.mkdir(parents=True)
                (source / "SKILL.md").write_text("old manager copy")
                binary = root / "bin"
                binary.mkdir()
                mc = binary / "mc"
                mc.write_text("""#!/usr/bin/env python3
import os, sys
from pathlib import Path
root = Path(os.environ['TEST_STATE'])
with (root / 'calls').open('a') as log:
    log.write(sys.argv[1] + '\\n')
if sys.argv[1] == 'stat':
    sys.exit(0 if (root / 'remote').exists() else 1)
if sys.argv[1] == 'mirror':
    (root / 'remote').write_text('restored')
""")
                mc.chmod(0o755)
                if exists:
                    (root / "remote").write_text("new distributed copy")
                result = subprocess.run(["bash", str(SCRIPT), "--worker", "alice", "--skill", "example", "--no-notify"],
                    env={**os.environ, "HOME": str(root), "PATH": str(binary) + os.pathsep + os.environ["PATH"],
                         "AGENTTEAMS_STORAGE_PREFIX": "test/bucket", "TEST_STATE": str(root)}, capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual((root / "remote").read_text(), "new distributed copy" if exists else "restored")
                self.assertEqual("mirror" in (root / "calls").read_text().splitlines(), not exists)


if __name__ == "__main__":
    unittest.main()
