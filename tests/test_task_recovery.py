"""Contract tests, not native Harness or real-model continuation evidence."""
import copy
import io
import os
from pathlib import Path
import sys
import unittest
import urllib.error
from unittest.mock import Mock, patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "shared/lib"))
from task_recovery import Recovery, RecoveryConflict, RecoveryUnavailable, enabled


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

    def test_managed_standalone_boot_still_discovers_later_team_assignments(self):
        with patch.dict(os.environ, {"AGENTTEAMS_CONTROLLER_URL": "http://controller", "AGENTTEAMS_AGENT_ROLE": "standalone"}):
            self.assertTrue(enabled())

    def test_scrubbed_token_file_environment_uses_the_scoped_worker_mount(self):
        with patch.dict(os.environ, {"AGENTTEAMS_PROCESS_ID": "actual-start", "AGENTTEAMS_CONTROLLER_URL": "http://controller.invalid"}, clear=True), \
                patch("task_recovery.Path.read_text", autospec=True, return_value="scoped-worker-token\n") as read, \
                patch("task_recovery.urllib.request.urlopen", return_value=io.BytesIO(b'{"tasks":[]}')) as request:
            self.assertEqual(Recovery().discover(), {"tasks": []})
        read.assert_called_once_with(Path("/var/run/secrets/agentteams/token"))
        headers = dict((key.lower(), value) for key, value in request.call_args.args[0].header_items())
        self.assertEqual(headers["authorization"], "Bearer scoped-worker-token")
        self.assertEqual(headers["x-agentteams-process-id"], "actual-start")

    def test_temporary_controller_failure_is_a_controlled_tool_error(self):
        failures = [urllib.error.HTTPError("http://controller", 503, "Unavailable", {}, None),
            urllib.error.URLError("connection reset"), TimeoutError()]
        for failure in failures:
            with self.subTest(failure=type(failure).__name__), patch.dict(os.environ, {
                    "AGENTTEAMS_CONTROLLER_URL": "http://controller.invalid", "AGENTTEAMS_AUTH_TOKEN_FILE": "/unused"}), \
                    patch("task_recovery.Path.read_text", return_value="private-test-token"), \
                    patch("task_recovery.urllib.request.urlopen", side_effect=failure):
                with self.assertRaises(RecoveryUnavailable):
                    Recovery()._request("GET", "tasks/recoverable")

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

    def test_delivered_progress_allows_next_reply_but_pending_and_submitted_do_not(self):
        cp = self.task["execution"]["checkpoint"]
        cp.update(result={"key": "progress.txt"}, delivery="delivered", reply_txn="progress", reply_event="$progress")
        with patch.dict(os.environ, {"AGENTTEAMS_STORAGE_PREFIX": "alias/bucket"}), patch("task_recovery.subprocess.run"):
            self.recovery.save_reply("stable-task", "team", "Step two done")
        saved = self.request.call_args.args[2]["checkpoint"]
        self.assertEqual(saved["remaining_steps"], ["two", "three"])
        self.assertNotIn("reply_event", saved)
        self.assertNotEqual(saved["reply_txn"], "progress")
        for status, delivery in (("in_progress", "pending"), ("submitted", "delivered")):
            self.task["status"] = status
            cp["delivery"] = delivery
            self.request.reset_mock()
            with patch("task_recovery.subprocess.run") as upload, self.assertRaises(RecoveryConflict):
                self.recovery.save_reply("stable-task", "team", "Do not replace")
            upload.assert_not_called()

    def test_interrupted_turn_leaves_saved_progress_available_to_the_next_start(self):
        cp = self.task["execution"]["checkpoint"]
        cp.update(result={"key": "progress.txt"}, delivery="delivered", reply_txn="progress", reply_event="$progress")
        before = copy.deepcopy(self.task)
        self.recovery.interrupted("stable-task", "team")
        self.assertEqual(self.task, before)
        self.request.assert_not_called()

    def test_native_callback_reclaims_only_its_process_and_delivers_saved_submission(self):
        self.task.update(status="submitted")
        self.task["execution"]["state"] = "released"
        claimed = {"task": self.task, "execution": {"checkpoint": {"delivery": "pending"}}, "saved_result": "Persisted report"}
        with patch.object(self.recovery, "claim", return_value=claimed) as claim, patch.object(self.recovery, "save_reply") as upload:
            saved, body = self.recovery.prepare_reply("stable-task", "team", "Local native final answer")
            self.assertEqual(saved, claimed)
            self.assertEqual(body, "Persisted report")
            claim.assert_called_once()
            upload.assert_not_called()
        self.snapshot["instance_id"] = "replacement-start"
        with patch.object(self.recovery, "claim") as claim, self.assertRaises(RecoveryConflict):
            self.recovery.prepare_reply("stable-task", "team", "Stale answer")
        claim.assert_not_called()

    def test_interruption_after_durable_submission_keeps_delivery_recoverable(self):
        self.task.update(status="submitted", result_path="shared/tasks/stable-task/result.md")
        before = copy.deepcopy(self.task)
        self.recovery.interrupted("stable-task", "team")
        self.assertEqual(self.task, before)
        self.request.assert_not_called()

    def test_active_submitted_callback_saves_its_answer_before_delivery_claim(self):
        self.task.update(status="submitted")
        self.task["execution"]["state"] = "active"
        with patch.object(self.recovery, "claim", side_effect=RecoveryConflict("no submitted report")) as claim, patch.object(self.recovery, "save_reply", return_value={"saved": True}) as save:
            saved, body = self.recovery.prepare_reply("stable-task", "team", "Native final answer")
        self.assertEqual((saved, body), ({"saved": True}, "Native final answer"))
        save.assert_called_once_with("stable-task", "team", "Native final answer")
        claim.assert_not_called()

    def test_active_submitted_callback_prefers_authoritative_report_over_native_text(self):
        self.task.update(status="submitted", result_path="shared/tasks/stable-task/result.md")
        self.task["execution"]["state"] = "active"
        claimed = {"task": self.task, "execution": {"checkpoint": {"delivery": "pending"}}, "saved_result": "Authoritative report"}
        with patch.object(self.recovery, "claim", return_value=claimed) as claim, patch.object(self.recovery, "save_reply") as save:
            saved, body = self.recovery.prepare_reply("stable-task", "team", "Stale native summary")
        self.assertEqual((saved, body), (claimed, "Authoritative report"))
        claim.assert_called_once()
        save.assert_not_called()

    def test_completed_callback_recovers_the_accepted_report(self):
        self.task.update(status="completed", result_path="shared/tasks/stable-task/result.md")
        claimed = {"task": self.task, "execution": {"checkpoint": {"delivery": "pending"}}, "saved_result": "Accepted report"}
        with patch.object(self.recovery, "claim", return_value=claimed) as claim, patch.object(self.recovery, "save_reply") as save:
            result = self.recovery.prepare_reply("stable-task", "team", "Stale native summary")
        self.assertEqual(result, (claimed, "Accepted report"))
        claim.assert_called_once()
        save.assert_not_called()


if __name__ == "__main__":
    unittest.main()
