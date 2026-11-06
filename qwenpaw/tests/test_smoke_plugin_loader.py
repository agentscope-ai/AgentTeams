"""Tests for scripts/smoke_plugin_loader.py (the keyless loader smoke).

The green path loads the real agentteams-matrix-channel package through
the installed QwenPaw PluginLoader; the red path corrupts a copy of the
package and proves the smoke turns red on it — the reproducible
negative control the CI job asserts. Both runs are offline: no network,
credentials, or services are touched (the package ships no
requirements.txt, so the loader performs no pip install).

The harness is invoked as a subprocess, exactly like the
plugin-loader-smoke CI job: the loader's import chain (qwenpaw, which
pulls in the PyPI ``mcp`` package) must stay out of the pytest process,
or it shadows TeamHarness' own top-level ``mcp`` package for any suite
run later in the same process.
"""
from __future__ import annotations

import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
SCRIPT = REPO / "qwenpaw" / "scripts" / "smoke_plugin_loader.py"
PLUGIN_DIR = REPO / "plugins" / "agentteams-matrix-channel"


def _run_smoke(*args: str) -> int:
    """Run the harness in a child process and return its exit code."""
    proc = subprocess.run(
        [sys.executable, str(SCRIPT), *args],
        capture_output=True,
        text=True,
    )
    if proc.returncode not in (0, 1, 2):
        raise AssertionError(
            f"smoke harness crashed (rc {proc.returncode}): {proc.stderr}"
        )
    return proc.returncode


def _broken_copy(source: Path, target_root: Path) -> Path:
    """Copy the package and make its backend entry fail at import time."""
    target_root.mkdir(parents=True, exist_ok=True)
    target = target_root / source.name
    if target.exists():
        shutil.rmtree(target)
    shutil.copytree(source, target)
    entry = target / "plugin.py"
    entry.write_text(
        entry.read_text(encoding="utf-8")
        + '\nraise RuntimeError("intentionally broken smoke package")\n',
        encoding="utf-8",
    )
    return target


class SmokePluginLoaderTest(unittest.TestCase):
    def test_real_package_loads_green(self):
        with tempfile.TemporaryDirectory(prefix="smoke-green-") as tmp:
            rc = _run_smoke(
                "--plugin-dir", str(PLUGIN_DIR), "--working-dir", tmp
            )
        self.assertEqual(rc, 0)

    def test_broken_package_is_red(self):
        with tempfile.TemporaryDirectory(prefix="smoke-broken-") as tmp:
            root = Path(tmp)
            broken = _broken_copy(PLUGIN_DIR, root / "packages")
            workdir = root / "workdir"
            # A plain run must fail on the broken package...
            self.assertEqual(
                _run_smoke(
                    "--plugin-dir",
                    str(broken),
                    "--working-dir",
                    str(workdir),
                ),
                1,
            )
            # ...and the negative control must accept that failure.
            self.assertEqual(
                _run_smoke(
                    "--plugin-dir",
                    str(broken),
                    "--working-dir",
                    str(workdir),
                    "--expect-failure",
                ),
                0,
            )

    def test_smoke_never_touches_the_callers_runtime(self):
        """QwenPaw bootstraps persisted state at import: the harness must
        redirect it into the scratch working dir, never the caller's
        QWENPAW_WORKING_DIR/HOME (the L3 leak this pins)."""
        with tempfile.TemporaryDirectory(prefix="smoke-caller-") as tmp:
            caller = Path(tmp) / "caller-runtime"
            caller.mkdir()
            (caller / ".env").write_text("CALLER=1\n", encoding="utf-8")
            before = sorted(p.name for p in caller.iterdir())
            scratch = Path(tmp) / "scratch"
            proc = subprocess.run(
                [
                    sys.executable,
                    str(SCRIPT),
                    "--working-dir",
                    str(scratch),
                ],
                capture_output=True,
                text=True,
                env={**os.environ, "QWENPAW_WORKING_DIR": str(caller), "HOME": str(caller)},
            )
            self.assertEqual(proc.returncode, 0, proc.stderr)
            after = sorted(p.name for p in caller.iterdir())
            self.assertEqual(
                after,
                before,
                "the smoke bootstrapped QwenPaw against the caller's runtime",
            )
            self.assertTrue(
                (scratch / ".qwenpaw_restore.lock").exists(),
                "the runtime bootstrap should land inside the scratch working dir",
            )

    def test_missing_manifest_is_usage_error(self):
        with tempfile.TemporaryDirectory(prefix="smoke-nomanifest-") as tmp:
            empty = Path(tmp) / "not-a-plugin"
            empty.mkdir()
            self.assertEqual(
                _run_smoke(
                    "--plugin-dir",
                    str(empty),
                    "--working-dir",
                    str(Path(tmp) / "workdir"),
                ),
                2,
            )

    def test_smoke_does_not_load_callers_secret_environment(self):
        with tempfile.TemporaryDirectory(prefix="smoke-secret-") as tmp:
            root = Path(tmp)
            secret = root / "caller-secret"
            secret.mkdir()
            (secret / "envs.json").write_text('{"AGENTTEAMS_TEST_CALLER_SECRET":"fixture-only"}')
            code = (
                "import os, runpy, sys; "
                "smoke = runpy.run_path(sys.argv[1]); "
                "rc = smoke['main'](['--working-dir', sys.argv[2]]); "
                "assert rc == 0; "
                "assert 'AGENTTEAMS_TEST_CALLER_SECRET' not in os.environ, 'caller secret loaded'; "
                "assert os.environ['QWENPAW_SECRET_DIR'] == sys.argv[3], 'caller environment not restored'"
            )
            proc = subprocess.run(
                [sys.executable, "-c", code, str(SCRIPT), str(root / "scratch"), str(secret)],
                capture_output=True, text=True,
                env={**os.environ, "QWENPAW_SECRET_DIR": str(secret)},
            )
            self.assertEqual(proc.returncode, 0, proc.stderr)


if __name__ == "__main__":
    unittest.main()
