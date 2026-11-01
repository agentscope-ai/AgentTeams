"""Regression tests for the C4 (dependencies dry-run) stage of
``qwenpaw_worker.upgrade.checker``.

Port of the pre-#1364 standalone-script-layout regression onto
the modular checker: when ``--check-deps`` is explicitly requested, a
dry-run that cannot run (missing tool -> OSError, timeout ->
``subprocess.TimeoutExpired``) must be upgrade-blocking (FAIL, exit 1),
not a silent WARN with exit 0.

Maintainer gate detail, AgentTeams#1341 (2026-10-09).

All tests are offline: ``subprocess.run`` is mocked and no wheel or
network access is required.
"""
from __future__ import annotations

import contextlib
import importlib.util
import io
import json
import subprocess
import sys
import tempfile
import unittest
import unittest.mock
from pathlib import Path

REPO = Path(__file__).resolve().parents[1]
SRC = REPO / "src"
if str(SRC) not in sys.path:
    sys.path.insert(0, str(SRC))

from qwenpaw_worker.upgrade import checker  # noqa: E402

WRAPPER = REPO / "scripts" / "check_qwenpaw_upgrade.py"

UNRUNNABLE_EXC = (
    OSError("network disabled in tests"),
    subprocess.TimeoutExpired(cmd=["uv", "pip"], timeout=300),
)


def load_wrapper():
    spec = importlib.util.spec_from_file_location("check_qwenpaw_upgrade", WRAPPER)
    module = importlib.util.module_from_spec(spec)
    # Register before exec: dataclass processing resolves string annotations
    # through sys.modules when the module uses `from __future__ import annotations`.
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def build_mini_repo(root: Path, version: str = "2.2.1") -> Path:
    """Miniature QwenPaw repo tree: C1/C2 PASS at the target, C3 skips
    (no patch script, no wheel) - same fixture pattern as
    ``test_check_qwenpaw_upgrade.build_repo``."""
    qwenpaw = root / "qwenpaw"
    (qwenpaw / "src" / "qwenpaw_worker").mkdir(parents=True)
    (qwenpaw / "src" / "qwenpaw_worker" / "worker.py").write_text(
        "async def f():\n"
        f'    await asyncio.to_thread(self.api_client.require_version, "{version}")\n',
        encoding="utf-8",
    )
    (qwenpaw / "Dockerfile").write_text(
        f"ARG QWENPAW_PIP_SPEC=qwenpaw=={version}\n", encoding="utf-8")
    (root / "manager").mkdir()
    (root / "manager" / "Dockerfile.qwenpaw").write_text(
        f"ARG QWENPAW_PIP_SPEC=qwenpaw=={version}\n", encoding="utf-8")
    (qwenpaw / "pyproject.toml").write_text(
        "dependencies = [\n"
        f'    "qwenpaw=={version}",\n'
        "]\n",
        encoding="utf-8",
    )
    return qwenpaw


def run_json(module, argv):
    buffer = io.StringIO()
    with contextlib.redirect_stdout(buffer):
        code = module.main(argv + ["--json"])
    return code, json.loads(buffer.getvalue())


def check_by_id(payload, check_id):
    return next(item for item in payload["checks"] if item["id"] == check_id)


class CheckDepsRegressionTests(unittest.TestCase):
    """Unit-level regression: ``checker.check_deps`` on both unrunnable
    paths (patch target: the checker module's ``subprocess``, the same
    module singleton the pre-#1364 script patched)."""

    def test_check_deps_unrunnable_fails(self):
        # Maintainer gate detail (AgentTeams#1341 comment, 2026-10-09): an
        # explicitly requested --check-deps stage that cannot run - missing
        # tool (OSError) or timeout (TimeoutExpired) - must be
        # upgrade-blocking, not a silent WARN with exit 0.
        for exc in UNRUNNABLE_EXC:
            with unittest.mock.patch.object(checker.subprocess, "run",
                                            side_effect=exc):
                result = checker.check_deps("2.2.1")
            self.assertEqual(result.status, checker.STATUS_FAIL,
                             f"status for {exc!r}")
            self.assertIn("could not run", result.evidence)
            self.assertIn("explicitly requested dry-run", result.evidence)
            self.assertIsNotNone(result.advice)

    def test_run_preflight_unrunnable_deps_blocks(self):
        # Aggregation level: a C4 FAIL from an unrunnable requested dry-run
        # must drive overall FAIL / exit_code 1.
        temp = tempfile.TemporaryDirectory(prefix="checkdeps-test-")
        self.addCleanup(temp.cleanup)
        qwenpaw = build_mini_repo(Path(temp.name))
        for exc in UNRUNNABLE_EXC:
            with unittest.mock.patch.object(checker.subprocess, "run",
                                            side_effect=exc):
                checks, meta = checker.run_preflight(
                    "2.2.1", qwenpaw_dir=qwenpaw, check_deps_flag=True)
            self.assertEqual(meta["overall"], checker.STATUS_FAIL,
                             f"overall for {exc!r}")
            self.assertEqual(meta["exit_code"], 1)
            deps = next(c for c in checks if c.id == "C4")
            self.assertEqual(deps.status, checker.STATUS_FAIL)
            self.assertIn("could not run", deps.evidence)
            self.assertIsNotNone(deps.advice)

    def test_run_preflight_default_skips_deps(self):
        # Non-goal guard: without the flag, C4 stays SKIP and the default
        # behavior is unchanged (check_deps is never reached).
        temp = tempfile.TemporaryDirectory(prefix="checkdeps-test-")
        self.addCleanup(temp.cleanup)
        qwenpaw = build_mini_repo(Path(temp.name))
        checks, meta = checker.run_preflight("2.2.1", qwenpaw_dir=qwenpaw)
        self.assertEqual(meta["overall"], checker.STATUS_PASS)
        self.assertEqual(meta["exit_code"], 0)
        deps = next(c for c in checks if c.id == "C4")
        self.assertEqual(deps.status, checker.STATUS_SKIP)


class WrapperCliTests(unittest.TestCase):
    """Wrapper CLI e2e: exit-code semantics through the full
    argparse -> run_preflight -> --json path (``python3
    scripts/check_qwenpaw_upgrade.py --target ... --qwenpaw-dir ...
    --check-deps --json``)."""

    def setUp(self):
        self.wrapper = load_wrapper()
        self.temp = tempfile.TemporaryDirectory(prefix="checkdeps-cli-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)

    def test_cli_check_deps_unrunnable_exits_1(self):
        qwenpaw = build_mini_repo(self.root)
        for exc in UNRUNNABLE_EXC:
            with unittest.mock.patch.object(checker.subprocess, "run",
                                            side_effect=exc):
                code, payload = run_json(self.wrapper, [
                    "--target", "2.2.1", "--qwenpaw-dir", str(qwenpaw),
                    "--check-deps",
                ])
            self.assertEqual(code, 1, f"exit code for {exc!r}")
            self.assertEqual(payload["overall"], "FAIL")
            deps = check_by_id(payload, "C4")
            self.assertEqual(deps["status"], "FAIL")
            self.assertIn("could not run", deps["evidence"])
            self.assertIsNotNone(deps["advice"])

    def test_cli_default_no_check_deps(self):
        qwenpaw = build_mini_repo(self.root)
        code, payload = run_json(self.wrapper, [
            "--target", "2.2.1", "--qwenpaw-dir", str(qwenpaw),
        ])
        self.assertEqual(code, 0)
        self.assertEqual(payload["overall"], "PASS")
        self.assertEqual(check_by_id(payload, "C4")["status"], "SKIP")


if __name__ == "__main__":
    unittest.main()
