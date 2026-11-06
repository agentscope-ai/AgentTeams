"""L3/L4 lifecycle conformance for the teamharness bundle adapters (P-c).

``L3`` — install, load and uninstall the OpenClaw and Hermes adapters on a
throwaway HOME. The staged plugin root must carry a loadable manifest (and,
for OpenClaw, a syntactically valid entry module), the runtime config must
carry the projected MCP server, and uninstall must remove exactly what the
installer wrote while leaving foreign content alone.

``L4`` — N-1 install -> N upgrade -> N-1 rollback for both adapters. The
OpenClaw adapter rebuilds its staged plugin root, so an upgrade drops the
files the old release carried. The Hermes adapter syncs without an ownership
marker, so an upgrade keeps files the new payload no longer ships; that
limitation is asserted explicitly here instead of being hidden, and uninstall
still cleans the tree.

Refs #1458, #1459 (OpenClaw/Hermes adapters). Run with ``make verify-bundle``.
"""

from __future__ import annotations

import os
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from _bundle_lifecycle import (  # noqa: E402
    REPO_ROOT,
    compile_python_tree,
    isolated_env,
    node_check,
    read_json,
    read_yaml,
    run,
    stage_bundle,
)

BUNDLE = REPO_ROOT / "plugins" / "agentteams-teamharness"
INSTALL = BUNDLE / "scripts" / "install.sh"
UNINSTALL = BUNDLE / "scripts" / "uninstall.sh"

LEGACY_SKILL = "skills/agent/legacy_skill.md"
NEW_SKILL = "skills/agent/new_skill.md"


def _env(home: Path, config: Path, hermes_home: Path) -> dict:
    return isolated_env(
        home,
        OPENCLAW_CONFIG_PATH=str(config),
        HERMES_HOME=str(hermes_home),
    )


def _install(bundle: Path, runtime: str, env: dict) -> None:
    run(bundle / "scripts" / "install.sh", "--runtime", runtime, env=env)


# ---------------------------------------------------------------------------
# L3 — OpenClaw
# ---------------------------------------------------------------------------
def test_l3_openclaw_install_load_uninstall(tmp_path: Path) -> None:
    home = tmp_path / "home"
    home.mkdir()
    config = home / "openclaw.json"
    config.write_text(
        '{"theme": "dark", "note": "operator"}\n', encoding="utf-8"
    )
    env = _env(home, config, home / "hermes")

    _install(BUNDLE, "openclaw", env)

    document = read_json(config)
    server = document["mcp"]["servers"]["teamharness"]
    assert server["command"] == "python3"
    assert server["cwd"] == str(BUNDLE)
    assert server["env"], "the MCP entry must carry its documented env allowlist"

    staged = home / "plugins" / "agentteams-teamharness"
    manifest = read_json(staged / "openclaw.plugin.json")
    assert manifest["id"] == "agentteams-teamharness"
    assert (staged / "index.mjs").is_file(), "the entry module is not staged"
    assert set(manifest["skills"]), "the manifest declares no skills"
    for skill in manifest["skills"]:
        assert (staged / skill).is_dir(), f"declared skill {skill!r} is missing"

    assert document["plugins"]["allow"] == ["agentteams-teamharness"]
    assert document["plugins"]["entries"]["agentteams-teamharness"]["enabled"] is True
    assert document["plugins"]["load"]["paths"] == [str(staged)]

    # Load: the manifest parses (above), the entry module must be valid JS,
    # and the operator's own config keys survive the projection.
    node_check(staged / "index.mjs")
    assert document["theme"] == "dark"
    assert document["note"] == "operator"

    run(UNINSTALL, "--runtime", "openclaw", env=env)

    document = read_json(config)
    assert "mcp" not in document
    assert document["plugins"] == {
        "allow": [],
        "entries": {},
        "load": {"paths": []},
    }
    assert document["theme"] == "dark"
    assert document["note"] == "operator"
    assert not staged.exists(), "uninstall left the staged plugin root behind"


# ---------------------------------------------------------------------------
# L3 — Hermes
# ---------------------------------------------------------------------------
def test_l3_hermes_install_load_uninstall(tmp_path: Path) -> None:
    home = tmp_path / "home"
    home.mkdir()
    hermes_home = home / "hermes"
    hermes_home.mkdir()
    (hermes_home / "operator-note.txt").write_text("keep\n", encoding="utf-8")
    env = _env(home, home / "openclaw.json", hermes_home)

    _install(BUNDLE, "hermes", env)

    config = read_yaml(hermes_home / "config.yaml")
    server = config["mcp_servers"]["teamharness"]
    assert server["command"] == "python3"
    assert server["args"][-1] == str(BUNDLE / "mcp" / "server.py")
    assert isinstance(server["env"], dict)

    plugin_dir = hermes_home / "plugins" / "agentteams-teamharness"
    manifest = read_yaml(plugin_dir / "plugin.yaml")
    assert manifest["name"] == "agentteams-teamharness"
    assert (plugin_dir / "__init__.py").is_file(), (
        "the hermes directory loader disables a plugin without __init__.py"
    )
    assert (plugin_dir / "register.py").is_file()
    assert (plugin_dir / "skills").is_dir()

    # Load: the payload the hermes plugin loader imports must compile.
    compile_python_tree(plugin_dir)

    run(UNINSTALL, "--runtime", "hermes", env=env)

    config = read_yaml(hermes_home / "config.yaml")
    assert "mcp_servers" not in config
    assert not plugin_dir.exists(), "uninstall left the plugin directory behind"
    assert (hermes_home / "operator-note.txt").is_file(), (
        "uninstall removed a file it never wrote"
    )


