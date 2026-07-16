"""Tests for AGENTTEAMS_MANAGER_HEARTBEAT_INTERVAL -> agent.json heartbeat.every.

The controller (hiclaw-controller/internal/service/worker_env.go) sets
AGENTTEAMS_MANAGER_HEARTBEAT_INTERVAL on the Manager container. The CoPaw side
of that pipeline is copaw_worker.bridge.bridge_openclaw_to_copaw ->
_write_agent_json, which materializes workspaces/default/agent.json from
templates/agent.manager.json (hardcoded "every": "30m") and must apply the
env override for the manager profile, defaulting to "10m" when unset.

This intentionally lives outside test_bridge.py: that file imports the
nonexistent `bridge_controller_to_copaw` symbol and fails collection
entirely (pre-existing, unrelated breakage — see baseline).
"""

import json
import tempfile
from pathlib import Path

import pytest

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


def _agent_json_path(working_dir: Path) -> dict:
    path = working_dir / "workspaces" / "default" / "agent.json"
    with open(path) as f:
        return json.load(f)


def test_manager_heartbeat_defaults_to_10m_when_env_unset(monkeypatch):
    monkeypatch.delenv("AGENTTEAMS_MANAGER_HEARTBEAT_INTERVAL", raising=False)

    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"
        bridge_openclaw_to_copaw(_make_openclaw_cfg(), working_dir, profile="manager")
        agent = _agent_json_path(working_dir)

    assert agent["heartbeat"]["every"] == "10m"
    assert agent["heartbeat"]["enabled"] is True


def test_manager_heartbeat_honors_env_override(monkeypatch):
    monkeypatch.setenv("AGENTTEAMS_MANAGER_HEARTBEAT_INTERVAL", "5m")

    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"
        bridge_openclaw_to_copaw(_make_openclaw_cfg(), working_dir, profile="manager")
        agent = _agent_json_path(working_dir)

    assert agent["heartbeat"]["every"] == "5m"


def test_manager_heartbeat_env_wins_over_stale_agent_json(monkeypatch):
    """A pre-existing agent.json (e.g. from before this env existed, or with
    the old hardcoded 30m) must be corrected on the next bridge run, not left
    stale — this is a restart-time overlay, not a create-only seed.
    """
    monkeypatch.setenv("AGENTTEAMS_MANAGER_HEARTBEAT_INTERVAL", "15m")

    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"
        # First run seeds the file (env unset at this point simulates the old
        # hardcoded-30m template state by asserting the seed, then we flip
        # the env and re-run to prove the overlay updates it).
        monkeypatch.delenv("AGENTTEAMS_MANAGER_HEARTBEAT_INTERVAL", raising=False)
        bridge_openclaw_to_copaw(_make_openclaw_cfg(), working_dir, profile="manager")
        seeded = _agent_json_path(working_dir)
        assert seeded["heartbeat"]["every"] == "10m"

        monkeypatch.setenv("AGENTTEAMS_MANAGER_HEARTBEAT_INTERVAL", "15m")
        bridge_openclaw_to_copaw(_make_openclaw_cfg(), working_dir, profile="manager")
        updated = _agent_json_path(working_dir)

    assert updated["heartbeat"]["every"] == "15m"


def test_worker_profile_unaffected_by_manager_heartbeat_env(monkeypatch):
    """The env var is manager-specific; the worker profile keeps its own
    template default (already 10m in templates/agent.worker.json) regardless
    of AGENTTEAMS_MANAGER_HEARTBEAT_INTERVAL.
    """
    monkeypatch.setenv("AGENTTEAMS_MANAGER_HEARTBEAT_INTERVAL", "99m")

    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"
        bridge_openclaw_to_copaw(_make_openclaw_cfg(), working_dir, profile="worker")
        agent = _agent_json_path(working_dir)

    assert agent["heartbeat"]["every"] == "10m"
