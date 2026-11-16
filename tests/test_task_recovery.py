"""Contract tests, not native Harness or real-model continuation evidence."""
import copy
import os
from pathlib import Path
import sys
import unittest
from unittest.mock import Mock, patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "shared/lib"))
from task_recovery import Recovery, RecoveryConflict


class RecoveryTests(unittest.TestCase):
    def setUp(self):
        self.environment = patch.dict(os.environ, {"AGENTTEAMS_PROCESS_ID": "actual-start"})
        self.environment.start()
        self.addCleanup(self.environment.stop)
        self.task = {"task_id": "stable-task", "status": "in_progress", "execution": {
            "instance_id": "container/started-at", "process_id": "actual-start", "generation": 2,
            "checkpoint": {"version": 3, "completed_steps": ["one"], "remaining_steps": ["two", "three"]}}}
        self.snapshot = {"instance_id": "container/started-at", "tasks": [
            {"task": self.task, "team_id": "team", "etag": "read-version"}]}
        self.request = Mock(return_value=self.snapshot)
        self.recovery = Recovery(request=self.request)

    def test_session_cache_requires_harness_version_and_expiry(self):
        checkpoint = self.task["execution"]["checkpoint"]
        session = {"runtime": "codex", "checkpoint_digest": self.recovery.digest(checkpoint), "expires_at": 100}
        self.assertTrue(self.recovery.session_usable(session, self.task, "codex", now=99))
        self.assertFalse(self.recovery.session_usable(session, self.task, "hermes", now=99))
        self.assertFalse(self.recovery.session_usable(session, self.task, "codex", now=100))
        self.task["execution"]["checkpoint"]["version"] = 4
        self.assertFalse(self.recovery.session_usable(session, self.task, "codex", now=99))
        self.assertFalse(self.recovery.session_usable(None, self.task, "codex", now=99))

    def test_claim_uses_team_and_stable_task_not_project(self):
        self.recovery.claim(self.snapshot, self.snapshot["tasks"][0])
        args = self.request.call_args.args
        self.assertEqual(args[:2], ("POST", "tasks/stable-task/execution?team=team"))
        self.assertEqual(args[2]["process_id"], "actual-start")
        self.assertEqual(args[3], "read-version")

    def test_stale_process_cannot_commit_to_new_container(self):
        self.snapshot["instance_id"] = "replacement/started-at"
        with self.assertRaises(RecoveryConflict):
            self.recovery.checkpoint("stable-task", "team", {"version": 4})
        self.assertEqual(self.request.call_count, 1)

    def test_cancelled_or_reassigned_task_cannot_commit(self):
        self.snapshot["tasks"] = []
        with self.assertRaises(RecoveryConflict):
            self.recovery.checkpoint("stable-task", "team", {"version": 4})
        self.assertEqual(self.request.call_count, 1)

    def test_reply_upload_precedes_checkpoint_and_retains_step_progress(self):
        calls = []
        def request(method, path, *args):
            if method == "GET":
                return self.snapshot
            calls.append("checkpoint")
            return args[0]
        self.recovery.request = request
        with patch.dict(os.environ, {"AGENTTEAMS_STORAGE_PREFIX": "alias/bucket"}), patch("task_recovery.subprocess.run", side_effect=lambda *a, **k: calls.append("upload")) as upload:
            result = self.recovery.save_reply("stable-task", "team", "Saved answer")
        self.assertEqual(calls, ["upload", "checkpoint"])
        cp = result["checkpoint"]
        self.assertEqual(cp["completed_steps"], ["one"])
        self.assertEqual(cp["version"], 4)
        self.assertEqual(cp["delivery"], "pending")
        self.assertTrue(cp["reply_txn"].startswith("task-reply:"))
        self.assertEqual(upload.call_args.kwargs["input"], b"Saved answer")

    def test_delivery_only_reuses_saved_transaction_and_result(self):
        cp = self.task["execution"]["checkpoint"]
        cp.update(result={"key": "saved.txt", "sha256": "a"*64, "size": 12}, delivery="pending", reply_txn="stable-reply")
        original = copy.deepcopy(cp)
        self.recovery.delivered("stable-task", "team", "$sent")
        sent = self.request.call_args.args[2]["checkpoint"]
        self.assertEqual(sent["result"], original["result"])
        self.assertEqual(sent["reply_txn"], "stable-reply")
        self.assertEqual(sent["delivery"], "delivered")
        self.assertEqual(sent["reply_event"], "$sent")


if __name__ == "__main__":
    unittest.main()
