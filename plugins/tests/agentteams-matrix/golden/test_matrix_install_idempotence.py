"""Install/uninstall idempotence tests for the agentteams-matrix bundle.

Exercises the real bundle scripts twice each (install, install, uninstall,
uninstall) against a fake QwenPaw working directory and a fake ``qwenpaw``
executable on PATH. No real QwenPaw service is required.
"""

import hashlib
import json
import os
import shutil
import subprocess
from pathlib import Path

GOLDEN_DIR = Path(__file__).resolve().parent
REPO_ROOT = GOLDEN_DIR.parents[3]
BUNDLE_DIR = REPO_ROOT / "plugins" / "agentteams-matrix"
INSTALL = BUNDLE_DIR / "scripts" / "install.sh"
UNINSTALL = BUNDLE_DIR / "scripts" / "uninstall.sh"
PLUGIN_DIRNAME = "agentteams-matrix-channel"


def _make_fake_qwenpaw(bin_dir: Path, call_log: Path) -> None:
    script = f"""#!/usr/bin/env bash
printf '%s\\n' "$*" >> "$(printf '%s' {call_log})"
exit 0
"""
    fake = bin_dir / "qwenpaw"
    fake.write_text(script, encoding="utf-8")
    fake.chmod(0o755)


def _env(tmp_path: Path, working_dir: Path, bin_dir: Path) -> dict:
    env = dict(os.environ)
    env["HOME"] = str(tmp_path)
    env["QWENPAW_WORKING_DIR"] = str(working_dir)
    env["PATH"] = f"{bin_dir}{os.pathsep}{env.get('PATH', '')}"
    env.pop("TEAMHARNESS_INSTALL_LOG", None)
    return env


def _snapshot(root: Path) -> dict:
    """Map of relpath -> (sha256, mtime_ns) for every file under root."""
    snapshot = {}
    if root.is_dir():
        for path in sorted(root.rglob("*")):
            if path.is_file():
                snapshot[
                    path.relative_to(root).as_posix()
                ] = (
                    hashlib.sha256(path.read_bytes()).hexdigest(),
                    path.stat().st_mtime_ns,
                )
    return snapshot


