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
