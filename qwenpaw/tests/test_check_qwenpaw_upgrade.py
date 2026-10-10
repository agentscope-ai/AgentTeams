"""Unit tests for scripts/check_qwenpaw_upgrade.py.

Run with pytest (``python -m pytest qwenpaw/tests/test_check_qwenpaw_upgrade.py``)
or directly (``python3 qwenpaw/tests/test_check_qwenpaw_upgrade.py``).
All tests are offline: they build a miniature repository tree plus a fake
wheel in a temporary directory and never reach the network.
"""
from __future__ import annotations

import contextlib
import importlib.util
import io
import json
import sys
import tempfile
import unittest
import unittest.mock
import zipfile
from pathlib import Path

REPO = Path(__file__).resolve().parents[1]
CHECKER = REPO / "scripts" / "check_qwenpaw_upgrade.py"

OLD_LINE = "                await self._card_store.save(card)\n"

# Verbatim copy of the replacement block in
# scripts/patch-qwenpaw-driver-policy-reload.py (its ``new = (...)`` literal).
# MANAGER_NEW must contain this exact text for the checker's whole-block
# integration check to pass.
NEW_BLOCK = (
    "                # Policy may change while the replacement handler initializes.\n"
    "                # Read it under the same lock as sync_driver_policy; never\n"
    "                # write the stale card back over newer persisted configuration.\n"
    "                latest = await self._card_store.load_path(path)\n"
    "                card.policy = latest.policy\n"
    "                if handler is not None:\n"
    "                    handler.set_policy(card.policy)\n"
)

MANAGER_OLD = (
    "class DriverManager:\n"
    "    async def reload_driver(self, name):\n"
    "        card = None\n"
    + OLD_LINE
    + "        return card\n"
    "\n"
    "    async def refresh_driver(self, name):\n"
    "        return None\n"
)

MANAGER_NEW = (
    "class DriverManager:\n"
    "    async def reload_driver(self, name):\n"
    "        card = None\n"
    + NEW_BLOCK
    + "        return card\n"
    "\n"
    "    async def refresh_driver(self, name):\n"
    "        return None\n"
)

# Shape of a partially applied patch: marker comment and first code line
# present, but the rest of the replacement block missing.
MANAGER_PARTIAL = (
    "class DriverManager:\n"
    "    async def reload_driver(self, name):\n"
    "        card = None\n"
    "                # Policy may change while the replacement handler initializes.\n"
    "                latest = await self._card_store.load_path(path)\n"
    "        return card\n"
    "\n"
    "    async def refresh_driver(self, name):\n"
    "        return None\n"
)

MANAGER_OTHER = (
    "class DriverManager:\n"
    "    async def reload_driver(self, name):\n"
    "        card = None\n"
    "        return card\n"
    "\n"
    "    async def refresh_driver(self, name):\n"
    "        return None\n"
)


def load_checker():
    spec = importlib.util.spec_from_file_location("check_qwenpaw_upgrade", CHECKER)
    module = importlib.util.module_from_spec(spec)
    # Register before exec: dataclass processing resolves string annotations
    # through sys.modules when the module uses `from __future__ import annotations`.
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def _render_literal(line: str) -> str:
    escaped = line.replace("\\", "\\\\").replace('"', '\\"').replace("\n", "\\n")
    return f'        "{escaped}"'


def patch_script_text(guard: str = "2.2.1") -> str:
    new_literals = "\n".join(
        _render_literal(line) for line in NEW_BLOCK.splitlines(keepends=True))
    return (
        '"""Fake compatibility fix."""\n'
        "from importlib.metadata import distribution\n"
        "\n"
        "\n"
        "def main():\n"
        '    package = distribution("qwenpaw")\n'
        f'    if package.version != "{guard}":\n'
        '        raise RuntimeError("review")\n'
        f"    old = {json.dumps(OLD_LINE)}\n"
        "    new = (\n"
        f"{new_literals}\n"
        "    )\n"
        "    return old, new\n"
    )


