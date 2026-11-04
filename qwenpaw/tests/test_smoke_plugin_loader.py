"""Tests for scripts/smoke_plugin_loader.py (the keyless loader smoke).

The green path loads the real agentteams-matrix-channel package through
the installed QwenPaw PluginLoader; the red path corrupts a copy of the
package and proves the smoke turns red on it — the reproducible
negative control the CI job asserts. Both runs are offline: no network,
credentials, or services are touched (the package ships no
requirements.txt, so the loader performs no pip install).
"""
from __future__ import annotations

import importlib.util
import shutil
import tempfile
import unittest
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
SCRIPT = REPO / "qwenpaw" / "scripts" / "smoke_plugin_loader.py"
PLUGIN_DIR = REPO / "plugins" / "agentteams-matrix-channel"


def _load_script():
    spec = importlib.util.spec_from_file_location(
        "smoke_plugin_loader", SCRIPT
    )
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


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
        smoke = _load_script()
        with tempfile.TemporaryDirectory(prefix="smoke-green-") as tmp:
            rc = smoke.main(
                ["--plugin-dir", str(PLUGIN_DIR), "--working-dir", tmp]
            )
        self.assertEqual(rc, 0)

    def test_broken_package_is_red(self):
        smoke = _load_script()
        with tempfile.TemporaryDirectory(prefix="smoke-broken-") as tmp:
            root = Path(tmp)
            broken = _broken_copy(PLUGIN_DIR, root / "packages")
            workdir = root / "workdir"
            # A plain run must fail on the broken package...
            self.assertEqual(
                smoke.main(
                    [
                        "--plugin-dir",
                        str(broken),
                        "--working-dir",
                        str(workdir),
                    ]
                ),
                1,
            )
            # ...and the negative control must accept that failure.
            self.assertEqual(
                smoke.main(
                    [
                        "--plugin-dir",
                        str(broken),
                        "--working-dir",
                        str(workdir),
                        "--expect-failure",
                    ]
                ),
                0,
            )

    def test_missing_manifest_is_usage_error(self):
        smoke = _load_script()
        with tempfile.TemporaryDirectory(prefix="smoke-nomanifest-") as tmp:
            empty = Path(tmp) / "not-a-plugin"
            empty.mkdir()
            self.assertEqual(
                smoke.main(
                    [
                        "--plugin-dir",
                        str(empty),
                        "--working-dir",
                        str(Path(tmp) / "workdir"),
                    ]
                ),
                2,
            )


if __name__ == "__main__":
    unittest.main()
