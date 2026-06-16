#!/usr/bin/env python3
"""Integration tests for the AgentTeams plugin package and CLI fallback."""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import tarfile
import tempfile
import textwrap
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[3]
CLI_SRC = REPO_ROOT / "plugins" / "cli" / "src"
PLUGIN_NAME = "demo-plugin"


class AgentTeamsPluginCliTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory(prefix="agentteams-cli-")
        self.project = Path(self.tmp.name)
        self.out_dir = self.project / "dist"
        self.out_dir.mkdir()
        self.plugin_root = self.create_plugin_fixture()

    def tearDown(self) -> None:
        self.tmp.cleanup()

    def create_plugin_fixture(self) -> Path:
        plugins_root = self.project / "plugins"
        plugin_root = plugins_root / PLUGIN_NAME
        schema_dir = plugins_root / "schemas"
        schema_dir.mkdir(parents=True)
        shutil.copy(REPO_ROOT / "plugins" / "schemas" / "plugin.schema.json", schema_dir / "plugin.schema.json")

        files = {
            "plugin.yaml": f"""
                apiVersion: hiclaw.agentteam/v1alpha1
                kind: AgentTeamPlugin
                metadata:
                  name: {PLUGIN_NAME}
                  version: 0.1.0
                dependencies:
                  - python>=3.9
                package:
                  include:
                    - plugin.yaml
                    - prompts
                    - skills
                    - mcp
                    - adapters
                    - scripts
                prompts:
                  team: prompts/team.md
                  agent:
                    worker: prompts/worker.md
                  manager:
                    default: prompts/manager.md
                skills:
                  team:
                    - id: demo-communication
                      path: skills/team/communication
                mcp:
                  servers:
                    - id: demo
                      command: python3
                      args:
                        - mcp/server.py
                adapters:
                  - id: demo
                    path: adapters/demo
            """,
            "prompts/team.md": "team prompt\n",
            "prompts/worker.md": "worker prompt\n",
            "prompts/manager.md": "manager prompt\n",
            "skills/team/communication/SKILL.md": "---\nname: demo-communication\n---\n",
            "mcp/server.py": "print('demo')\n",
            "adapters/demo/README.md": "# Demo adapter\n",
            "scripts/install.sh": """
                #!/usr/bin/env bash
                set -euo pipefail
                if [ -n "${AGENTTEAMS_TEST_INSTALL_LOG:-}" ]; then
                  printf '{"event":"install","name":"%s","dir":"%s"}\\n' "${AGENTTEAMS_PLUGIN_NAME:-}" "${AGENTTEAMS_PLUGIN_DIR:-}" >> "$AGENTTEAMS_TEST_INSTALL_LOG"
                fi
            """,
            "scripts/uninstall.sh": """
                #!/usr/bin/env bash
                set -euo pipefail
                if [ -n "${AGENTTEAMS_TEST_INSTALL_LOG:-}" ]; then
                  printf '{"event":"uninstall","name":"%s","dir":"%s"}\\n' "${AGENTTEAMS_PLUGIN_NAME:-}" "${AGENTTEAMS_PLUGIN_DIR:-}" >> "$AGENTTEAMS_TEST_INSTALL_LOG"
                fi
            """,
        }
        for relative, content in files.items():
            path = plugin_root / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(textwrap.dedent(content).lstrip(), encoding="utf-8")
        return plugin_root

    def run_agentteams(
        self,
        *args: str,
        env_extra: dict[str, str] | None = None,
    ) -> subprocess.CompletedProcess[str]:
        env = {
            **os.environ,
            "PYTHONPATH": str(CLI_SRC),
            "PYTHONDONTWRITEBYTECODE": "1",
            "AGENTTEAMS_TEST_INSTALL_LOG": str(self.project / "agentteams-install.jsonl"),
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

    def package_demo_plugin(self) -> Path:
        result = subprocess.run(
            ["ruby", str(REPO_ROOT / "plugins" / "scripts" / "package-plugin.rb"), str(self.plugin_root / "plugin.yaml")],
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

    def test_plugin_tarball_contract(self) -> None:
        package = self.package_demo_plugin()

        self.assertEqual(package.name, f"{PLUGIN_NAME}.tar.gz")
        with tarfile.open(package, "r:gz") as archive:
            names = set(archive.getnames())

        required = {
            "plugin.yaml",
            "prompts/team.md",
            "skills/team/communication/SKILL.md",
            "mcp/server.py",
            "adapters/demo/README.md",
            "scripts/install.sh",
            "scripts/uninstall.sh",
        }
        self.assertTrue(required.issubset(names), sorted(required - names))

    def test_cli_installs_updates_and_uninstalls_same_tarball(self) -> None:
        package = self.package_demo_plugin()

        listed_empty = self.run_agentteams("plugin", "list")
        self.assertEqual(listed_empty.returncode, 0, listed_empty.stderr + listed_empty.stdout)
        self.assertIn("No plugins installed.", listed_empty.stdout)

        installed = self.run_agentteams("plugin", "install", PLUGIN_NAME, "--package", str(package))
        self.assertEqual(installed.returncode, 0, installed.stderr + installed.stdout)

        manifest_path = self.project / ".agentteams" / "plugins" / PLUGIN_NAME / "manifest.json"
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        self.assertEqual(manifest["name"], PLUGIN_NAME)
        self.assertEqual(manifest["version"], "0.1.0")
        self.assertEqual(manifest["package"], str(package))
        self.assertTrue((self.project / manifest["content_dir"] / "scripts" / "install.sh").is_file())

        listed = self.run_agentteams("plugin", "list")
        self.assertEqual(listed.returncode, 0, listed.stderr + listed.stdout)
        self.assertIn(PLUGIN_NAME, listed.stdout)

        updated = self.run_agentteams("plugin", "update", PLUGIN_NAME, "--package", str(package))
        self.assertEqual(updated.returncode, 0, updated.stderr + updated.stdout)
        self.assertIn(f"Updated {PLUGIN_NAME}", updated.stdout)

        uninstalled = self.run_agentteams("plugin", "uninstall", PLUGIN_NAME)
        self.assertEqual(uninstalled.returncode, 0, uninstalled.stderr + uninstalled.stdout)
        self.assertFalse(manifest_path.exists())
        self.assertFalse((self.project / ".agentteams" / "plugins" / PLUGIN_NAME).exists())

        log_lines = (self.project / "agentteams-install.jsonl").read_text(encoding="utf-8").splitlines()
        events = [json.loads(line)["event"] for line in log_lines]
        self.assertGreaterEqual(events.count("install"), 2)
        self.assertGreaterEqual(events.count("uninstall"), 2)

    def test_cli_reports_invalid_package_without_traceback(self) -> None:
        broken = self.project / "broken.tar.gz"
        broken.write_text("not a tarball", encoding="utf-8")

        result = self.run_agentteams("plugin", "install", PLUGIN_NAME, "--package", str(broken))

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("ERROR:", result.stdout + result.stderr)
        self.assertNotIn("Traceback", result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main(verbosity=2)
