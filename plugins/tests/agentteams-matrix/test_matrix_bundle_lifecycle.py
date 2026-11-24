"""OpenClaw projector lifecycle conformance on a throwaway HOME.

The retired Hermes adapter's payload upgrade tests now belong to its Harness.

Refs #1427 (bundle lifecycle). Run the whole gate with ``make verify-bundle``.
"""

from __future__ import annotations

import os
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from _bundle_lifecycle import (  # noqa: E402
    REPO_ROOT,
    isolated_env,
    read_json,
    read_yaml,
    residue,
    run,
)

BUNDLE = REPO_ROOT / "plugins" / "agentteams-matrix"
INSTALL = BUNDLE / "scripts" / "install.sh"
UNINSTALL = BUNDLE / "scripts" / "uninstall.sh"


def _fake_runtime(bin_dir: Path, name: str) -> None:
    """A stand-in binary so runtime detection sees the adapter's runtime."""
    binary = bin_dir / name
    binary.write_text("#!/usr/bin/env bash\nexit 0\n", encoding="utf-8")
    binary.chmod(0o755)


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


def test_l3_uninstall_without_adapter_cleans_every_detected_runtime(
    tmp_path: Path,
) -> None:
    home = tmp_path / "home"
    home.mkdir()
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    for runtime in ("openclaw",):
        _fake_runtime(bin_dir, runtime)
    config = home / "openclaw.json"
    config.write_text("{}\n", encoding="utf-8")
    env = isolated_env(home, PATH=f"{bin_dir}{os.pathsep}{os.environ.get('PATH', '')}")
    env["OPENCLAW_CONFIG_PATH"] = str(config)

    for adapter in ("openclaw",):
        run(INSTALL, "--adapter", adapter, env=env)

    run(UNINSTALL, env=env)  # no --adapter: every detected runtime

    assert read_json(config).get("channels", {}).get("matrix") is None
