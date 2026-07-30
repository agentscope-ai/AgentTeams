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
import stat
import tempfile
from pathlib import Path

import pytest

from copaw_worker.bridge import (
    bridge_controller_to_copaw,
    bridge_runtime_to_standard,
    bridge_standard_to_runtime,
    refresh_standard_to_runtime,
    sync_mcporter_config_to_runtime,
    sync_outer_prompt_files_to_inner,
    sync_skills_to_runtime,
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
    bridge_controller_to_copaw(cfg, working_dir, **kwargs)


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
    agent = _bridge_and_read_agent(_make_openclaw_cfg())

    assert agent["id"] == "default"
    assert agent["name"] == "Default Agent"
    assert agent["language"] == "zh"
    assert agent["system_prompt_files"] == ["AGENTS.md", "SOUL.md", "PROFILE.md"]
    # Console on by default, from template.
    assert agent["channels"]["console"]["enabled"] is True
    assert agent["channels"]["matrix"]["filter_tool_messages"] is False
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
    assert agent["channels"]["matrix"]["filter_tool_messages"] is False
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
    """Bridge applies default Matrix stream filter policy when unset."""
    cfg = _make_openclaw_cfg()

    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"
        _run_bridge(cfg, working_dir)

        agent_path = _agent_json_path(working_dir)
        agent = json.loads(agent_path.read_text())
        agent["channels"]["matrix"]["filter_tool_messages"] = True
        agent["channels"]["matrix"]["filter_thinking"] = True
        agent_path.write_text(json.dumps(agent))

        _run_bridge(cfg, working_dir)

        matrix = _read_agent(working_dir)["channels"]["matrix"]
        assert matrix["filter_tool_messages"] is False
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
    agent = _bridge_and_read_agent(_make_openclaw_cfg())
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
            bridge_controller_to_copaw(_make_openclaw_cfg(), working_dir, profile="leader")


# ---------------------------------------------------------------------------
# Standard/runtime file materialization
# ---------------------------------------------------------------------------

def test_sync_outer_prompt_files_to_inner_copies_prompts_and_seeds_heartbeat(tmp_path):
    """SOUL/AGENTS refresh every run; HEARTBEAT is copied only on first boot."""
    standard_dir = tmp_path / "standard"
    runtime_dir = tmp_path / "standard" / ".copaw"
    standard_dir.mkdir()
    (standard_dir / "SOUL.md").write_text("soul v1")
    (standard_dir / "AGENTS.md").write_text("agents v1")
    (standard_dir / "HEARTBEAT.md").write_text("heartbeat v1")

    sync_outer_prompt_files_to_inner(standard_dir, runtime_dir)
    workspace_dir = runtime_dir / "workspaces" / "default"

    assert (workspace_dir / "SOUL.md").read_text() == "soul v1"
    assert (workspace_dir / "AGENTS.md").read_text() == "agents v1"
    assert (workspace_dir / "HEARTBEAT.md").read_text() == "heartbeat v1"

    (standard_dir / "SOUL.md").write_text("soul v2")
    (standard_dir / "AGENTS.md").write_text("agents v2")
    (standard_dir / "HEARTBEAT.md").write_text("heartbeat v2")
    sync_outer_prompt_files_to_inner(standard_dir, runtime_dir)

    assert (workspace_dir / "SOUL.md").read_text() == "soul v2"
    assert (workspace_dir / "AGENTS.md").read_text() == "agents v2"
    assert (workspace_dir / "HEARTBEAT.md").read_text() == "heartbeat v1"


def test_refresh_standard_to_runtime_uses_legacy_prompt_fallbacks(tmp_path):
    """Re-bridge can still seed prompts from legacy MinIO readers."""
    standard_dir = tmp_path / "standard"
    runtime_dir = tmp_path / "standard" / ".copaw"
    standard_dir.mkdir()

    refresh_standard_to_runtime(
        standard_dir,
        runtime_dir,
        _make_openclaw_cfg(),
        get_soul=lambda: "fallback soul",
        get_agents_md=lambda: "fallback agents",
    )

    workspace_dir = runtime_dir / "workspaces" / "default"
    assert (workspace_dir / "SOUL.md").read_text() == "fallback soul"
    assert (workspace_dir / "AGENTS.md").read_text() == "fallback agents"
    assert (workspace_dir / "agent.json").exists()


def test_sync_mcporter_config_to_runtime_prefers_config_path(tmp_path):
    """config/mcporter.json wins over legacy mcporter-servers.json."""
    standard_dir = tmp_path / "standard"
    runtime_dir = tmp_path / "standard" / ".copaw"
    (standard_dir / "config").mkdir(parents=True)
    (standard_dir / "config" / "mcporter.json").write_text("new config")
    (standard_dir / "mcporter-servers.json").write_text("legacy config")

    copied = sync_mcporter_config_to_runtime(standard_dir, runtime_dir)

    assert copied == runtime_dir / "workspaces" / "default" / "config" / "mcporter.json"
    assert copied.read_text() == "new config"


def test_sync_skills_to_runtime_exposes_standard_skills_via_symlink(tmp_path):
    """Runtime workspace skills are a projection of standard-space skills."""
    standard_dir = tmp_path / "standard"
    runtime_dir = standard_dir / ".copaw"
    src_skill = standard_dir / "skills" / "github"
    script = src_skill / "scripts" / "run.sh"
    script.parent.mkdir(parents=True)
    (src_skill / "SKILL.md").write_text("Use GitHub.")
    script.write_text("#!/bin/sh\necho ok\n")
    script.chmod(stat.S_IRUSR | stat.S_IWUSR)

    installed = sync_skills_to_runtime(standard_dir, runtime_dir, ["github"])

    workspace_skills = runtime_dir / "workspaces" / "default" / "skills"
    assert installed == ["github"]
    assert workspace_skills.is_symlink()
    assert workspace_skills.resolve() == (standard_dir / "skills").resolve()
    assert (workspace_skills / "github" / "SKILL.md").read_text() == "Use GitHub."
    dst_script = workspace_skills / "github" / "scripts" / "run.sh"
    assert dst_script.read_text() == "#!/bin/sh\necho ok\n"
    assert dst_script.stat().st_mode & stat.S_IXUSR
    manifest = json.loads(
        (runtime_dir / "workspaces" / "default" / "skill.json").read_text(),
    )
    assert manifest["skills"]["github"]["enabled"] is True
    assert manifest["skills"]["github"]["channels"] == ["all"]


def test_sync_skills_to_runtime_reenables_projected_manifest_entries(tmp_path):
    """AgentTeams-projected skills are enabled even after CoPaw reconciled them off."""
    standard_dir = tmp_path / "standard"
    runtime_dir = standard_dir / ".copaw"
    src_skill = standard_dir / "skills" / "github"
    src_skill.mkdir(parents=True)
    (src_skill / "SKILL.md").write_text("Use GitHub.")

    manifest_path = runtime_dir / "workspaces" / "default" / "skill.json"
    manifest_path.parent.mkdir(parents=True)
    manifest_path.write_text(
        json.dumps(
            {
                "schema_version": "workspace-skill-manifest.v1",
                "version": 1,
                "skills": {
                    "github": {
                        "enabled": False,
                        "channels": ["matrix"],
                        "source": "customized",
                    },
                },
            },
        ),
    )

    installed = sync_skills_to_runtime(standard_dir, runtime_dir, ["github"])

    assert installed == ["github"]
    manifest = json.loads(manifest_path.read_text())
    assert manifest["skills"]["github"]["enabled"] is True
    assert manifest["skills"]["github"]["channels"] == ["matrix"]


def test_sync_skills_to_runtime_replaces_runtime_dir_and_cleans_stale_standard_skills(tmp_path):
    """Runtime skills dir is derived; stale local copies are removed."""
    standard_dir = tmp_path / "standard"
    runtime_dir = standard_dir / ".copaw"
    workspace_skills = runtime_dir / "workspaces" / "default" / "skills"
    stale_runtime_skill = workspace_skills / "stale"
    stale_runtime_skill.mkdir(parents=True)
    (stale_runtime_skill / "SKILL.md").write_text("remove me")

    stale_standard_skill = standard_dir / "skills" / "stale-standard"
    stale_standard_skill.mkdir(parents=True)
    (stale_standard_skill / "SKILL.md").write_text("remove me too")

    fresh_skill = standard_dir / "skills" / "fresh"
    fresh_skill.mkdir(parents=True)
    (fresh_skill / "SKILL.md").write_text("new")

    installed = sync_skills_to_runtime(standard_dir, runtime_dir, ["fresh"])

    assert installed == ["fresh"]
    assert workspace_skills.is_symlink()
    assert workspace_skills.resolve() == (standard_dir / "skills").resolve()
    assert (workspace_skills / "fresh" / "SKILL.md").read_text() == "new"
    assert not (workspace_skills / "stale").exists()
    assert not (workspace_skills / "stale-standard").exists()


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


def test_bridge_standard_to_runtime_materializes_prompts_mcporter_and_skills(tmp_path):
    """High-level bridge writes CoPaw config plus standard-space file copies."""
    standard_dir = tmp_path / "standard"
    runtime_dir = standard_dir / ".copaw"
    skill_dir = standard_dir / "skills" / "task-management"
    (standard_dir / "config").mkdir(parents=True)
    skill_dir.mkdir(parents=True)
    (standard_dir / "SOUL.md").write_text("soul")
    (standard_dir / "AGENTS.md").write_text("agents")
    (standard_dir / "config" / "mcporter.json").write_text('{"mcpServers": {}}')
    (skill_dir / "SKILL.md").write_text("Use task tools.")

    bridge_standard_to_runtime(
        standard_dir,
        runtime_dir,
        _make_openclaw_cfg(),
        skill_names=["task-management"],
    )

    workspace_dir = runtime_dir / "workspaces" / "default"
    assert (workspace_dir / "SOUL.md").read_text() == "soul"
    assert (workspace_dir / "AGENTS.md").read_text() == "agents"
    assert (workspace_dir / "config" / "mcporter.json").read_text() == '{"mcpServers": {}}'
    assert (workspace_dir / "skills" / "task-management" / "SKILL.md").read_text() == "Use task tools."
    assert (workspace_dir / "agent.json").exists()
    assert (runtime_dir / "providers.json").exists()