def build_repo(root: Path, gate: str = "2.2.1", dockerfile: str = "2.2.1",
               manager: str = "2.2.1", pyproject: str = "2.2.1",
               guard: str = "2.2.1") -> Path:
    qwenpaw = root / "qwenpaw"
    (qwenpaw / "src" / "qwenpaw_worker").mkdir(parents=True)
    (qwenpaw / "scripts").mkdir()
    (qwenpaw / "tests").mkdir()
    (qwenpaw / "src" / "qwenpaw_worker" / "worker.py").write_text(
        "async def f():\n"
        f'    await asyncio.to_thread(self.api_client.require_version, "{gate}")\n',
        encoding="utf-8",
    )
    (qwenpaw / "Dockerfile").write_text(
        f"ARG QWENPAW_PIP_SPEC=qwenpaw=={dockerfile}\n", encoding="utf-8")
    (root / "manager").mkdir()
    (root / "manager" / "Dockerfile.qwenpaw").write_text(
        f"ARG QWENPAW_PIP_SPEC=qwenpaw=={manager}\n", encoding="utf-8")
    (qwenpaw / "pyproject.toml").write_text(
        "dependencies = [\n"
        f'    "qwenpaw=={pyproject}",\n'
        "]\n",
        encoding="utf-8",
    )
    (qwenpaw / "scripts" / "patch-qwenpaw-driver-policy-reload.py").write_text(
        patch_script_text(guard), encoding="utf-8")
    (qwenpaw / "tests" / "test_runtime_dependencies.py").write_text(
        'assert "qwenpaw==2.2.1" in deps\n', encoding="utf-8")
    return qwenpaw


def build_wheel(directory: Path, version: str, manager_text: str) -> Path:
    wheel = directory / f"qwenpaw-{version}-py3-none-any.whl"
    with zipfile.ZipFile(wheel, "w") as zf:
        zf.writestr("qwenpaw/drivers/manager.py", manager_text)
        zf.writestr(
            f"qwenpaw-{version}.dist-info/METADATA",
            f"Metadata-Version: 2.1\nName: qwenpaw\nVersion: {version}\n",
        )
    return wheel


def run_main(module, argv):
    buffer = io.StringIO()
    with contextlib.redirect_stdout(buffer):
        code = module.main(argv)
    return code, buffer.getvalue()


def run_json(module, argv):
    code, output = run_main(module, argv + ["--json"])
    return code, json.loads(output)


def check_by_id(payload, check_id):
    return next(item for item in payload["checks"] if item["id"] == check_id)