# ---------------------------------------------------------------------------
# L4 — OpenClaw: the staged root follows the payload
# ---------------------------------------------------------------------------
def test_l4_openclaw_upgrade_replaces_the_staged_root(tmp_path: Path) -> None:
    home = tmp_path / "home"
    home.mkdir()
    config = home / "openclaw.json"
    config.write_text('{"theme": "dark"}\n', encoding="utf-8")
    env = _env(home, config, home / "hermes")
    staged = home / "plugins" / "agentteams-teamharness"

    previous = stage_bundle(
        BUNDLE,
        tmp_path / "bundle-n-1",
        version="0.0.9",
        add={LEGACY_SKILL: "legacy skill\n"},
    )
    current = stage_bundle(
        BUNDLE,
        tmp_path / "bundle-n",
        version="0.1.0",
        add={NEW_SKILL: "new skill\n"},
    )

    _install(previous, "openclaw", env)
    assert (staged / LEGACY_SKILL).is_file()
    assert read_json(config)["mcp"]["servers"]["teamharness"]["cwd"] == str(previous)

    _install(current, "openclaw", env)  # upgrade
    assert (staged / NEW_SKILL).is_file()
    assert not (staged / LEGACY_SKILL).exists(), (
        "upgrade kept a file only the N-1 payload carried"
    )
    assert read_json(config)["mcp"]["servers"]["teamharness"]["cwd"] == str(current)

    _install(previous, "openclaw", env)  # rollback
    assert (staged / LEGACY_SKILL).is_file()
    assert not (staged / NEW_SKILL).exists(), (
        "rollback kept a file only the N payload carried"
    )
    assert read_json(config)["mcp"]["servers"]["teamharness"]["cwd"] == str(previous)

    run(previous / "scripts" / "uninstall.sh", "--runtime", "openclaw", env=env)
    assert "mcp" not in read_json(config)
    assert not staged.exists()


# ---------------------------------------------------------------------------
# L4 — Hermes: sync without an ownership marker
# ---------------------------------------------------------------------------
def test_l4_hermes_upgrade_and_rollback(tmp_path: Path) -> None:
    home = tmp_path / "home"
    home.mkdir()
    hermes_home = home / "hermes"
    hermes_home.mkdir()
    env = _env(home, home / "openclaw.json", hermes_home)
    plugin_dir = hermes_home / "plugins" / "agentteams-teamharness"

    previous = stage_bundle(
        BUNDLE,
        tmp_path / "bundle-n-1",
        version="0.0.9",
        add={LEGACY_SKILL: "legacy skill\n"},
    )
    current = stage_bundle(
        BUNDLE,
        tmp_path / "bundle-n",
        version="0.1.0",
        add={NEW_SKILL: "new skill\n"},
    )

    _install(previous, "hermes", env)
    assert (plugin_dir / LEGACY_SKILL).is_file()
    assert read_yaml(hermes_home / "config.yaml")["mcp_servers"]["teamharness"][
        "args"
    ][-1] == str(previous / "mcp" / "server.py")

    _install(current, "hermes", env)  # upgrade
    assert (plugin_dir / NEW_SKILL).is_file()
    # Documented limitation: this adapter syncs without an ownership marker,
    # so it cannot prune a file the previous payload owned and the new one
    # does not (the matrix bundle's hermes adapter solves this with
    # installed.json). Asserted so the gap is visible, not silently blessed.
    assert (plugin_dir / LEGACY_SKILL).is_file(), (
        "the hermes adapter now prunes stale files; drop this assertion and "
        "the limitation note together"
    )
    assert read_yaml(hermes_home / "config.yaml")["mcp_servers"]["teamharness"][
        "args"
    ][-1] == str(current / "mcp" / "server.py")

    _install(previous, "hermes", env)  # rollback
    assert (plugin_dir / LEGACY_SKILL).is_file()
    assert read_yaml(hermes_home / "config.yaml")["mcp_servers"]["teamharness"][
        "args"
    ][-1] == str(previous / "mcp" / "server.py")

    run(previous / "scripts" / "uninstall.sh", "--runtime", "hermes", env=env)
    assert "mcp_servers" not in read_yaml(hermes_home / "config.yaml")
    assert not plugin_dir.exists(), "uninstall left the plugin directory behind"
