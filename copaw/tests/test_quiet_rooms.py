"""Tests for HICLAW_QUIET_ROOMS -> config.json root show_tool_details.

Phase 5b "quiet rooms" (mechanism only, env-gated default-off): when
HICLAW_QUIET_ROOMS is truthy, copaw_worker.bridge._write_config_json (via
bridge_openclaw_to_copaw) additionally writes a root-level
"show_tool_details": false into config.json — the tap onto
copaw/src/matrix/config.py:1164 (default True, never set anywhere today).
The channels.matrix.{filter_tool_messages,filter_thinking} flags are
untouched either way.

When the env is unset (default), config.json output must be byte-identical
to before this flag existed — regression-tested here.

This intentionally lives outside test_bridge.py: that file imports the
nonexistent `bridge_controller_to_copaw` symbol and fails collection
entirely (pre-existing, unrelated breakage — see baseline). Never add
tests to test_bridge.py.
"""

import json
import tempfile
from pathlib import Path

from copaw_worker.bridge import bridge_openclaw_to_copaw


def _make_openclaw_cfg():
    return {
        "channels": {
            "matrix": {
                "enabled": True,
                "homeserver": "http://localhost:6167",
                "accessToken": "tok",
            }
        },
        "models": {
            "providers": {
                "gw": {
                    "baseUrl": "http://aigw:8080/v1",
                    "apiKey": "key123",
                    "models": [{"id": "qwen3.5-plus", "name": "qwen3.5-plus"}],
                }
            }
        },
        "agents": {"defaults": {"model": {"primary": "gw/qwen3.5-plus"}}},
    }


def _config_json_path(working_dir: Path) -> dict:
    with open(working_dir / "config.json") as f:
        return json.load(f)


def test_quiet_rooms_unset_leaves_show_tool_details_absent(monkeypatch):
    monkeypatch.delenv("HICLAW_QUIET_ROOMS", raising=False)

    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"
        bridge_openclaw_to_copaw(_make_openclaw_cfg(), working_dir, profile="manager")
        config = _config_json_path(working_dir)

    assert "show_tool_details" not in config
    assert config["channels"]["matrix"]["filter_tool_messages"] is True
    assert config["channels"]["matrix"]["filter_thinking"] is True


def test_quiet_rooms_false_leaves_show_tool_details_absent(monkeypatch):
    monkeypatch.setenv("HICLAW_QUIET_ROOMS", "false")

    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"
        bridge_openclaw_to_copaw(_make_openclaw_cfg(), working_dir, profile="manager")
        config = _config_json_path(working_dir)

    assert "show_tool_details" not in config


def test_quiet_rooms_truthy_sets_root_show_tool_details_false(monkeypatch):
    monkeypatch.setenv("HICLAW_QUIET_ROOMS", "true")

    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"
        bridge_openclaw_to_copaw(_make_openclaw_cfg(), working_dir, profile="manager")
        config = _config_json_path(working_dir)

    assert config["show_tool_details"] is False
    # The channel-dict flags are kept as-is alongside the new root flag.
    assert config["channels"]["matrix"]["filter_tool_messages"] is True
    assert config["channels"]["matrix"]["filter_thinking"] is True


def test_quiet_rooms_accepts_common_truthy_spellings(monkeypatch):
    for value in ("1", "yes", "on", "TRUE", "True"):
        monkeypatch.setenv("HICLAW_QUIET_ROOMS", value)

        with tempfile.TemporaryDirectory() as tmpdir:
            working_dir = Path(tmpdir) / "agent"
            bridge_openclaw_to_copaw(
                _make_openclaw_cfg(), working_dir, profile="manager"
            )
            config = _config_json_path(working_dir)

        assert config["show_tool_details"] is False, f"failed for {value!r}"


def test_quiet_rooms_env_unset_output_byte_identical_across_runs(monkeypatch):
    """Regression guard: with the env unset, re-bridging (restart-time
    overlay) must not introduce show_tool_details or otherwise change the
    matrix channel block shape from before this flag existed.
    """
    monkeypatch.delenv("HICLAW_QUIET_ROOMS", raising=False)

    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"
        bridge_openclaw_to_copaw(_make_openclaw_cfg(), working_dir, profile="manager")
        first = (working_dir / "config.json").read_text()

        bridge_openclaw_to_copaw(_make_openclaw_cfg(), working_dir, profile="manager")
        second = (working_dir / "config.json").read_text()

    assert first == second
    assert "show_tool_details" not in first


def test_quiet_rooms_toggling_off_after_on_removes_root_flag(monkeypatch):
    """If an operator flips HICLAW_QUIET_ROOMS back off, the next bridge run
    must not leave a stale show_tool_details: false behind."""
    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"

        monkeypatch.setenv("HICLAW_QUIET_ROOMS", "true")
        bridge_openclaw_to_copaw(_make_openclaw_cfg(), working_dir, profile="manager")
        enabled = _config_json_path(working_dir)
        assert enabled["show_tool_details"] is False

        monkeypatch.delenv("HICLAW_QUIET_ROOMS", raising=False)
        bridge_openclaw_to_copaw(_make_openclaw_cfg(), working_dir, profile="manager")
        disabled = _config_json_path(working_dir)

    assert "show_tool_details" not in disabled
