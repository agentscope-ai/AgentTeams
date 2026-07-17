"""Tests for bridge.py — template-create + controller-field overlay model.

On every bridge invocation two things happen:

1. **create phase** — any of {config.json, workspaces/<agent>/agent.json,
   providers.json} that is missing is installed verbatim from an in-tree
   template. Templates carry all defaults (identity, security off,
   channels.console enabled, etc.).

2. **restart-overlay phase** — ``_CONTROLLER_FIELDS`` refreshes only the
   fields Controller genuinely owns (Matrix scalars, running.max_input_length,
   running.embedding_config, heartbeat). Everything else — user edits,
   CoPaw migration writes — is left alone.

Three merge policies cover the controller fields: ``remote-wins`` (scalar
overwrite), ``union`` (list dedup), ``deep-merge`` (local-wins-at-leaves for
``channels.matrix.groups``). ``env`` is never bridged.
"""

import json
import os
import tempfile
from pathlib import Path

import pytest

from copaw_worker.bridge import (
    bridge_openclaw_to_copaw,
    bridge_runtime_to_standard,
)


# ---------------------------------------------------------------------------
# Fixtures / helpers
# ---------------------------------------------------------------------------

def _make_openclaw_cfg(**memory_search_overrides):
    """Helper to build an openclaw config with optional memorySearch overrides."""
    base = {
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
    if memory_search_overrides is not None:
        base["agents"]["defaults"]["memorySearch"] = {
            "provider": "openai",
            "model": "text-embedding-v4",
            "remote": {
                "baseUrl": "http://aigw:8080/v1",
                "apiKey": "key123",
            },
            **memory_search_overrides,
        }
    return base


def _agent_json_path(working_dir: Path, agent: str = "default") -> Path:
    return working_dir / "workspaces" / agent / "agent.json"


def _run_bridge(cfg, working_dir: Path, **kwargs):
    bridge_openclaw_to_copaw(cfg, working_dir, **kwargs)


def _read_agent(working_dir: Path, agent: str = "default"):
    with open(_agent_json_path(working_dir, agent)) as f:
        return json.load(f)


def _bridge_and_read_agent(cfg, **kwargs):
    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"
        _run_bridge(cfg, working_dir, **kwargs)
        return _read_agent(working_dir)


# ---------------------------------------------------------------------------
# Template create phase
# ---------------------------------------------------------------------------

def test_create_installs_config_json_from_template():
    """On first boot bridge writes config.json from the template."""
    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"
        _run_bridge(_make_openclaw_cfg(), working_dir)

        cfg_path = working_dir / "config.json"
        assert cfg_path.exists()
        cfg = json.loads(cfg_path.read_text())
        # Security defaults: all three guards disabled.
        assert cfg["security"]["tool_guard"]["enabled"] is False
        assert cfg["security"]["file_guard"]["enabled"] is False
        assert cfg["security"]["skill_scanner"]["mode"] == "off"


def test_create_installs_worker_agent_json_from_template():
    """Worker profile seeds agent.json from agent.worker.json."""
    agent = _bridge_and_read_agent(_make_openclaw_cfg(), profile="worker")

    assert agent["id"] == "default"
    assert agent["name"] == "Default Agent"
    assert agent["language"] == "zh"
    assert agent["system_prompt_files"] == ["AGENTS.md", "SOUL.md", "PROFILE.md"]
    # Bridge overlay disables console (Matrix-only workers).
    assert agent["channels"]["console"]["enabled"] is False
    assert agent["channels"]["matrix"]["filter_tool_messages"] is True
    assert agent["channels"]["matrix"]["filter_thinking"] is True
    # Manager-only fields absent.
    assert "require_mention" not in agent["channels"]["matrix"]
    assert "require_approval" not in agent.get("running", {})


def test_create_installs_manager_agent_json_from_template(monkeypatch):
    """Manager profile seeds agent.json from agent.manager.json."""
    monkeypatch.setenv("AGENTTEAMS_MATRIX_DOMAIN", "matrix.example.org")
    monkeypatch.setenv("AGENTTEAMS_WORKER_NAME", "manager")

    agent = _bridge_and_read_agent(_make_openclaw_cfg(), profile="manager")

    assert agent["name"] == "Manager"
    assert agent["system_prompt_files"] == [
        "AGENTS.md", "SOUL.md", "PROFILE.md", "TOOLS.md",
    ]
    assert agent["channels"]["matrix"]["require_mention"] is True
    assert agent["channels"]["matrix"]["filter_tool_messages"] is True
    assert agent["channels"]["matrix"]["filter_thinking"] is True
    assert "require_approval" not in agent.get("running", {})
    assert agent["channels"]["matrix"]["user_id"] == "@manager:matrix.example.org"


def test_create_respects_custom_agent_key():
    """Non-default ``agent`` parameter writes to workspaces/<agent>/agent.json."""
    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"
        _run_bridge(_make_openclaw_cfg(), working_dir, agent="alice")
        assert (working_dir / "workspaces" / "alice" / "agent.json").exists()
        assert not (working_dir / "workspaces" / "default" / "agent.json").exists()


# ---------------------------------------------------------------------------
# User-edit preservation
# ---------------------------------------------------------------------------

def test_user_edits_to_config_json_preserved():
    """Once config.json exists, bridge never touches it — user owns it."""
    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"
        _run_bridge(_make_openclaw_cfg(), working_dir)

        cfg_path = working_dir / "config.json"
        cfg = json.loads(cfg_path.read_text())
        cfg["security"]["tool_guard"]["enabled"] = True
        cfg["user_custom"] = {"hello": "world"}
        cfg_path.write_text(json.dumps(cfg))

        _run_bridge(_make_openclaw_cfg(), working_dir)

        cfg2 = json.loads(cfg_path.read_text())
        assert cfg2["security"]["tool_guard"]["enabled"] is True
        assert cfg2["user_custom"] == {"hello": "world"}


def test_user_edits_to_agent_non_controller_fields_preserved():
    """Fields not in _CONTROLLER_FIELDS (identity, console, security, env)
    must survive re-bridge."""
    cfg = _make_openclaw_cfg()

    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"
        _run_bridge(cfg, working_dir)

        agent_path = _agent_json_path(working_dir)
        agent = json.loads(agent_path.read_text())
        agent["name"] = "My Renamed Agent"
        agent["language"] = "en"
        agent["channels"]["console"]["enabled"] = False
        agent["env"] = {"TEST_VAR": "test_value"}
        agent["custom_user_field"] = {"keep_me": True}
        agent_path.write_text(json.dumps(agent))

        _run_bridge(cfg, working_dir)

        agent2 = json.loads(agent_path.read_text())

    assert agent2["name"] == "My Renamed Agent"
    assert agent2["language"] == "en"
    assert agent2["channels"]["console"]["enabled"] is False
    assert agent2["env"] == {"TEST_VAR": "test_value"}
    assert agent2["custom_user_field"] == {"keep_me": True}


def test_agent_json_never_seeds_env_from_openclaw():
    """openclaw.env is ignored — env is agent-owned."""
    cfg = _make_openclaw_cfg()
    cfg["env"] = {"vars": {"FOO": "bar"}}

    agent = _bridge_and_read_agent(cfg)
    assert "env" not in agent


# ---------------------------------------------------------------------------
# Controller-field overlay: remote-wins
# ---------------------------------------------------------------------------

def test_remote_wins_access_token_refreshes():
    """channels.matrix.access_token rotation from controller takes effect."""
    cfg = _make_openclaw_cfg()
    cfg["channels"]["matrix"]["accessToken"] = "tok_v1"

    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"
        _run_bridge(cfg, working_dir)
        assert _read_agent(working_dir)["channels"]["matrix"]["access_token"] == "tok_v1"

        cfg["channels"]["matrix"]["accessToken"] = "tok_v2"
        _run_bridge(cfg, working_dir)
        assert _read_agent(working_dir)["channels"]["matrix"]["access_token"] == "tok_v2"


def test_remote_wins_matrix_stream_filters_use_defaults():
    """Bridge refresh reapplies AgentTeams Matrix stream filter defaults."""
    cfg = _make_openclaw_cfg()

    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"
        _run_bridge(cfg, working_dir, profile="worker")

        agent_path = _agent_json_path(working_dir)
        agent = json.loads(agent_path.read_text())
        agent["channels"]["matrix"]["filter_tool_messages"] = False
        agent["channels"]["matrix"]["filter_thinking"] = False
        agent_path.write_text(json.dumps(agent))

        _run_bridge(cfg, working_dir, profile="worker")

        matrix = _read_agent(working_dir)["channels"]["matrix"]
        assert matrix["filter_tool_messages"] is True
        assert matrix["filter_thinking"] is True


def test_remote_wins_matrix_stream_filters_allow_controller_override():
    cfg = _make_openclaw_cfg()
    cfg["channels"]["matrix"]["filterToolMessages"] = True
    cfg["channels"]["matrix"]["filterThinking"] = False

    agent = _bridge_and_read_agent(cfg)

    matrix = agent["channels"]["matrix"]
    assert matrix["filter_tool_messages"] is True
    assert matrix["filter_thinking"] is False


def test_remote_wins_max_input_length_refreshes():
    """Controller bumping contextWindow propagates to running.max_input_length."""
    cfg = _make_openclaw_cfg()
    cfg["models"]["providers"]["gw"]["models"][0]["contextWindow"] = 4096

    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"
        _run_bridge(cfg, working_dir)
        assert _read_agent(working_dir)["running"]["max_input_length"] == 4096

        cfg["models"]["providers"]["gw"]["models"][0]["contextWindow"] = 8192
        _run_bridge(cfg, working_dir)
        assert _read_agent(working_dir)["running"]["max_input_length"] == 8192


def test_embedding_config_from_memory_search():
    """memorySearch in openclaw → agent.json running.embedding_config."""
    agent = _bridge_and_read_agent(_make_openclaw_cfg())

    emb = agent["running"]["embedding_config"]
    assert emb["backend"] == "openai"
    assert emb["model_name"] == "text-embedding-v4"
    assert emb["base_url"] == "http://aigw:18080/v1"  # :8080 → :18080 off-container
    assert emb["api_key"] == "key123"
    assert emb["dimensions"] == 1024


def test_embedding_config_absent_when_memory_search_missing():
    cfg = _make_openclaw_cfg()
    del cfg["agents"]["defaults"]["memorySearch"]
    agent = _bridge_and_read_agent(cfg)
    assert "embedding_config" not in agent.get("running", {})


def test_embedding_config_custom_dimensions():
    cfg = _make_openclaw_cfg(outputDimensionality=768)
    agent = _bridge_and_read_agent(cfg)
    assert agent["running"]["embedding_config"]["dimensions"] == 768


# ---------------------------------------------------------------------------
# Controller-field overlay: union
# ---------------------------------------------------------------------------

def test_union_allow_from_merges_cr_and_user():
    """channels.matrix.allow_from: CR entries + user additions co-exist."""
    cfg = _make_openclaw_cfg()
    cfg["channels"]["matrix"]["dm"] = {
        "policy": "allowlist",
        "allowFrom": ["@alice:example.org"],
    }

    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"
        _run_bridge(cfg, working_dir)

        agent_path = _agent_json_path(working_dir)
        agent = json.loads(agent_path.read_text())
        agent["channels"]["matrix"]["allow_from"].append("@bob:example.org")
        agent_path.write_text(json.dumps(agent))

        cfg["channels"]["matrix"]["dm"]["allowFrom"] = [
            "@alice:example.org", "@carol:example.org",
        ]
        _run_bridge(cfg, working_dir)
        agent = _read_agent(working_dir)

    allow_from = agent["channels"]["matrix"]["allow_from"]
    assert set(allow_from) == {"@alice:example.org", "@bob:example.org", "@carol:example.org"}
    assert allow_from.count("@alice:example.org") == 1  # dedup


# ---------------------------------------------------------------------------
# Controller-field overlay: deep-merge (channels.matrix.groups)
# ---------------------------------------------------------------------------

def test_deep_merge_groups_preserves_user_override():
    """channels.matrix.groups: user leaf edits survive; controller may only
    add new leaves the agent doesn't have yet."""
    cfg = _make_openclaw_cfg()
    cfg["channels"]["matrix"]["groups"] = {
        "*": {"requireMention": True, "historyLimit": 50},
    }

    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"
        _run_bridge(cfg, working_dir)

        agent_path = _agent_json_path(working_dir)
        agent = json.loads(agent_path.read_text())
        assert agent["channels"]["matrix"]["groups"]["*"]["historyLimit"] == 50

        agent["channels"]["matrix"]["groups"]["*"]["requireMention"] = False
        agent["channels"]["matrix"]["groups"]["!room:example.org"] = {
            "requireMention": False,
        }
        agent_path.write_text(json.dumps(agent))

        cfg["channels"]["matrix"]["groups"]["*"]["historyLimit"] = 200
        cfg["channels"]["matrix"]["groups"]["*"]["newFlag"] = True
        _run_bridge(cfg, working_dir)
        agent = _read_agent(working_dir)

    groups = agent["channels"]["matrix"]["groups"]
    assert groups["*"]["requireMention"] is False  # user override kept
    assert groups["*"]["historyLimit"] == 50  # existing leaf NOT overwritten
    assert groups["*"]["newFlag"] is True  # new leaf added
    assert groups["!room:example.org"] == {"requireMention": False}


# ---------------------------------------------------------------------------
# Identity / user_id derivation
# ---------------------------------------------------------------------------

def test_worker_user_id_from_openclaw():
    """Worker carries userId in openclaw.json — bridge writes it verbatim."""
    cfg = _make_openclaw_cfg()
    cfg["channels"]["matrix"]["userId"] = "@dmd:matrix-local.agentteams.io:18080"

    agent = _bridge_and_read_agent(cfg)
    assert agent["channels"]["matrix"]["user_id"] == "@dmd:matrix-local.agentteams.io:18080"


def test_manager_user_id_from_openclaw_wins_over_env(monkeypatch):
    monkeypatch.setenv("AGENTTEAMS_MATRIX_DOMAIN", "other.example.org")
    cfg = _make_openclaw_cfg()
    cfg["channels"]["matrix"]["userId"] = "@explicit:explicit.example.org"

    agent = _bridge_and_read_agent(cfg, profile="manager")
    assert agent["channels"]["matrix"]["user_id"] == "@explicit:explicit.example.org"


# ---------------------------------------------------------------------------
# Heartbeat (template seed + controller fallback seed)
# ---------------------------------------------------------------------------

def test_manager_template_heartbeat_wins_over_openclaw_seed(monkeypatch):
    monkeypatch.setenv("AGENTTEAMS_MATRIX_DOMAIN", "matrix.example.org")

    cfg = _make_openclaw_cfg()
    cfg["agents"]["defaults"]["heartbeat"] = {
        "every": "5m",
        "target": "self",
        "activeHours": "09:00-18:00",
    }

    agent = _bridge_and_read_agent(cfg, profile="manager")
    assert agent["heartbeat"] == {"enabled": True, "every": "10m"}


def test_worker_template_seeds_default_heartbeat_when_openclaw_silent():
    agent = _bridge_and_read_agent(_make_openclaw_cfg(), profile="worker")
    assert agent["heartbeat"] == {"enabled": True, "every": "10m"}


def test_openclaw_heartbeat_seeds_existing_agent_without_heartbeat():
    cfg = _make_openclaw_cfg()
    cfg["agents"]["defaults"]["heartbeat"] = {
        "every": "5m",
        "target": "self",
        "activeHours": "09:00-18:00",
    }

    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"
        _run_bridge(_make_openclaw_cfg(), working_dir)
        agent_path = _agent_json_path(working_dir)
        agent = json.loads(agent_path.read_text())
        agent.pop("heartbeat")
        agent_path.write_text(json.dumps(agent))

        _run_bridge(cfg, working_dir)
        agent = _read_agent(working_dir)

    assert agent["heartbeat"] == {
        "enabled": True,
        "every": "5m",
        "target": "self",
        "active_hours": "09:00-18:00",
    }


# ---------------------------------------------------------------------------
# Validation
# ---------------------------------------------------------------------------

def test_bridge_rejects_unknown_profile():
    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"
        with pytest.raises(ValueError, match="unknown bridge profile"):
            bridge_openclaw_to_copaw(_make_openclaw_cfg(), working_dir, profile="leader")


def test_bridge_openclaw_to_copaw_alias_still_works():
    """Deprecated alias must still dispatch to the new entry point."""
    from copaw_worker.bridge import bridge_openclaw_to_copaw

    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"
        bridge_openclaw_to_copaw(_make_openclaw_cfg(), working_dir)
        assert _agent_json_path(working_dir).exists()
        assert (working_dir / "config.json").exists()


# ---------------------------------------------------------------------------
# Runtime -> standard materialization
# ---------------------------------------------------------------------------
# NOTE: The former standard->runtime sync tests (sync_outer_prompt_files_to_inner,
# refresh_standard_to_runtime, sync_mcporter_config_to_runtime, sync_skills_to_runtime,
# bridge_standard_to_runtime) were removed: those symbols no longer exist in
# copaw_worker.bridge. Only the reverse direction (bridge_runtime_to_standard) remains.

def test_bridge_runtime_to_standard_copies_newer_prompt_edits(tmp_path):
    """Agent-edited runtime prompts are materialized back to the sync root."""
    standard_dir = tmp_path / "standard"
    workspace_dir = standard_dir / ".copaw" / "workspaces" / "default"
    workspace_dir.mkdir(parents=True)
    outer = standard_dir / "AGENTS.md"
    inner = workspace_dir / "AGENTS.md"
    outer.write_text("outer")
    inner.write_text("inner")
    os.utime(outer, (1, 1))
    os.utime(inner, (2, 2))

    bridge_runtime_to_standard(standard_dir)

    assert outer.read_text() == "inner"


def test_bridge_runtime_to_standard_keeps_newer_or_same_age_outer_prompts(tmp_path):
    """Runtime prompts only win when they are strictly newer than standard space."""
    standard_dir = tmp_path / "standard"
    workspace_dir = standard_dir / ".copaw" / "workspaces" / "default"
    workspace_dir.mkdir(parents=True)
    outer = standard_dir / "AGENTS.md"
    inner = workspace_dir / "AGENTS.md"
    outer.write_text("outer")
    inner.write_text("inner")

    os.utime(outer, (2, 2))
    os.utime(inner, (1, 1))
    bridge_runtime_to_standard(standard_dir)
    assert outer.read_text() == "outer"

    os.utime(outer, (2, 2))
    os.utime(inner, (2, 2))
    bridge_runtime_to_standard(standard_dir)
    assert outer.read_text() == "outer"
