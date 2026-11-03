#!/usr/bin/env python3
"""Regression tests for QwenPaw Manager workspace skill hot loading."""

from __future__ import annotations

import importlib.util
import json
import tempfile
import unittest
from pathlib import Path
from urllib.error import URLError


PROJECT_ROOT = Path(__file__).resolve().parents[2]
MODULE_PATH = (
    PROJECT_ROOT
    / "manager"
    / "scripts"
    / "init"
    / "qwenpaw_manager_skill_sync.py"
)


def load_module():
    spec = importlib.util.spec_from_file_location("qwenpaw_manager_skill_sync", MODULE_PATH)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"unable to load {MODULE_PATH}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class QwenPawManagerSkillSyncTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.module = load_module()

    def setUp(self) -> None:
        self.requests: list[tuple[str, object]] = []
        self.fail_refresh = False
        self.tempdir = tempfile.TemporaryDirectory()
        root = Path(self.tempdir.name)
        self.source = root / "manager-workspace" / "skills"
        self.workspace = root / ".qwenpaw" / "workspaces" / "default"
        self.state_file = root / ".qwenpaw" / "agentteams-manager-skills.json"
        self.source.mkdir(parents=True)
        self.workspace.mkdir(parents=True)
        self.sync = self.module.ManagerSkillSync(
            source_dir=self.source,
            workspace_dir=self.workspace,
            api_url="http://qwenpaw.test",
            state_file=self.state_file,
        )
        self.sync._post_json = self.post_json

    def tearDown(self) -> None:
        self.tempdir.cleanup()

    def write_skill(self, name: str, body: str = "first") -> None:
        skill_dir = self.source / name
        skill_dir.mkdir(parents=True, exist_ok=True)
        (skill_dir / "SKILL.md").write_text(
            f"---\nname: {name}\ndescription: test\n---\n\n{body}\n",
            encoding="utf-8",
        )

    def post_json(self, path: str, payload: object) -> object:
        self.requests.append((path, payload))
        if path == "/api/skills/refresh" and self.fail_refresh:
            raise URLError("refresh unavailable")
        if path == "/api/skills/batch-enable":
            return {
                "results": {
                    name: {
                        "success": True,
                        "updated_workspaces": ["default"],
                        "failed": [],
                        "reason": None,
                    }
                    for name in payload
                }
            }
        return {}

    def test_new_skill_is_copied_refreshed_and_enabled(self) -> None:
        self.write_skill("alert-fusion")

        self.assertTrue(self.sync.sync_once())

        copied = self.workspace / "skills" / "alert-fusion" / "SKILL.md"
        self.assertTrue(copied.is_file())
        self.assertEqual(
            self.requests,
            [
                ("/api/skills/refresh", {}),
                ("/api/skills/batch-enable", ["alert-fusion"]),
            ],
        )
        self.assertEqual(
            json.loads(self.state_file.read_text(encoding="utf-8"))["skills"].keys(),
            {"alert-fusion"},
        )

    def test_content_update_triggers_another_refresh(self) -> None:
        self.write_skill("alert-fusion")
        self.assertTrue(self.sync.sync_once())
        self.requests = []

        self.write_skill("alert-fusion", body="updated")
        self.assertTrue(self.sync.sync_once())

        self.assertIn(
            "updated",
            (self.workspace / "skills" / "alert-fusion" / "SKILL.md").read_text(
                encoding="utf-8"
            ),
        )
        self.assertEqual(
            self.requests,
            [
                ("/api/skills/refresh", {}),
                ("/api/skills/batch-enable", ["alert-fusion"]),
            ],
        )

    def test_removal_only_deletes_previously_managed_skills(self) -> None:
        self.write_skill("alert-fusion")
        unmanaged = self.workspace / "skills" / "qwenpaw-native"
        unmanaged.mkdir(parents=True)
        (unmanaged / "SKILL.md").write_text("native", encoding="utf-8")
        self.assertTrue(self.sync.sync_once())
        active_copy = self.workspace / "active_skills" / "alert-fusion"
        active_copy.mkdir(parents=True)
        (active_copy / "SKILL.md").write_text("startup copy", encoding="utf-8")
        self.requests = []

        skill_dir = self.source / "alert-fusion"
        (skill_dir / "SKILL.md").unlink()
        skill_dir.rmdir()
        self.assertTrue(self.sync.sync_once())

        self.assertFalse((self.workspace / "skills" / "alert-fusion").exists())
        self.assertFalse(active_copy.exists())
        self.assertTrue(unmanaged.is_dir())
        self.assertEqual(self.requests, [("/api/skills/refresh", {})])

    def test_failed_refresh_does_not_commit_state(self) -> None:
        self.write_skill("alert-fusion")
        self.fail_refresh = True

        self.assertFalse(self.sync.sync_once())
        self.assertFalse(self.state_file.exists())

        self.fail_refresh = False
        self.requests = []
        self.assertTrue(self.sync.sync_once())
        self.assertTrue(self.state_file.exists())
        self.assertEqual(
            self.requests,
            [
                ("/api/skills/refresh", {}),
                ("/api/skills/batch-enable", ["alert-fusion"]),
            ],
        )

    def test_directory_without_skill_md_is_ignored(self) -> None:
        invalid = self.source / "not-a-skill"
        invalid.mkdir()
        (invalid / "README.md").write_text("ignored", encoding="utf-8")

        self.assertTrue(self.sync.sync_once())

        self.assertEqual(self.requests, [])
        self.assertFalse((self.workspace / "skills" / "not-a-skill").exists())


if __name__ == "__main__":
    unittest.main()
