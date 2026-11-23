#!/usr/bin/env python3
"""Integration tests for the TeamHarness plugin package and CLI fallback."""

from __future__ import annotations

import io
import os
import subprocess
import sys
import tarfile
import tempfile
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[3]
CLI_SRC = REPO_ROOT / "plugins" / "cli" / "src"
TEAMHARNESS_MANIFEST = REPO_ROOT / "plugins" / "teamharness" / "plugin.yaml"


class AgentTeamsPluginCliTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory(prefix="teamharness-cli-")
        self.project = Path(self.tmp.name)
        self.out_dir = self.project / "dist"
        self.out_dir.mkdir()

    def tearDown(self) -> None:
        self.tmp.cleanup()

    def run_agentteams(
        self,
        *args: str,
        env_extra: dict[str, str] | None = None,
    ) -> subprocess.CompletedProcess[str]:
        env = {
            **os.environ,
            "PYTHONPATH": str(CLI_SRC),
            "PYTHONDONTWRITEBYTECODE": "1",
        }
        if env_extra:
            env.update(env_extra)
        return subprocess.run(
            [sys.executable, "-m", "agentteams_cli.main", *args],
            cwd=self.project,
            env=env,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )

    def package_teamharness(self, manifest: Path = TEAMHARNESS_MANIFEST) -> Path:
        result = subprocess.run(
            ["ruby", str(REPO_ROOT / "plugins" / "scripts" / "package-plugin.rb"), str(manifest)],
            cwd=REPO_ROOT,
            env={**os.environ, "OUT_DIR": str(self.out_dir)},
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
        package = Path(result.stdout.strip().splitlines()[-1])
        self.assertTrue(package.is_file(), package)
        return package

    def test_teamharness_tarball_contract(self) -> None:
        package = self.package_teamharness()

        self.assertEqual(package.name, "teamharness.tar.gz")
        with tarfile.open(package, "r:gz") as archive:
            names = set(archive.getnames())

        required = {
            "plugin.yaml",
            "prompts/team/TEAMS.md",
            "skills/team/communication/SKILL.md",
            "mcp/server.py",
        }
        self.assertTrue(required.issubset(names), sorted(required - names))
        self.assertFalse(any(name.startswith("adapters/qwenpaw/") for name in names))

    def test_cli_installs_updates_and_uninstalls_portable_hermes_bundle(self) -> None:
        manifest = REPO_ROOT / "plugins/agentteams-teamharness/plugin.yaml"
        package = self.package_teamharness(manifest)
        name = "agentteams-teamharness"
        hermes_home = self.project / "hermes"
        hermes_home.mkdir()
        env = {"HOME": str(self.project), "HERMES_HOME": str(hermes_home), "PATH": os.defpath}
        # Keep this interpreter's PyYAML available to the bundle scripts.
        env["PATH"] = os.pathsep.join((str(Path(sys.executable).parent), env["PATH"]))
        plugin = hermes_home / "plugins" / name
        for action in ("install", "update"):
            result = self.run_agentteams("plugin", action, name, "--package", str(package), env_extra=env)
            self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
            self.assertTrue((plugin / "register.py").is_file())
        result = self.run_agentteams("plugin", "uninstall", name, env_extra=env)
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
        self.assertFalse(plugin.exists())

    def test_cli_reports_invalid_package_without_traceback(self) -> None:
        broken = self.project / "broken.tar.gz"
        broken.write_text("not a tarball", encoding="utf-8")

        result = self.run_agentteams("plugin", "install", "teamharness", "--package", str(broken))

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("ERROR:", result.stdout + result.stderr)
        self.assertNotIn("Traceback", result.stdout + result.stderr)

    def test_cli_rejects_tar_symlink_escape(self) -> None:
        package = self.project / "symlink-escape.tar.gz"
        outside = self.project / "outside"
        outside.mkdir()
        payload = b"escaped\n"

        with tarfile.open(package, "w:gz") as archive:
            link = tarfile.TarInfo("teamharness/escape")
            link.type = tarfile.SYMTYPE
            link.linkname = str(outside)
            archive.addfile(link)

            member = tarfile.TarInfo("teamharness/escape/pwned.txt")
            member.size = len(payload)
            archive.addfile(member, io.BytesIO(payload))

        result = self.run_agentteams(
            "plugin", "install", "teamharness", "--package", str(package)
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("unsafe tar member", result.stdout + result.stderr)
        self.assertFalse((outside / "pwned.txt").exists())


if __name__ == "__main__":
    unittest.main(verbosity=2)
