"""Tests for bridge.py — template-create + controller-field overlay model.

Current contract (2.2 line; the pre-2.2 "rich overlay" contract — union
allow_from, deep-merge groups, embedding_config, openclaw heartbeat seed,
per-agent ``agent=`` key — was removed during the QwenPaw 2.2 migration.
Tests below pin the behavior the Manager actually runs in production):

1. **create phase** — a missing ``workspaces/default/agent.json`` is
   installed from an in-tree template (``agent.{profile}.json``); a missing
   config.json is created with the Matrix channel block. The bridge never
   manages the ``security`` block (QwenPaw applies runtime defaults).

2. **restart-overlay phase** — the overlay refreshes only the fields the
   Controller owns: Matrix scalars (token, user), ``running.max_input_length``,
   ``subagent_model``; stream filters are pinned True/True. Console is
   forced off.  ``channels.matrix.allow_from`` / ``group_allow_from`` /
   ``groups`` are controller-wins: the openclaw.json values fully replace
   the previously projected values (an empty source value clears the
   field), so revocations and policy tightening take effect on re-bridge.
   ``env`` and other user-owned fields are never bridged.
"""

import json
import os
import stat
import tempfile
from pathlib import Path

import pytest

from agentteams_manager.bridge import (
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

def test_create_writes_config_json_with_matrix_channel():
    """On first boot the bridge writes config.json with the Matrix channel
    block and disables the console channel. The bridge does not manage the
    ``security`` block — QwenPaw applies its own runtime defaults."""
    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"
        _run_bridge(_make_openclaw_cfg(), working_dir)

        cfg_path = working_dir / "config.json"
        assert cfg_path.exists()
        cfg = json.loads(cfg_path.read_text())
        matrix = cfg["channels"]["matrix"]
        assert matrix["enabled"] is True
        assert matrix["access_token"] == "tok"
        assert matrix["filter_tool_messages"] is True
        assert cfg["channels"]["console"]["enabled"] is False
        assert "security" not in cfg


def test_create_installs_worker_agent_json_from_template():
    """Worker profile seeds agent.json from agent.worker.json.

    (Explicit profile: the bridge default profile is ``manager``.)
    """
    agent = _bridge_and_read_agent(_make_openclaw_cfg(), profile="worker")

    assert agent["id"] == "default"
    assert agent["name"] == "Default Agent"
    assert agent["language"] == "zh"
    assert agent["system_prompt_files"] == ["AGENTS.md", "SOUL.md", "PROFILE.md"]
    # Console forced off by the overlay (Matrix is the channel).
    assert agent["channels"]["console"]["enabled"] is False
    # Stream filters pinned True by the overlay.
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
    # Stream filters pinned True by the overlay (template says false, but
    # the overlay wins — the Manager must not leak raw tool calls to Matrix).
    assert agent["channels"]["matrix"]["filter_tool_messages"] is True
    assert agent["channels"]["matrix"]["filter_thinking"] is True
    assert "require_approval" not in agent.get("running", {})
    assert agent["channels"]["matrix"]["user_id"] == "@manager:matrix.example.org"


def test_agent_json_always_targets_default_workspace():
    """The controller bridge always writes workspaces/default/agent.json —
    per-agent-key support was removed in the 2.2 line."""
    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"
        _run_bridge(_make_openclaw_cfg(), working_dir)
        assert (working_dir / "workspaces" / "default" / "agent.json").exists()


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
        cfg["user_custom"] = {"hello": "world"}
        cfg_path.write_text(json.dumps(cfg))

        _run_bridge(_make_openclaw_cfg(), working_dir)

        cfg2 = json.loads(cfg_path.read_text())
        assert cfg2["user_custom"] == {"hello": "world"}
        assert cfg2["channels"]["matrix"]["access_token"] == "tok"


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


def test_stream_filters_enforced_on_rebridge():
    """The overlay pins both stream filters True; user edits do not survive
    a re-bridge (runtime policy, not user-owned)."""
    cfg = _make_openclaw_cfg()

    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"
        _run_bridge(cfg, working_dir)

        agent_path = _agent_json_path(working_dir)
        agent = json.loads(agent_path.read_text())
        agent["channels"]["matrix"]["filter_tool_messages"] = False
        agent["channels"]["matrix"]["filter_thinking"] = False
        agent_path.write_text(json.dumps(agent))

        _run_bridge(cfg, working_dir)

        matrix = _read_agent(working_dir)["channels"]["matrix"]
        assert matrix["filter_tool_messages"] is True
        assert matrix["filter_thinking"] is True


def test_openclaw_stream_filter_keys_ignored():
    """openclaw.json stream-filter keys are not consumed by the current
    contract — the overlay pins both filters True regardless."""
    cfg = _make_openclaw_cfg()
    cfg["channels"]["matrix"]["filterToolMessages"] = False
    cfg["channels"]["matrix"]["filterThinking"] = False

    agent = _bridge_and_read_agent(cfg)

    matrix = agent["channels"]["matrix"]
    assert matrix["filter_tool_messages"] is True
    assert matrix["filter_thinking"] is True


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


def test_embedding_config_never_written_by_bridge():
    """The 2.2-line bridge does not write running.embedding_config —
    memory-search embedding config is handled outside the controller
    bridge (the pre-2.2 memorySearch→embedding_config path was removed)."""
    agent = _bridge_and_read_agent(_make_openclaw_cfg())
    assert "embedding_config" not in agent.get("running", {})


# ---------------------------------------------------------------------------
# Controller-field overlay: controller-wins allowlists
# ---------------------------------------------------------------------------

def test_allow_from_is_controller_wins():
    """channels.matrix.allow_from: the openclaw.json value fully replaces
    the previously projected value on re-bridge.

    A value left in agent.json by an earlier bridge is the bridge's own
    projection, not an operator-owned override — merging it back would
    make a revoked user stay allowed forever.
    """
    cfg = _make_openclaw_cfg()
    cfg["channels"]["matrix"]["dm"] = {
        "policy": "allowlist",
        "allowFrom": ["@alice:example.org"],
    }

    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"
        _run_bridge(cfg, working_dir)

        # Stale local addition left in agent.json must not survive.
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
    assert allow_from == ["@alice:example.org", "@carol:example.org"]


# ---------------------------------------------------------------------------
# Controller-field overlay: controller-wins groups
# ---------------------------------------------------------------------------

def test_groups_is_controller_wins():
    """channels.matrix.groups: the openclaw.json value fully replaces the
    previously projected value (per-room leaves included)."""
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

        # Stale local edits left in agent.json must not survive.
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
    assert groups == {
        "*": {"requireMention": True, "historyLimit": 200, "newFlag": True},
    }


def test_allowlist_removal_takes_effect_on_rebridge():
    """Regression (upstream review of the Manager extraction): a user
    removed from every source allowlist must not survive in agent.json.

    Repro: invoke the bridge twice — first allowing a user, then
    removing that user from the source allowlists.  The merge semantics
    removed by this fix kept the previously projected value, so the
    user stayed allowed; controller-wins makes the revocation effective.
    """
    cfg = _make_openclaw_cfg()
    cfg["channels"]["matrix"]["dm"] = {
        "policy": "allowlist",
        "allowFrom": ["@alice:example.org", "@bob:example.org"],
    }
    cfg["channels"]["matrix"]["groupAllowFrom"] = ["@bob:example.org"]

    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"
        _run_bridge(cfg, working_dir)
        agent = _read_agent(working_dir)
        assert "@bob:example.org" in agent["channels"]["matrix"]["allow_from"]
        assert agent["channels"]["matrix"]["group_allow_from"] == ["@bob:example.org"]

        # Revoke @bob from every source allowlist.
        cfg["channels"]["matrix"]["dm"]["allowFrom"] = ["@alice:example.org"]
        cfg["channels"]["matrix"]["groupAllowFrom"] = []
        _run_bridge(cfg, working_dir)
        agent = _read_agent(working_dir)

    matrix = agent["channels"]["matrix"]
    assert matrix["allow_from"] == ["@alice:example.org"]
    assert matrix["group_allow_from"] == []


def test_group_policy_tightening_takes_effect_on_rebridge():
    """Regression (upstream review of the Manager extraction): tightening
    a group policy — requireMention false -> true, historyLimit lowered —
    must take effect on re-bridge; the previously projected value must
    not win over the controller."""
    cfg = _make_openclaw_cfg()
    cfg["channels"]["matrix"]["groups"] = {
        "*": {"requireMention": False, "historyLimit": 200},
    }

    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"
        _run_bridge(cfg, working_dir)
        assert (
            _read_agent(working_dir)["channels"]["matrix"]["groups"]["*"]["requireMention"]
            is False
        )

        # Tighten: mention now required, history window shrunk.
        cfg["channels"]["matrix"]["groups"]["*"]["requireMention"] = True
        cfg["channels"]["matrix"]["groups"]["*"]["historyLimit"] = 50
        _run_bridge(cfg, working_dir)
        agent = _read_agent(working_dir)

    groups = agent["channels"]["matrix"]["groups"]
    assert groups["*"]["requireMention"] is True
    assert groups["*"]["historyLimit"] == 50


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

def test_manager_template_heartbeat_seed_ignores_openclaw(monkeypatch):
    """Template heartbeat (manager: 30m) is installed at create time;
    openclaw.json agents.defaults.heartbeat is not consumed by the
    controller bridge."""
    monkeypatch.setenv("AGENTTEAMS_MATRIX_DOMAIN", "matrix.example.org")

    cfg = _make_openclaw_cfg()
    cfg["agents"]["defaults"]["heartbeat"] = {
        "every": "5m",
        "target": "self",
        "activeHours": "09:00-18:00",
    }

    agent = _bridge_and_read_agent(cfg, profile="manager")
    assert agent["heartbeat"] == {"enabled": True, "every": "30m"}


def test_worker_template_seeds_default_heartbeat_when_openclaw_silent():
    agent = _bridge_and_read_agent(_make_openclaw_cfg(), profile="worker")
    assert agent["heartbeat"] == {"enabled": True, "every": "10m"}


def test_bridge_does_not_seed_heartbeat_into_existing_agent():
    """Heartbeat is a template-install-only seed: an existing agent.json
    without a heartbeat block is left as-is (the bridge never adds one,
    and openclaw.json heartbeat is not consumed)."""
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

    assert "heartbeat" not in agent


# ---------------------------------------------------------------------------
# Validation
# ---------------------------------------------------------------------------

def test_unknown_profile_falls_back_to_minimal_agent_json():
    """No template exists for unknown profiles — the bridge installs a
    minimal agent.json (boot never fails) instead of raising."""
    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"
        bridge_controller_to_copaw(_make_openclaw_cfg(), working_dir, profile="leader")

        agent = _read_agent(working_dir)
    assert agent["id"] == "default"
    assert agent["channels"]["matrix"]["enabled"] is True


# ---------------------------------------------------------------------------
# Standard/runtime file materialization
# ---------------------------------------------------------------------------

def test_sync_outer_prompt_files_to_inner_copies_prompts_and_seeds_heartbeat(tmp_path):
    """SOUL/AGENTS refresh every run; HEARTBEAT is copied only on first boot."""
    standard_dir = tmp_path / "standard"
    runtime_dir = tmp_path / "standard" / ".qwenpaw"
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
    runtime_dir = tmp_path / "standard" / ".qwenpaw"
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
    runtime_dir = tmp_path / "standard" / ".qwenpaw"
    (standard_dir / "config").mkdir(parents=True)
    (standard_dir / "config" / "mcporter.json").write_text("new config")
    (standard_dir / "mcporter-servers.json").write_text("legacy config")

    copied = sync_mcporter_config_to_runtime(standard_dir, runtime_dir)

    assert copied == runtime_dir / "workspaces" / "default" / "config" / "mcporter.json"
    assert copied.read_text() == "new config"


def test_sync_skills_to_runtime_exposes_standard_skills_via_symlink(tmp_path):
    """Runtime workspace skills are a projection of standard-space skills."""
    standard_dir = tmp_path / "standard"
    runtime_dir = standard_dir / ".qwenpaw"
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
    runtime_dir = standard_dir / ".qwenpaw"
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
    runtime_dir = standard_dir / ".qwenpaw"
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
    workspace_dir = standard_dir / ".qwenpaw" / "workspaces" / "default"
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
    workspace_dir = standard_dir / ".qwenpaw" / "workspaces" / "default"
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
    runtime_dir = standard_dir / ".qwenpaw"
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


# ---------------------------------------------------------------------------
# Controller → bridge regression: model input capability propagation
# ---------------------------------------------------------------------------

def _make_openclaw_cfg_with_input(models_input_map: dict[str, list[str]]):
    """Build an openclaw config where each model carries an ``input`` list.

    ``models_input_map`` maps model_id → input modalities, e.g.
    ``{"vision-model": ["text", "image"], "text-model": ["text"]}``.
    """
    model_list = [
        {"id": mid, "input": modalities}
        for mid, modalities in models_input_map.items()
    ]
    return {
        "channels": {"matrix": {"enabled": True}},
        "models": {
            "providers": {
                "gw": {
                    "baseUrl": "http://aigw:8080/v1",
                    "apiKey": "key123",
                    "models": model_list,
                }
            }
        },
        "agents": {"defaults": {"model": {"primary": f"gw/{model_list[0]['id']}"}}},
    }


def _run_bridge_and_read_providers(cfg):
    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"
        _run_bridge(cfg, working_dir)
        with open(working_dir / "providers.json") as f:
            return json.load(f)


def test_providers_json_propagates_input_capability():
    """Controller model ``input`` must reach providers.json as capability flags."""
    cfg = _make_openclaw_cfg_with_input({
        "vision-model": ["text", "image"],
        "video-model": ["text", "video"],
        "text-model": ["text"],
    })
    providers = _run_bridge_and_read_providers(cfg)
    models = providers["custom_providers"]["gw"]["models"]
    by_id = {m["id"]: m for m in models}

    vision = by_id["vision-model"]
    assert vision["supports_image"] is True
    assert vision["supports_video"] is False
    assert vision["supports_multimodal"] is True

    video = by_id["video-model"]
    assert video["supports_image"] is False
    assert video["supports_video"] is True
    assert video["supports_multimodal"] is True

    text = by_id["text-model"]
    assert text["supports_image"] is False
    assert text["supports_video"] is False
    assert text["supports_multimodal"] is False


def test_providers_json_no_input_field_no_capability_flags():
    """Models without ``input`` keep plain {id, name} — backward compat."""
    cfg = _make_openclaw_cfg_with_input({"plain-model": []})
    # Remove the empty input list to simulate old openclaw.json
    cfg["models"]["providers"]["gw"]["models"][0].pop("input", None)
    providers = _run_bridge_and_read_providers(cfg)
    model = providers["custom_providers"]["gw"]["models"][0]
    assert model == {"id": "plain-model", "name": "plain-model"}

# ---------------------------------------------------------------------------
# subagent_model overlay (QwenPaw >= 2.1.1 native field)
# ---------------------------------------------------------------------------


def test_agent_json_overlays_subagent_model_from_openclaw():
    """agents.defaults.model.subagent is mirrored into agent.json verbatim."""
    cfg = _make_openclaw_cfg()
    cfg["agents"]["defaults"]["model"]["subagent"] = {
        "provider_id": "gw",
        "model": "qwen3.5-flash",
    }
    agent = _bridge_and_read_agent(cfg)
    assert agent["subagent_model"] == {
        "provider_id": "gw",
        "model": "qwen3.5-flash",
    }


def test_agent_json_clears_subagent_model_when_unset():
    """Controller-owned field: a stale value is removed when openclaw has none."""
    with tempfile.TemporaryDirectory() as tmpdir:
        working_dir = Path(tmpdir) / "agent"
        ws = working_dir / "workspaces" / "default"
        ws.mkdir(parents=True)
        stale = {
            "id": "default",
            "channels": {"matrix": {"enabled": True}},
            "subagent_model": {"provider_id": "gw", "model": "stale-model"},
        }
        with open(ws / "agent.json", "w") as f:
            json.dump(stale, f)
        _run_bridge(_make_openclaw_cfg(), working_dir)
        assert "subagent_model" not in _read_agent(working_dir)
