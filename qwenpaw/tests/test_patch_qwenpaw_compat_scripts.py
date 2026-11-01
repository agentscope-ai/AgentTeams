"""Build-time qwenpaw patch script tests: version-gate consistency + patch contract.

Run with pytest (``python -m pytest qwenpaw/tests/test_patch_qwenpaw_compat_scripts.py``).
All cases are offline.  The installed-executor case only reads the locally
installed qwenpaw package (never writes) and skips when the distribution
is absent.

Regression background (AgentTeams#1471): the qwenpaw pin was bumped to
2.2.2.post1 (hack/versions.env QWENPAW_PIP_SPEC) and the driver-reload
patch gate was widened, but the matrix-cron patch gate still rejected
2.2.2.post1, failing both Dockerfile builds at the patch RUN step.
"""
from __future__ import annotations

import importlib.util
import sys
import unittest
from importlib.metadata import PackageNotFoundError, distribution
from pathlib import Path

QWENPAW = Path(__file__).resolve().parents[1]
MATRIX_PATCH = QWENPAW / "scripts" / "patch-qwenpaw-matrix-cron-final.py"
DRIVER_PATCH = QWENPAW / "scripts" / "patch-qwenpaw-driver-policy-reload.py"
VERSIONS_ENV = QWENPAW.parent / "hack" / "versions.env"

MARKER = "# AgentTeams Matrix needs the closing response to flush its final reply."

# Minimal executor source carrying the three patch anchors with the real
# 2.2.x executor's indentation (declaration at 12 spaces; the event-loop
# dispatch branch and the final deliver call at 16 spaces, the latter
# inside the ``if final_event is not None:`` guard).
FIXTURE = (
    "            final_event: Any | None = None\n"
    "            for event in stream:\n"
    '                if job.dispatch.mode == "final":\n'
    "                    pass\n"
    "            if final_event is not None:\n"
    "                await _deliver(final_event)\n"
)


def load_script(name: str, path: Path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


def pinned_version() -> str:
    for line in VERSIONS_ENV.read_text(encoding="utf-8").splitlines():
        if line.startswith("QWENPAW_PIP_SPEC="):
            value = line.split("=", 1)[1].strip()
            if "==" in value:
                return value.split("==", 1)[1].strip()
    raise AssertionError("QWENPAW_PIP_SPEC not found in hack/versions.env")


class VersionGateConsistencyTests(unittest.TestCase):
    def test_pinned_spec_supported_by_both_patch_scripts(self):
        pinned = pinned_version()
        matrix = load_script("patch_qwenpaw_matrix_cron_final", MATRIX_PATCH)
        driver = load_script("patch_qwenpaw_driver_policy_reload", DRIVER_PATCH)
        self.assertIn(pinned, matrix.SUPPORTED_VERSIONS)
        self.assertIn(pinned, driver.SUPPORTED_VERSIONS)


class ApplyPatchContractTests(unittest.TestCase):
    def setUp(self):
        self.module = load_script("patch_qwenpaw_matrix_cron_final", MATRIX_PATCH)

    def test_applies_to_fixture(self):
        result = self.module.apply_patch(FIXTURE)
        self.assertIn(MARKER, result)
        self.assertIn("            final_response: Any | None = None\n", result)
        self.assertIn('                    if getattr(event, "object", None) == "response":\n', result)
        self.assertIn("                        final_response = event\n", result)
        self.assertIn(
            '                if target_channel == "agentteams_matrix" and final_response is not None:\n',
            result,
        )
        self.assertEqual(result.count("                    await _deliver(final_response)\n"), 1)
        # The extra delivery sits inside the existing final_event guard,
        # after _deliver(final_event) - never elsewhere.
        guard = result.index("if final_event is not None:")
        delivery = result.index("                    await _deliver(final_response)\n")
        first_delivery = result.index("                await _deliver(final_event)\n")
        self.assertTrue(guard < first_delivery < delivery)

    def test_idempotent(self):
        once = self.module.apply_patch(FIXTURE)
        twice = self.module.apply_patch(once)
        self.assertEqual(once, twice)

    def test_missing_anchor_raises(self):
        for anchor in self.module._ANCHORS:
            with self.subTest(anchor=anchor.strip()):
                drifted = FIXTURE.replace(anchor, "            something_else()\n")
                with self.assertRaises(RuntimeError):
                    self.module.apply_patch(drifted)

    def test_duplicate_anchor_raises(self):
        anchor = self.module._ANCHORS[1]
        drifted = FIXTURE.replace(anchor, anchor + anchor)
        with self.assertRaises(RuntimeError):
            self.module.apply_patch(drifted)


class InstalledExecutorTests(unittest.TestCase):
    def test_patch_applies_to_installed_executor_read_only(self):
        try:
            package = distribution("qwenpaw")
        except PackageNotFoundError:
            self.skipTest("qwenpaw distribution not installed here")
        path = package.locate_file("qwenpaw/app/crons/executor.py")
        source = path.read_text(encoding="utf-8")  # read-only; never written back
        print("\ninstalled qwenpaw:", package.version, "executor:", path)
        self.assertNotIn(MARKER, source)
        for anchor in self.module_anchors():
            self.assertEqual(source.count(anchor), 1, "anchor count != 1: %r" % anchor)
            for no, line in enumerate(source.splitlines(), 1):
                if line == anchor.rstrip("\n"):
                    print("anchor at %s:%d" % (path.name, no))
                    break
        patched = load_script("patch_qwenpaw_matrix_cron_final", MATRIX_PATCH).apply_patch(source)
        for fragment in (
            MARKER,
            "            final_response: Any | None = None\n",
            '                    if getattr(event, "object", None) == "response":\n',
            "                        final_response = event\n",
            '                if target_channel == "agentteams_matrix" and final_response is not None:\n',
            "                    await _deliver(final_response)\n",
        ):
            self.assertIn(fragment, patched)

    @staticmethod
    def module_anchors():
        module = load_script("patch_qwenpaw_matrix_cron_final", MATRIX_PATCH)
        return module._ANCHORS


if __name__ == "__main__":
    unittest.main()
