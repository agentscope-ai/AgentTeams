"""Reject the forged step-boundary pattern observed in real case 17."""
import unittest
from unittest.mock import patch

with patch.dict("os.environ", {"RECOVERY_E2E_FROM": "codex", "RECOVERY_E2E_TO": "hermes", "RECOVERY_E2E_MODE": "replace", "RECOVERY_E2E_CASE": "01"}):
    from runtime_recovery_e2e import assert_step_boundaries


class ReceiptBoundaryTests(unittest.TestCase):
    def test_registered_start_and_verified_boundary_before_the_next_step(self):
        assert_step_boundaries([{"checkpoint_version": n} for n in [1, 5, 7]], 4)
        assert_step_boundaries([{"checkpoint_version": n} for n in [1, 5, 7]], 2)

    def test_case17_manual_receipts_without_a_step_two_boundary_are_rejected(self):
        with self.assertRaisesRegex(ValueError, "step boundary"):
            assert_step_boundaries([{"checkpoint_version": n} for n in [1, 5, 6]], 4)


class ImageSelectionTests(unittest.TestCase):
    def test_explicit_current_images_allow_the_qwenpaw_lane(self):
        import json
        import runpy
        from pathlib import Path
        with patch.dict("os.environ", {
            "RECOVERY_E2E_FROM": "qwenpaw", "RECOVERY_E2E_TO": "hermes",
            "RECOVERY_E2E_MODE": "replace", "RECOVERY_E2E_CASE": "23",
            "RECOVERY_E2E_CORE_IMAGE": "recovery-1631/controller:local-current",
            "RECOVERY_E2E_HARNESS_IMAGES": json.dumps({"qwenpaw": "recovery-1631/qwenpaw:local-current"}),
        }):
            config = runpy.run_path(str(Path(__file__).with_name("runtime_recovery_e2e.py")))
        self.assertEqual(config["CORE_IMAGE"], "recovery-1631/controller:local-current")
        self.assertEqual(config["HARNESS_IMAGES"]["qwenpaw"], "recovery-1631/qwenpaw:local-current")
        self.assertEqual(config["TASK_ID"], "live-qwenpaw-hermes-23")


class SubmissionHistoryTests(unittest.TestCase):
    def test_acceptance_preserves_the_native_worker_submission(self):
        from runtime_recovery_e2e import assert_native_submission
        submit = {"action": "submit_task", "actor": "worker:agent-a", "from": "in_progress", "to": "submitted", "seq": 2}
        accept = {"action": "accept_task_result", "actor": "leader:leader", "from": "submitted", "to": "completed", "seq": 3}
        assert_native_submission({"status": "submitted", "history": [submit]})
        assert_native_submission({"status": "completed", "history": [submit, accept]})

    def test_final_status_alone_does_not_prove_submission_or_review(self):
        from runtime_recovery_e2e import assert_native_submission
        submit = {"action": "submit_task", "actor": "worker:agent-a", "from": "in_progress", "to": "submitted", "seq": 2}
        accept = {"action": "accept_task_result", "actor": "leader:leader", "from": "submitted", "to": "completed", "seq": 3}
        invalid = [
            {"status": "completed", "history": []},
            {"status": "completed", "history": [submit]},
            {"status": "completed", "history": [submit, {**accept, "actor": "worker:agent-a"}]},
            {"status": "completed", "history": [submit, {**accept, "seq": 1}]},
            {"status": "submitted", "history": [submit, submit]},
            {"status": "cancelled", "history": [submit, accept]},
        ]
        for task in invalid:
            with self.subTest(task=task), self.assertRaises(ValueError):
                assert_native_submission(task)


class OpenClawCacheTests(unittest.TestCase):
    def test_actual_native_session_and_latest_checkpoint_are_required(self):
        import hashlib
        import json
        import time
        from runtime_recovery_e2e import assert_openclaw_native_cache
        cp = {"version": 10, "completed_steps": ["one", "two", "three"]}
        cache = {"runtime": "openclaw", "expires_at": time.time() + 60, "session_id": "native",
                 "checkpoint_digest": hashlib.sha256(json.dumps(cp, sort_keys=True, separators=(",", ":")).encode()).hexdigest(),
                 "native_digest": "actual-sdk-transcript"}
        native = {"session_id": "native", "native_digest": "actual-sdk-transcript", "message_count": 7, "task_mentioned": True}
        assert_openclaw_native_cache({"cache": cache, "native": native}, cp)
        for changed in [{"session_id": "old"}, {"native_digest": "uncommitted"}, {"message_count": 0}, {"task_mentioned": False}]:
            with self.subTest(changed=changed), self.assertRaises(AssertionError):
                assert_openclaw_native_cache({"cache": cache, "native": {**native, **changed}}, cp)
        with self.assertRaises(AssertionError):
            assert_openclaw_native_cache({"cache": cache, "native": native}, {**cp, "version": 11})


class CutoverBoundaryTests(unittest.TestCase):
    def test_source_snapshot_is_persisted_before_any_lifecycle_mutation(self):
        import json
        from pathlib import Path
        import uuid
        import runtime_recovery_e2e as driver
        task = {"status": "in_progress", "execution": {"checkpoint": {
            "completed_steps": ["one"], "remaining_steps": ["two", "three"]}}}
        before = {"task": task, "instances": [{}, {"id": "old"}], "native_sessions": {"source": "actual-native"}}
        case = "boundary-test-" + uuid.uuid4().hex
        path = Path("/tmp/1631-live-" + case + "-before-cutover.json")
        self.addCleanup(lambda: path.unlink(missing_ok=True))
        class MutationObserved(Exception):
            pass
        def api(method, *args):
            if method == "GET":
                return task
            self.assertEqual(json.loads(path.read_text())["native_sessions"], before["native_sessions"])
            raise MutationObserved()
        with patch.object(driver, "CASE", case), patch.object(driver, "FAULT", ""), \
                patch.object(driver, "api", side_effect=api), \
                patch.object(driver, "docker", return_value=json.dumps([{"Id": "old", "Config": {"Image": driver.HARNESS_IMAGES[driver.SOURCE]}}])), \
                patch.object(driver, "evidence", return_value=before), \
                patch.object(driver, "fixture_tool_processes", return_value=[]):
            with self.assertRaises(MutationObserved):
                driver.cutover()
            with self.assertRaisesRegex(RuntimeError, "already has a boundary"):
                driver.cutover()
