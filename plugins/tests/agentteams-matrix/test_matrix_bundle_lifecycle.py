"""L3/L4 lifecycle conformance for the agentteams-matrix bundle (P-b).

``L3`` — install, load and uninstall every runtime adapter on a throwaway
HOME (no live runtime, no host state, no residue afterwards).

``L4`` — the release-to-release lifecycle of the marker-based adapters:
install the N-1 payload, upgrade to N, roll back to N-1. At every hop the
installed tree and the ownership marker must agree with the payload that was
just installed — files that only the old release carried must disappear on
upgrade and come back on rollback.

Refs #1427 (bundle lifecycle). Run the whole gate with ``make verify-bundle``.
"""

from __future__ import annotations

import os
import re
import subprocess
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from _bundle_lifecycle import (  # noqa: E402
    REPO_ROOT,
    compile_python_tree,
    files,
    isolated_env,
    read_json,
    read_yaml,
    residue,
    run,
    stage_bundle,
)

BUNDLE = REPO_ROOT / "plugins" / "agentteams-matrix"
INSTALL = BUNDLE / "scripts" / "install.sh"
UNINSTALL = BUNDLE / "scripts" / "uninstall.sh"

QWENPAW_PLUGIN = Path("plugins") / "agentteams-matrix-channel"
HERMES_PLUGIN = Path("plugins") / "agentteams-matrix"
MARKER = Path(".agentteams") / "agentteams-matrix" / "installed.json"

LEGACY_FILE = "legacy_only.py"
NEW_FILE = "new_only.py"


def _fake_runtime(bin_dir: Path, name: str) -> None:
    """A stand-in binary so runtime detection sees the adapter's runtime."""
    binary = bin_dir / name
    binary.write_text("#!/usr/bin/env bash\nexit 0\n", encoding="utf-8")
    binary.chmod(0o755)


