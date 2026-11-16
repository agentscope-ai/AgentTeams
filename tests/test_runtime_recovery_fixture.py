"""Acceptance-oracle unit tests; all Task/storage calls are fixtures."""
import contextlib
import copy
import hashlib
import io
import json
import os
from pathlib import Path
import runpy
import subprocess
import sys
from types import SimpleNamespace
import unittest
from unittest.mock import patch


class EffectReceiptTests(unittest.TestCase):
    def setUp(self):
        self.task = {"task_id": "stable", "status": "in_progress", "execution": {"generation": 1}}
        self.prefix = "teams/team/shared/recovery-fixtures/stable/effects/"
        self.store = {self.prefix + ".keep": b""}
        self.process = "first-process"
        self.fail_output = False
        owner = self
        class Client:
            @property
            def process(self):
                return owner.process

            def current(self, task_id, team):
                return copy.deepcopy(owner.task)

            def checkpoint(self, task_id, team, checkpoint):
                before = owner.task["execution"].get("checkpoint", {})
                owner.assertEqual(checkpoint["version"], before.get("version", 0)+1)
                for artifact in checkpoint.get("artifacts", []):
                    data = owner.store[artifact["key"]]
                    owner.assertEqual(hashlib.sha256(data).hexdigest(), artifact["sha256"])
                owner.task["execution"]["checkpoint"] = copy.deepcopy(checkpoint)
        self.client = Client()

    def mc(self, args, *, input=None, **kwargs):
        self.assertEqual(args[0], "mc")
        key = args[-1].removeprefix("alias/bucket/")
        if args[1] == "cat":
            data = self.store[key]
        elif args[1] == "ls":
            data = b"\n".join(json.dumps({"type": "file", "key": path[len(key):]}).encode()
                for path in self.store if path.startswith(key))
        elif args[1] == "pipe":
            if self.fail_output and "/output/" in key:
                self.fail_output = False
                raise subprocess.CalledProcessError(1, args)
            self.store[key] = input
            data = b""
        else:
            self.fail("unexpected storage operation")
        return SimpleNamespace(stdout=data)

    def invoke(self):
        output = io.StringIO()
        module = SimpleNamespace(Recovery=lambda: self.client)
        with patch.dict(sys.modules, {"task_recovery": module}), patch.object(sys, "argv", ["fixture", "stable", "team"]), \
                patch.dict(os.environ, {"AGENTTEAMS_STORAGE_PREFIX": "alias/bucket"}), \
                patch("subprocess.run", side_effect=self.mc), contextlib.redirect_stdout(output):
            try:
                runpy.run_path(str(Path(__file__).parent / "fixtures/runtime_recovery_step.py"))
            except SystemExit as error:
                self.assertEqual(error.code, 0)
        return output.getvalue()

    def receipts(self):
        return sorted((json.loads(data) for key, data in self.store.items()
            if key.startswith(self.prefix) and key.endswith(".json")), key=lambda entry: entry["checkpoint_version"])

    def test_stale_task_directory_ledger_cannot_erase_effect_receipts(self):
        self.invoke()
        self.assertIn("PAUSED_AT_STEP_BOUNDARY", self.invoke())
        # This reproduced the mutable attempts.json oracle's failure in case 12.
        self.store["teams/team/shared/tasks/stable/attempts.json"] = b"[]"
        self.task["execution"]["generation"] = 2
        self.process = "replacement-process"
        self.invoke()
        self.invoke()
        self.assertEqual([entry["step"] for entry in self.receipts()], ["one", "two", "three"])
        self.assertEqual([entry["generation"] for entry in self.receipts()], [1, 2, 2])
        self.assertEqual(self.task["execution"]["checkpoint"]["completed_steps"], ["one", "two", "three"])

    def test_uncertain_effect_is_verified_without_executing_it_again(self):
        self.fail_output = True
        with self.assertRaises(subprocess.CalledProcessError):
            self.invoke()
        self.assertEqual(len(self.receipts()), 1)
        self.task["execution"]["generation"] = 2
        self.process = "replacement-process"
        self.invoke()
        self.assertEqual(len(self.receipts()), 1)
        self.assertEqual(self.receipts()[0]["process_id"], "first-process")
        self.assertEqual(self.task["execution"]["checkpoint"]["operation"]["state"], "verified")

    def test_unproved_effect_waits_for_verification(self):
        self.task["execution"]["checkpoint"] = {"version": 1, "operation": {"id": "fixture:stable:one", "state": "unknown"}}
        with self.assertRaisesRegex(RuntimeError, "Needs verification"):
            self.invoke()
        self.assertEqual(self.receipts(), [])


if __name__ == "__main__":
    unittest.main()
