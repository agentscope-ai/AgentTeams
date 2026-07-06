#!/usr/bin/env python3
"""Integration tests for the TeamHarness plugin package and CLI fallback."""

from __future__ import annotations

import json
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
LOONGSUITE_DEFINITION = (
    REPO_ROOT / "plugins" / "teamharness" / "loongsuite" / "agents.d" / "teamharness.json"
)


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
            "TEAMHARNESS_INSTALL_LOG": str(self.project / "teamharness-install.jsonl"),
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

    def write_fake_runtime(self, name: str) -> Path:
        bin_dir = self.project / "bin"
        bin_dir.mkdir(exist_ok=True)
        fake = bin_dir / name
        fake.write_text("#!/usr/bin/env sh\nexit 0\n", encoding="utf-8")
        fake.chmod(0o755)
        return bin_dir

    def package_teamharness(self) -> Path:
        result = subprocess.run(
            ["ruby", str(REPO_ROOT / "plugins" / "scripts" / "package-plugin.rb"), str(TEAMHARNESS_MANIFEST)],
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
            "adapters/qwenpaw/README.md",
            "adapters/qwenpaw/plugin.py",
            "adapters/qwenpaw/plugin.json",
            "scripts/install.sh",
            "scripts/uninstall.sh",
        }
        self.assertTrue(required.issubset(names), sorted(required - names))

    def test_loongsuite_plugin_probe_definition_contract(self) -> None:
        definition = json.loads(LOONGSUITE_DEFINITION.read_text(encoding="utf-8"))

        self.assertEqual(definition["id"], "teamharness")
        self.assertEqual(definition["displayName"], "TeamHarness")
        self.assertEqual(definition["deployMode"], "plugin-probe")
        self.assertEqual(definition["pluginProbe"]["source"]["type"], "tar")
        self.assertEqual(
            definition["pluginProbe"]["source"]["tarball"],
            "$PILOT_DIR/plugins/teamharness.tar.gz",
        )
        self.assertEqual(
            definition["pluginProbe"]["source"]["destDir"],
            "$PILOT_DATA/plugins/teamharness",
        )
        self.assertEqual(definition["pluginProbe"]["mountType"], "wrapper")
        self.assertIn("qwenpaw", definition["detection"]["commands"])
        self.assertNotIn("claude", definition["detection"]["commands"])

    def test_cli_installs_updates_and_uninstalls_same_tarball(self) -> None:
        package = self.package_teamharness()
        fake_bin = self.write_fake_runtime("qwenpaw")
        env_extra = {"PATH": f"{fake_bin}{os.pathsep}{os.environ.get('PATH', '')}"}

        listed_empty = self.run_agentteams("plugin", "list", env_extra=env_extra)
        self.assertEqual(listed_empty.returncode, 0, listed_empty.stderr + listed_empty.stdout)
        self.assertIn("No plugins installed.", listed_empty.stdout)

        installed = self.run_agentteams(
            "plugin", "install", "teamharness", "--package", str(package), env_extra=env_extra
        )
        self.assertEqual(installed.returncode, 0, installed.stderr + installed.stdout)

        manifest_path = self.project / ".agentteams" / "plugins" / "teamharness" / "manifest.json"
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        self.assertEqual(manifest["name"], "teamharness")
        self.assertEqual(manifest["version"], "0.1.0")
        self.assertEqual(manifest["package"], str(package))
        self.assertTrue((self.project / manifest["content_dir"] / "scripts" / "install.sh").is_file())

        listed = self.run_agentteams("plugin", "list", env_extra=env_extra)
        self.assertEqual(listed.returncode, 0, listed.stderr + listed.stdout)
        self.assertIn("teamharness", listed.stdout)

        updated = self.run_agentteams(
            "plugin", "update", "teamharness", "--package", str(package), env_extra=env_extra
        )
        self.assertEqual(updated.returncode, 0, updated.stderr + updated.stdout)
        self.assertIn("Updated teamharness", updated.stdout)

        uninstalled = self.run_agentteams("plugin", "uninstall", "teamharness", env_extra=env_extra)
        self.assertEqual(uninstalled.returncode, 0, uninstalled.stderr + uninstalled.stdout)
        self.assertFalse(manifest_path.exists())
        self.assertFalse((self.project / ".agentteams" / "plugins" / "teamharness").exists())

        log_lines = (self.project / "teamharness-install.jsonl").read_text(encoding="utf-8").splitlines()
        events = [json.loads(line)["event"] for line in log_lines]
        self.assertGreaterEqual(events.count("install"), 2)
        self.assertIn("uninstall", events)
        self.assertIn("qwenpaw", [json.loads(line).get("runtime") for line in log_lines])

    def test_loongsuite_plugin_probe_can_run_same_install_script(self) -> None:
        package = self.package_teamharness()
        fake_bin = self.write_fake_runtime("qwenpaw")
        dest_dir = self.project / "pilot-data" / "plugins" / "teamharness"
        dest_dir.mkdir(parents=True)

        with tarfile.open(package, "r:gz") as archive:
            archive.extractall(dest_dir)

        result = subprocess.run(
            ["bash", str(dest_dir / "scripts" / "install.sh")],
            cwd=self.project,
            env={
                **os.environ,
                "PATH": f"{fake_bin}{os.pathsep}{os.environ.get('PATH', '')}",
                "PILOT_DATA_DIR": str(self.project / "pilot-data"),
                "PILOT_LOG_DIR": str(self.project / "pilot-data" / "logs" / "teamharness"),
                "TEAMHARNESS_INSTALL_LOG": str(self.project / "teamharness-install.jsonl"),
            },
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )

        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
        log_lines = (self.project / "teamharness-install.jsonl").read_text(encoding="utf-8").splitlines()
        self.assertIn("qwenpaw", [json.loads(line).get("runtime") for line in log_lines])

    def test_cli_reports_invalid_package_without_traceback(self) -> None:
        broken = self.project / "broken.tar.gz"
        broken.write_text("not a tarball", encoding="utf-8")

        result = self.run_agentteams("plugin", "install", "teamharness", "--package", str(broken))

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("ERROR:", result.stdout + result.stderr)
        self.assertNotIn("Traceback", result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main(verbosity=2)