def _import_installed_channel(plugin_dir: Path) -> None:
    """Import the installed channel module, as the runtime does at load time.

    A missing third-party runtime dependency (matrix-nio) is an environment
    gap, not a payload defect, so it skips with the reason instead of failing;
    anything else — including a missing payload module — fails the test.
    """
    imported = subprocess.run(
        [sys.executable, "-c", "import agentteams_matrix.channel"],
        cwd=str(plugin_dir),
        env={**os.environ, "PYTHONPATH": str(plugin_dir)},
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    if imported.returncode == 0:
        return
    missing = re.search(r"ModuleNotFoundError: No module named '([^']+)'", imported.stderr)
    payload_roots = {"agentteams_matrix", "core"}
    if missing and missing.group(1).split(".")[0] not in payload_roots:
        pytest.skip(
            f"third-party runtime dependency {missing.group(1)!r} is not installed; "
            "the installed payload itself is staged (see the marker check)"
        )
    raise AssertionError(
        "installed plugin fails to import:\n" + imported.stderr
    )


def _qwenpaw_env(home: Path, work: Path, bin_dir: Path) -> dict:
    base_path = os.environ.get("PATH", "")
    return isolated_env(
        home,
        QWENPAW_WORKING_DIR=str(work),
        PATH=f"{bin_dir}{os.pathsep}{base_path}",
    )


def _installer_residue(root: Path) -> list:
    """Files left behind, ignoring bytecode the runtime generated itself.

    ``__pycache__`` appears when the runtime imports the installed plugin; it
    is runtime output, not an installer-owned file, so uninstall is not
    expected to remove it (the marker only ever lists installer-owned files).
    """
    return [
        path
        for path in residue(root)
        if "__pycache__" not in Path(path).parts
    ]


def _assert_marker_matches_tree(marker_path: Path, plugin_dir: Path) -> None:
    marker = read_json(marker_path)
    owned = set(marker["files"])
    on_disk = set(files(plugin_dir))
    assert owned == on_disk, (
        "ownership marker and installed tree disagree: "
        f"only in marker={sorted(owned - on_disk)}, "
        f"only on disk={sorted(on_disk - owned)}"
    )
    assert marker["schemaVersion"] >= 1
    assert marker["contentHash"]


# ---------------------------------------------------------------------------
# L3 — isolated install / load / uninstall, one test per adapter
# ---------------------------------------------------------------------------
def test_l3_qwenpaw_install_load_uninstall(tmp_path: Path) -> None:
    home = tmp_path / "home"
    home.mkdir()
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    _fake_runtime(bin_dir, "qwenpaw")
    work = home / ".qwenpaw"
    env = _qwenpaw_env(home, work, bin_dir)

    run(INSTALL, "--adapter", "qwenpaw", env=env)

    plugin_dir = work / QWENPAW_PLUGIN
    assert (plugin_dir / "plugin.json").is_file()
    assert (plugin_dir / "agentteams_matrix" / "channel.py").is_file()
    assert (plugin_dir / "core" / "sync.py").is_file(), (
        "the shared core package must ship with the plugin (load-time import)"
    )
    _assert_marker_matches_tree(work / MARKER, plugin_dir)

    # Load: the installed channel module imports from the installed layout.
    _import_installed_channel(plugin_dir)

    run(UNINSTALL, "--adapter", "qwenpaw", env=env)
    assert _installer_residue(work) == [], (
        "uninstall left installer-owned files behind"
    )


def test_l3_openclaw_projects_contract_and_uninstall_keeps_foreign_keys(
    tmp_path: Path,
) -> None:
    home = tmp_path / "home"
    home.mkdir()
    config = home / "openclaw.json"
    config.write_text(
        """{
          "theme": "dark",
          "channels": {
            "telegram": {"enabled": true},
            "matrix": {
              "userNote": "keep",
              "groups": {"!foreign:example.org": {"enabled": true}}
            }
          }
        }
""",
        encoding="utf-8",
    )
    env = isolated_env(home, OPENCLAW_CONFIG_PATH=str(config))

    run(INSTALL, "--adapter", "openclaw", env=env)

    contract = read_yaml(BUNDLE / "contract.yaml")["matrix"]
    document = read_json(config)
    matrix = document["channels"]["matrix"]
    # The projection is the contract in the runtime's shape.
    assert matrix["enabled"] is True
    assert matrix["homeserver"] == contract["homeserver"]
    assert matrix["userId"] == contract["user_id"]
    assert matrix["historyLimit"] == contract["history_limit"]
    assert matrix["autoJoin"] == "allowlist"
    assert matrix["autoJoinAllowlist"] == contract["rooms"]
    assert matrix["dm"] == {
        "policy": "allowlist",
        "allowFrom": contract["allow_from"],
    }
    assert matrix["groupPolicy"] == "allowlist"
    assert matrix["groupAllowFrom"] == contract["group_allow_from"]
    for room in contract["rooms"]:
        assert matrix["groups"][room] == {
            "requireMention": contract["require_mention"]
        }, f"contract room {room!r} was not projected"
    assert document["theme"] == "dark"
    assert document["channels"]["telegram"] == {"enabled": True}
    # Keys the adapter does not own survive the projection untouched.
    assert matrix["userNote"] == "keep"
    assert matrix["groups"]["!foreign:example.org"] == {"enabled": True}

    run(UNINSTALL, "--adapter", "openclaw", env=env)

    document = read_json(config)
    matrix = document["channels"]["matrix"]
    for key in (
        "enabled",
        "homeserver",
        "userId",
        "historyLimit",
        "autoJoin",
        "autoJoinAllowlist",
        "dm",
        "groupPolicy",
        "groupAllowFrom",
    ):
        assert key not in matrix, f"adapter-owned key {key!r} survived the uninstall"
    assert matrix["userNote"] == "keep"
    assert matrix["groups"] == {"!foreign:example.org": {"enabled": True}}
    assert document["channels"]["telegram"] == {"enabled": True}
    assert residue(home) == ["openclaw.json"]


def test_l3_hermes_install_load_uninstall(tmp_path: Path) -> None:
    home = tmp_path / "home"
    home.mkdir()
    hermes_home = home / ".hermes"
    env = isolated_env(home, HERMES_HOME=str(hermes_home))

    run(INSTALL, "--adapter", "hermes", env=env)

    plugin_dir = hermes_home / HERMES_PLUGIN
    manifest = read_yaml(plugin_dir / "plugin.yaml")
    assert manifest["kind"] == "platform"
    assert manifest["requires_hermes"].startswith(">=")
    assert (plugin_dir / "__init__.py").is_file()
    assert (plugin_dir / "adapter.py").is_file()
    assert (plugin_dir / "core" / "sync.py").is_file()
    _assert_marker_matches_tree(hermes_home / MARKER, plugin_dir)

    # Load: the payload the Hermes plugin loader receives must compile.
    compile_python_tree(plugin_dir)

    run(UNINSTALL, "--adapter", "hermes", env=env)
    assert _installer_residue(hermes_home) == [], (
        "uninstall left installer-owned files behind"
    )


def test_l3_uninstall_without_adapter_cleans_every_detected_runtime(
    tmp_path: Path,
) -> None:
    home = tmp_path / "home"
    home.mkdir()
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    for runtime in ("qwenpaw", "openclaw", "hermes"):
        _fake_runtime(bin_dir, runtime)
    work = home / ".qwenpaw"
    hermes_home = home / ".hermes"
    config = home / "openclaw.json"
    config.write_text("{}\n", encoding="utf-8")
    env = _qwenpaw_env(home, work, bin_dir)
    env.update({"HERMES_HOME": str(hermes_home), "OPENCLAW_CONFIG_PATH": str(config)})

    for adapter in ("qwenpaw", "openclaw", "hermes"):
        run(INSTALL, "--adapter", adapter, env=env)

    run(UNINSTALL, env=env)  # no --adapter: every detected runtime

    assert _installer_residue(work) == []
    assert _installer_residue(hermes_home) == []
    assert read_json(config).get("channels", {}).get("matrix") is None


def test_l4_marker_guard_repairs_drift_and_keeps_user_files(tmp_path: Path) -> None:
    """A tree that drifted from the marker is repaired, never reported as current.

    This is the installer's state invariant: re-running against an unchanged
    payload is a no-op, but a file the marker owns that went missing or was
    edited (a native uninstall, an operator edit) is restored from the
    payload — and a file the installer never wrote survives uninstall.
    """
    home = tmp_path / "home"
    home.mkdir()
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    _fake_runtime(bin_dir, "qwenpaw")
    work = home / ".qwenpaw"
    env = _qwenpaw_env(home, work, bin_dir)
    plugin_dir = work / QWENPAW_PLUGIN
    marker_path = work / MARKER

    run(INSTALL, "--adapter", "qwenpaw", env=env)
    installed = files(plugin_dir)
    assert installed

    # Unchanged payload: a no-op that says so, with no file rewritten.
    repeat = run(INSTALL, "--adapter", "qwenpaw", env=env)
    assert "up to date" in repeat.stdout.lower(), repeat.stdout
    assert files(plugin_dir) == installed

    victim = plugin_dir / "core" / "sync.py"
    victim.unlink()
    corrupted = plugin_dir / "core" / "state.py"
    corrupted.write_text("# corrupted by the runtime\n", encoding="utf-8")

    repaired = run(INSTALL, "--adapter", "qwenpaw", env=env)
    assert "up to date" not in repaired.stdout.lower(), (
        "the installer called a drifted tree up to date"
    )
    assert victim.is_file(), "reinstall did not restore a deleted owned file"
    assert files(plugin_dir) == installed, (
        "reinstall left the tree different from the payload it installed"
    )
    _assert_marker_matches_tree(marker_path, plugin_dir)

    user_file = plugin_dir / "notes.txt"
    user_file.write_text("user file\n", encoding="utf-8")
    run(UNINSTALL, "--adapter", "qwenpaw", env=env)
    assert user_file.is_file(), "uninstall removed a file it never wrote"
    assert sorted(files(plugin_dir)) == ["notes.txt"]


# ---------------------------------------------------------------------------
# L4 — N-1 install -> N upgrade -> N-1 rollback
# ---------------------------------------------------------------------------
@pytest.mark.parametrize(
    "adapter,plugin_rel,marker_root_name",
    [
        ("qwenpaw", QWENPAW_PLUGIN, "work"),
        ("hermes", HERMES_PLUGIN, "hermes"),
    ],
)
def test_l4_upgrade_and_rollback_follow_the_payload(
    tmp_path: Path, adapter: str, plugin_rel: Path, marker_root_name: str
) -> None:
    home = tmp_path / "home"
    home.mkdir()
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    for runtime in ("qwenpaw", "openclaw", "hermes"):
        _fake_runtime(bin_dir, runtime)
    work = home / ".qwenpaw"
    hermes_home = home / ".hermes"
    env = _qwenpaw_env(home, work, bin_dir)
    env["HERMES_HOME"] = str(hermes_home)

    previous = stage_bundle(
        BUNDLE,
        tmp_path / "bundle-n-1",
        version="0.0.9",
        add={f"adapters/{adapter}/{LEGACY_FILE}": "legacy payload\n"},
    )
    current = stage_bundle(
        BUNDLE,
        tmp_path / "bundle-n",
        version="0.1.0",
        add={f"adapters/{adapter}/{NEW_FILE}": "new payload\n"},
    )
    root = work if marker_root_name == "work" else hermes_home
    plugin_dir = root / plugin_rel

    def install(bundle: Path) -> None:
        run(bundle / "scripts" / "install.sh", "--adapter", adapter, env=env)
        _assert_marker_matches_tree(root / MARKER, plugin_dir)

    install(previous)
    assert (plugin_dir / LEGACY_FILE).is_file()
    assert not (plugin_dir / NEW_FILE).exists()

    install(current)  # upgrade
    assert (plugin_dir / NEW_FILE).is_file()
    assert not (plugin_dir / LEGACY_FILE).exists(), (
        f"upgrade to N left the N-1-only file {LEGACY_FILE} behind"
    )

    install(previous)  # rollback
    assert (plugin_dir / LEGACY_FILE).is_file()
    assert not (plugin_dir / NEW_FILE).exists(), (
        f"rollback to N-1 left the N-only file {NEW_FILE} behind"
    )

    run(previous / "scripts" / "uninstall.sh", "--adapter", adapter, env=env)
    assert _installer_residue(root) == []
