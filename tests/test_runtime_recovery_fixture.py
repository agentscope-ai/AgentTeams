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
import urllib.error
import urllib.parse

from fixtures.runtime_recovery_oracle import Ledger
import tempfile


class EffectReceiptTests(unittest.TestCase):
    def setUp(self):
        self.task = {"task_id": "stable", "status": "in_progress", "recovery_fixture": {"oracle_url": "http://oracle"},
                     "execution": {"generation": 1, "state": "active"}}
        self.prefix = "teams/team/shared/recovery-fixtures/stable/effects/"
        self.store = {self.prefix + ".keep": b""}
        self.process = "first-process"
        self.fail_output = False
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.ledger = Ledger(Path(self.directory.name) / "effects.json")
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
                owner.task["execution"]["state"] = "needs_verification" if checkpoint["operation"]["state"] != "verified" else "active"
        self.client = Client()

    def http(self, request, **kwargs):
        params = urllib.parse.parse_qs(urllib.parse.urlsplit(request.full_url).query)
        self.task["execution"].update(process_id=self.process, instance_id="docker/" + self.process)
        snapshot = {"instance_id": "docker/" + self.process, "tasks": [{"team_id": "team", "task": self.task}]}
        try:
            method = "REGISTER" if urllib.parse.urlsplit(request.full_url).path == "/intent" else request.method
            entry = self.ledger.effect(snapshot, "team", "stable", params["step"][0], self.process, method)
        except ValueError as error:
            raise urllib.error.HTTPError(request.full_url, 409, str(error), {}, None) from error
        return contextlib.closing(io.BytesIO(json.dumps(entry, sort_keys=True).encode()))

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
                patch.dict(os.environ, {"AGENTTEAMS_STORAGE_PREFIX": "alias/bucket", "AGENTTEAMS_AUTH_TOKEN_FILE": "/mock/token"}), \
                patch("pathlib.Path.read_text", return_value="scoped-worker-token"), \
                patch("urllib.request.urlopen", side_effect=self.http), \
                patch("subprocess.run", side_effect=self.mc), contextlib.redirect_stdout(output):
            try:
                runpy.run_path(str(Path(__file__).parent / "fixtures/runtime_recovery_step.py"))
            except SystemExit as error:
                self.assertEqual(error.code, 0)
        return output.getvalue()

    def receipts(self):
        return self.ledger.proof("team", "stable")["effects"]

    def test_source_step_waits_for_its_native_assignment_read(self):
        worker = "@worker:test"
        self.task.update(room_id="!room:test", assigned_to=worker, notifyTxn="dispatch-1")
        self.task["recovery_fixture"]["wait_for_source_read"] = True
        assignment = {"event_id": "$source", "type": "m.room.message", "content": {
            "org.agentteams.coordination": {"channel_meta": {"teamharness_coordination": {
                "kind": "task_assigned", "task_id": "stable", "delivery_id": "dispatch-1"}}}}}
        receipt = {"type": "receipt", "sender": worker,
                   "content": {"status": "read", "m.relates_to": {"event_id": "$source"}}}
        pages = iter([[assignment, {**receipt, "sender": "@other:test"}], [assignment, receipt]])

        def get_events(*arguments):
            self.assertEqual(self.receipts(), [])
            return {"chunk": next(pages)}

        receipts = SimpleNamespace(RECEIPT_TYPE="receipt", credentials=lambda _: ("server", "key"),
                                   matrix_request=lambda *_: get_events)
        with patch.dict(sys.modules, {"agentteams_receipts": receipts}):
            self.assertIn("SOURCE_READ_PENDING", self.invoke())
            self.assertEqual(self.receipts(), [])
            self.assertIsNone(self.task["execution"].get("checkpoint"))
            self.invoke()
        self.assertEqual([row["step"] for row in self.receipts()], ["one"])

    def test_openhuman_command_accepts_a_three_digit_fixture_task(self):
        command = Path(self.directory.name) / "agentteams-recovery-step"
        command.symlink_to(Path(__file__).parent / "fixtures/runtime_recovery_step.py")
        task_id = "live-hermes-openhuman-116"
        controller = "http://recovery-1631-live-20261118-v116-controller:8090"
        received = []
        client = SimpleNamespace(current=lambda *args: received.append(args))
        with patch.dict(sys.modules, {"task_recovery": SimpleNamespace(Recovery=lambda: client)}), \
                patch.object(sys, "argv", [str(command), task_id, "recovery-team"]), \
                patch.dict(os.environ, {"AGENTTEAMS_CONTROLLER_URL": controller}):
            with self.assertRaisesRegex(RuntimeError, "ACK the assigned Task"):
                runpy.run_path(str(command))
        self.assertEqual(received, [(task_id, "recovery-team")])

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

    def test_returning_runtime_cannot_run_the_next_generation_step_early(self):
        self.task["recovery_fixture"]["return_to_source"] = True
        for number, process in enumerate(("first", "second", "third"), 1):
            self.task["execution"]["generation"] = number
            self.process = process
            self.invoke()
            if number < 3:
                self.assertIn("PAUSED_AT_STEP_BOUNDARY", self.invoke())
        self.assertEqual([entry["generation"] for entry in self.receipts()], [1, 2, 3])
        self.assertEqual([entry["step"] for entry in self.receipts()], ["one", "two", "three"])

    def test_next_step_retires_delivered_progress_and_its_device_binding(self):
        self.invoke()
        cp = self.task["execution"]["checkpoint"]
        cp.update(delivery="delivered", result={"summary": "Progress"}, reply_txn="progress-txn",
                  reply_event="$progress", reply_attempt={"state": "bound", "user": "@worker:test", "device": "old"})
        self.task["execution"]["generation"] = 2
        self.process = "replacement-process"
        self.invoke()
        current = self.task["execution"]["checkpoint"]
        self.assertFalse({"result", "delivery", "reply_txn", "reply_event", "reply_attempt"} & current.keys())
        self.assertEqual(current["completed_steps"], ["one", "two"])
        self.assertEqual([e["generation"] for e in self.receipts()], [1, 2])

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
        with self.assertRaisesRegex(urllib.error.HTTPError, "Needs verification"):
            self.invoke()
        self.assertEqual(self.receipts(), [])

    def test_model_registered_start_without_oracle_intent_is_verified_before_effect(self):
        self.task["recovery_fixture"]["oracle_version"] = 4
        self.task["execution"].update(state="needs_verification", checkpoint={"version": 1,
            "completed_steps": [], "remaining_steps": ["one", "two", "three"],
            "operation": {"id": "fixture:stable:one", "state": "started"}})
        self.invoke()
        proof = self.ledger.proof("team", "stable")
        self.assertEqual([row["method"] for row in proof["calls"]], ["GET", "REGISTER", "POST"])
        self.assertEqual(proof["calls"][0]["verification"], "never_authorized")
        self.assertTrue(all(row["accepted"] for row in proof["calls"]))
        self.assertEqual([effect["step"] for effect in proof["effects"]], ["one"])

    def test_interruption_after_external_effect_requires_readback_before_continuing(self):
        self.task["recovery_fixture"]["pause_after_effect"] = "one"
        with patch("time.sleep", side_effect=RuntimeError("injected process stop")):
            with self.assertRaisesRegex(RuntimeError, "injected process stop"):
                self.invoke()
        self.assertEqual(self.task["execution"]["checkpoint"]["operation"]["state"], "started")
        self.assertEqual(self.task["execution"]["checkpoint"]["completed_steps"], [])
        self.assertEqual(len(self.receipts()), 1)
        self.task["execution"]["generation"] = 2
        self.process = "replacement-process"
        self.task["execution"]["checkpoint"]["artifacts"] = None  # Controller's serialized empty slice.
        self.invoke()
        proof = self.ledger.proof("team", "stable")
        self.assertEqual([row["method"] for row in proof["calls"]], ["REGISTER", "POST", "GET"])
        self.assertEqual(len(proof["effects"]), 1)
        self.assertEqual(self.task["execution"]["checkpoint"]["operation"]["state"], "verified")


if __name__ == "__main__":
    unittest.main()