def _run(script: Path, env: dict) -> None:
    result = subprocess.run(
        ["bash", str(script)],
        env=env,
        cwd=REPO_ROOT,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    assert result.returncode == 0, (
        f"{script.name} failed: {result.stdout}\n{result.stderr}"
    )


def _adapter_env(working_dir: Path) -> dict:
    """Environment for running the adapter scripts directly (no QwenPaw
    CLI is involved in the Matrix file-level installer)."""
    env = dict(os.environ)
    env["QWENPAW_WORKING_DIR"] = str(working_dir)
    env.pop("TEAMHARNESS_INSTALL_LOG", None)
    return env


def test_install_twice_then_uninstall_twice(tmp_path):
    working_dir = tmp_path / "qwenpaw-workdir"
    working_dir.mkdir()
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    call_log = tmp_path / "qwenpaw-calls.log"
    _make_fake_qwenpaw(bin_dir, call_log)
    env = _env(tmp_path, working_dir, bin_dir)

    plugin_dir = working_dir / "plugins" / PLUGIN_DIRNAME
    state_root = working_dir / ".agentteams"
    marker = state_root / "agentteams-matrix" / "installed.json"

    # Install #1: payload lands in the QwenPaw working directory.
    _run(INSTALL, env)
    assert (plugin_dir / "plugin.json").is_file()
    assert (plugin_dir / "plugin.py").is_file()
    assert (plugin_dir / "agentteams_matrix" / "channel.py").is_file()
    payload = json.loads(
        (plugin_dir / "plugin.json").read_text(encoding="utf-8")
    )
    assert payload["id"] == "agentteams-matrix-channel"
    assert marker.is_file()
    recorded = json.loads(marker.read_text(encoding="utf-8"))
    assert recorded["contentHash"]
    assert recorded["files"]["plugin.py"]
    first_snapshot = _snapshot(working_dir)

    # Install #2: no-op — files, mtimes, and marker are all untouched.
    _run(INSTALL, env)
    assert _snapshot(working_dir) == first_snapshot
    assert (plugin_dir / "plugin.json").is_file()

    # Uninstall #1: removes exactly the installed files and all state.
    _run(UNINSTALL, env)
    assert not plugin_dir.exists()
    assert not marker.exists()
    assert not state_root.exists(), "installer state residue left behind"
    assert not (working_dir / "plugins").exists() or not any(
        (working_dir / "plugins").iterdir()
    )

    # Uninstall #2: no-op, still clean.
    _run(UNINSTALL, env)
    assert not plugin_dir.exists()
    assert not state_root.exists()


def test_install_update_touches_only_changed_files(tmp_path):
    working_dir = tmp_path / "qwenpaw-workdir"
    working_dir.mkdir()
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    call_log = tmp_path / "qwenpaw-calls.log"
    _make_fake_qwenpaw(bin_dir, call_log)
    env = _env(tmp_path, working_dir, bin_dir)

    plugin_dir = working_dir / "plugins" / PLUGIN_DIRNAME

    # Work on a copy of the bundle so the test never mutates repo content.
    bundle_copy = tmp_path / "bundle"
    shutil.copytree(BUNDLE_DIR, bundle_copy)
    _run(bundle_copy / "scripts" / "install.sh", env)
    first_snapshot = _snapshot(plugin_dir)

    # Change one payload file, keep the rest.
    target_file = plugin_dir / "plugin.json"
    marker_file = (
        working_dir / ".agentteams" / "agentteams-matrix" / "installed.json"
    )
    old_hash = json.loads(marker_file.read_text(encoding="utf-8"))["contentHash"]
    source_manifest = (
        bundle_copy / "adapters" / "qwenpaw" / "plugin.json"
    ).read_text(encoding="utf-8")
    (bundle_copy / "adapters" / "qwenpaw" / "plugin.json").write_text(
        source_manifest.replace("0.1.0", "0.1.1"), encoding="utf-8"
    )

    _run(bundle_copy / "scripts" / "install.sh", env)
    second_snapshot = _snapshot(plugin_dir)
    new_hash = json.loads(marker_file.read_text(encoding="utf-8"))["contentHash"]
    assert new_hash != old_hash
    changed = {
        rel
        for rel in second_snapshot
        if first_snapshot.get(rel) != second_snapshot[rel]
    }
    assert changed == {"plugin.json"}
    assert target_file.is_file()
    assert (
        "0.1.1"
        in target_file.read_text(encoding="utf-8")
    )


def test_update_removes_owned_file_removed_from_payload(tmp_path):
    working_dir = tmp_path / "work"
    working_dir.mkdir()
    env = _adapter_env(working_dir)

    # Work on a copy of the bundle so the test can mutate the payload.
    bundle_copy = tmp_path / "bundle"
    shutil.copytree(BUNDLE_DIR, bundle_copy)
    adapter_install = bundle_copy / "adapters" / "qwenpaw" / "install.sh"

    plugin_dir = working_dir / "plugins" / PLUGIN_DIRNAME
    marker_file = (
        working_dir / ".agentteams" / "agentteams-matrix" / "installed.json"
    )

    _run(adapter_install, env)
    removed_rel = "agentteams_matrix/__init__.py"
    assert (plugin_dir / removed_rel).is_file()
    recorded = json.loads(marker_file.read_text(encoding="utf-8"))
    assert recorded["files"][removed_rel]

    # Remove the file from the payload, then update.
    (bundle_copy / "adapters" / "qwenpaw" / removed_rel).unlink()
    _run(adapter_install, env)

    # The previously owned file is gone from the install target and from
    # the marker; the rest of the payload is intact.
    assert not (plugin_dir / removed_rel).exists()
    updated = json.loads(marker_file.read_text(encoding="utf-8"))
    assert removed_rel not in updated["files"]
    for rel in ("plugin.json", "plugin.py", "agentteams_matrix/channel.py"):
        assert (plugin_dir / rel).is_file()
        assert updated["files"][rel]


def test_uninstall_preserves_user_files(tmp_path):
    working_dir = tmp_path / "work"
    working_dir.mkdir()
    env = _adapter_env(working_dir)

    bundle_copy = tmp_path / "bundle"
    shutil.copytree(BUNDLE_DIR, bundle_copy)

    plugin_dir = working_dir / "plugins" / PLUGIN_DIRNAME
    state_dir = working_dir / ".agentteams" / "agentteams-matrix"

    _run(bundle_copy / "adapters" / "qwenpaw" / "install.sh", env)
    assert (plugin_dir / "plugin.json").is_file()

    # A user file inside the plugin directory is not installer-owned.
    user_file = plugin_dir / "notes.txt"
    user_file.write_text("keep me\n", encoding="utf-8")

    _run(bundle_copy / "adapters" / "qwenpaw" / "uninstall.sh", env)

    assert not (state_dir / "installed.json").exists()
    assert not plugin_dir.exists() or not any(
        p.name != "notes.txt" for p in plugin_dir.rglob("*")
    )
    assert user_file.is_file(), (
        "uninstall removed a file the installer never wrote"
    )
    assert plugin_dir.is_dir(), (
        "plugin directory removed although it still holds user files"
    )