class PreflightTests(unittest.TestCase):
    def setUp(self):
        self.module = load_checker()
        self.temp = tempfile.TemporaryDirectory(prefix="preflight-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)

    def test_consistent_target_passes(self):
        qwenpaw = build_repo(self.root)
        wheel = build_wheel(Path(self.temp.name), "2.2.1", MANAGER_OLD)
        code, payload = run_json(self.module, [
            "--target", "2.2.1", "--qwenpaw-dir", str(qwenpaw), "--wheel", str(wheel),
        ])
        self.assertEqual(code, 0)
        self.assertEqual(payload["overall"], "PASS")
        self.assertEqual(check_by_id(payload, "C1")["status"], "PASS")
        self.assertEqual(check_by_id(payload, "C2")["status"], "PASS")
        self.assertEqual(check_by_id(payload, "C3")["status"], "PASS")

    def test_gate_mismatch_fails(self):
        qwenpaw = build_repo(self.root)
        code, payload = run_json(self.module, [
            "--target", "2.2.2", "--qwenpaw-dir", str(qwenpaw),
        ])
        self.assertEqual(code, 1)
        gate = check_by_id(payload, "C1")
        self.assertEqual(gate["status"], "FAIL")
        self.assertIn("2.2.1", gate["evidence"])
        self.assertIn("worker.py", gate["evidence"])

    def test_pins_listed_for_bump(self):
        qwenpaw = build_repo(self.root)
        code, payload = run_json(self.module, [
            "--target", "2.2.2", "--qwenpaw-dir", str(qwenpaw),
        ])
        self.assertEqual(code, 1)
        pins = check_by_id(payload, "C2")
        self.assertEqual(pins["status"], "FAIL")
        self.assertIn("qwenpaw/Dockerfile", pins["evidence"])
        self.assertIn("manager/Dockerfile.qwenpaw", pins["evidence"])
        self.assertIn("qwenpaw/pyproject.toml", pins["evidence"])

    def test_partial_pin_drift(self):
        qwenpaw = build_repo(self.root, dockerfile="2.2.2", manager="2.2.1", pyproject="2.2.1")
        code, payload = run_json(self.module, [
            "--target", "2.2.2", "--qwenpaw-dir", str(qwenpaw),
        ])
        self.assertEqual(code, 1)
        pins = check_by_id(payload, "C2")
        self.assertEqual(pins["status"], "FAIL")
        self.assertIn("qwenpaw/Dockerfile: 2.2.2", pins["evidence"])
        self.assertIn("manager/Dockerfile.qwenpaw: 2.2.1", pins["evidence"])

    def test_patch_applies_to_old_source(self):
        qwenpaw = build_repo(self.root)
        wheel_dir = Path(tempfile.mkdtemp(dir=self.root))
        wheel = build_wheel(wheel_dir, "2.2.1", MANAGER_OLD)
        code, payload = run_json(self.module, [
            "--target", "2.2.1", "--qwenpaw-dir", str(qwenpaw), "--wheel", str(wheel),
        ])
        patch = check_by_id(payload, "C3")
        self.assertEqual(patch["status"], "PASS")
        self.assertIn("can be applied", patch["evidence"])

    def test_patch_already_integrated(self):
        qwenpaw = build_repo(self.root)
        wheel = build_wheel(Path(self.root), "2.2.1", MANAGER_NEW)
        code, payload = run_json(self.module, [
            "--target", "2.2.1", "--qwenpaw-dir", str(qwenpaw), "--wheel", str(wheel),
        ])
        patch = check_by_id(payload, "C3")
        self.assertEqual(patch["status"], "PASS")
        self.assertIn("already present", patch["evidence"])

    def test_patch_unexpected_source_fails(self):
        qwenpaw = build_repo(self.root)
        wheel = build_wheel(Path(self.root), "2.2.1", MANAGER_OTHER)
        code, payload = run_json(self.module, [
            "--target", "2.2.1", "--qwenpaw-dir", str(qwenpaw), "--wheel", str(wheel),
        ])
        self.assertEqual(code, 1)
        patch = check_by_id(payload, "C3")
        self.assertEqual(patch["status"], "FAIL")
        self.assertIn("occurs 0x", patch["evidence"])

    def test_wheel_version_mismatch_fails(self):
        qwenpaw = build_repo(self.root)
        wheel = build_wheel(Path(self.root), "2.2.3", MANAGER_OLD)
        code, payload = run_json(self.module, [
            "--target", "2.2.2", "--qwenpaw-dir", str(qwenpaw), "--wheel", str(wheel),
        ])
        patch = check_by_id(payload, "C3")
        self.assertEqual(patch["status"], "FAIL")
        self.assertIn("!= target", patch["evidence"])

    def test_no_wheel_skips_patch_check(self):
        qwenpaw = build_repo(self.root)
        code, payload = run_json(self.module, [
            "--target", "2.2.1", "--qwenpaw-dir", str(qwenpaw),
        ])
        self.assertEqual(code, 0)
        self.assertEqual(check_by_id(payload, "C3")["status"], "SKIP")
        self.assertEqual(check_by_id(payload, "C4")["status"], "SKIP")

    def test_json_shape(self):
        qwenpaw = build_repo(self.root)
        code, payload = run_json(self.module, [
            "--target", "2.2.1", "--qwenpaw-dir", str(qwenpaw),
        ])
        self.assertEqual(set(payload), {"target", "qwenpaw_dir", "overall", "exit_code", "checks"})
        for item in payload["checks"]:
            self.assertEqual(set(item), {"id", "name", "status", "evidence", "advice"})

    # ------------------------------------------------------------------
    # Round-2 regressions (AgentTeams#1343): the build-time patch contract
    # must be enforced, and explicit wheel-acquisition failures must not be
    # swallowed into a PASS/SKIP.
    # ------------------------------------------------------------------

    def test_patch_guard_lag_fails(self):
        # Maintainer repro: every pin already bumped to the target, only the
        # patch guard still lags - C3 must FAIL even though the old pattern
        # is still applicable in the wheel's source.
        qwenpaw = build_repo(self.root, gate="2.2.2", dockerfile="2.2.2",
                             manager="2.2.2", pyproject="2.2.2")
        wheel = build_wheel(Path(self.root), "2.2.2", MANAGER_OLD)
        code, payload = run_json(self.module, [
            "--target", "2.2.2", "--qwenpaw-dir", str(qwenpaw), "--wheel", str(wheel),
        ])
        self.assertEqual(code, 1)
        self.assertEqual(payload["overall"], "FAIL")
        self.assertEqual(check_by_id(payload, "C1")["status"], "PASS")
        self.assertEqual(check_by_id(payload, "C2")["status"], "PASS")
        patch = check_by_id(payload, "C3")
        self.assertEqual(patch["status"], "FAIL")
        self.assertIn('guard expects "2.2.1"', patch["evidence"])
        self.assertIn("2.2.2", patch["evidence"])

    def test_patch_partial_replacement_not_integrated(self):
        qwenpaw = build_repo(self.root)
        wheel = build_wheel(Path(self.root), "2.2.1", MANAGER_PARTIAL)
        code, payload = run_json(self.module, [
            "--target", "2.2.1", "--qwenpaw-dir", str(qwenpaw), "--wheel", str(wheel),
        ])
        self.assertEqual(code, 1)
        patch = check_by_id(payload, "C3")
        self.assertEqual(patch["status"], "FAIL")
        self.assertIn("marker present", patch["evidence"])
        self.assertNotIn("already present", patch["evidence"])

    def test_explicit_missing_wheel_fails(self):
        qwenpaw = build_repo(self.root, gate="2.2.2", dockerfile="2.2.2",
                             manager="2.2.2", pyproject="2.2.2")
        code, payload = run_json(self.module, [
            "--target", "2.2.2", "--qwenpaw-dir", str(qwenpaw),
            "--wheel", "/nonexistent.whl",
        ])
        self.assertEqual(code, 1)
        self.assertEqual(payload["overall"], "FAIL")
        patch = check_by_id(payload, "C3")
        self.assertEqual(patch["status"], "FAIL")
        self.assertIn("not found", patch["evidence"])

    def test_empty_wheelhouse_fails(self):
        qwenpaw = build_repo(self.root, gate="2.2.2", dockerfile="2.2.2",
                             manager="2.2.2", pyproject="2.2.2")
        house = self.root / "empty-wheelhouse"
        house.mkdir()
        code, payload = run_json(self.module, [
            "--target", "2.2.2", "--qwenpaw-dir", str(qwenpaw),
            "--wheelhouse", str(house),
        ])
        self.assertEqual(code, 1)
        patch = check_by_id(payload, "C3")
        self.assertEqual(patch["status"], "FAIL")
        self.assertIn("no qwenpaw-2.2.2-*.whl", patch["evidence"])

    def test_download_failure_fails(self):
        qwenpaw = build_repo(self.root, gate="2.2.2", dockerfile="2.2.2",
                             manager="2.2.2", pyproject="2.2.2")
        with unittest.mock.patch.object(self.module, "_http_json",
                                        side_effect=OSError("network disabled in tests")):
            code, payload = run_json(self.module, [
                "--target", "2.2.2", "--qwenpaw-dir", str(qwenpaw), "--download",
            ])
        self.assertEqual(code, 1)
        patch = check_by_id(payload, "C3")
        self.assertEqual(patch["status"], "FAIL")
        self.assertIn("--download failed", patch["evidence"])


if __name__ == "__main__":
    unittest.main()
