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
import sys
from pathlib import Path

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

HERMES_PLUGIN = Path("plugins") / "agentteams-matrix"
MARKER = Path(".agentteams") / "agentteams-matrix" / "installed.json"

LEGACY_FILE = "legacy_only.py"
NEW_FILE = "new_only.py"


def _fake_runtime(bin_dir: Path, name: str) -> None:
    """A stand-in binary so runtime detection sees the adapter's runtime."""
    binary = bin_dir / name
    binary.write_text("#!/usr/bin/env bash\nexit 0\n", encoding="utf-8")
    binary.chmod(0o755)


def _installer_residue(root: Path) -> list:
    return [path for path in residue(root) if "__pycache__" not in Path(path).parts]


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
    for runtime in ("openclaw", "hermes"):
        _fake_runtime(bin_dir, runtime)
    hermes_home = home / ".hermes"
    config = home / "openclaw.json"
    config.write_text("{}\n", encoding="utf-8")
    env = isolated_env(home, PATH=f"{bin_dir}{os.pathsep}{os.environ.get('PATH', '')}")
    env.update({"HERMES_HOME": str(hermes_home), "OPENCLAW_CONFIG_PATH": str(config)})

    for adapter in ("openclaw", "hermes"):
        run(INSTALL, "--adapter", adapter, env=env)

    run(UNINSTALL, env=env)  # no --adapter: every detected runtime

    assert _installer_residue(hermes_home) == []
    assert read_json(config).get("channels", {}).get("matrix") is None


# ---------------------------------------------------------------------------
# L4 — N-1 install -> N upgrade -> N-1 rollback
# ---------------------------------------------------------------------------
def test_l4_upgrade_and_rollback_follow_the_payload(tmp_path: Path) -> None:
    adapter = "hermes"
    home = tmp_path / "home"
    home.mkdir()
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    for runtime in ("openclaw", "hermes"):
        _fake_runtime(bin_dir, runtime)
    hermes_home = home / ".hermes"
    env = isolated_env(home, PATH=f"{bin_dir}{os.pathsep}{os.environ.get('PATH', '')}")
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
    root = hermes_home
    plugin_dir = root / HERMES_PLUGIN

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
