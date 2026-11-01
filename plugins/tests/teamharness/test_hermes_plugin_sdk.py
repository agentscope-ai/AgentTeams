"""Real-SDK regression for the TeamHarness Hermes adapter.

The maintainer's P1 on #1459: the installer staged only ``plugin.yaml``,
``register.py`` and ``skills/``, so the official directory loader disabled
the plugin with ``No __init__.py``; and ``register()`` passed ``str`` to
``PluginContext.register_skill``, which expects a ``Path``
(``AttributeError: 'str' object has no attribute 'exists'``).

These tests execute the real ``hermes_cli.plugins`` loader and context from a
pinned Hermes checkout (``HERMES_416_ROOT`` or ``/tmp/hermes-416``, otherwise
skipped), driving the real installer into an isolated HOME. This is an SDK
component check: it does not claim full gateway or MCP acceptance.
"""

from __future__ import annotations

import os
import shutil
import subprocess
import sys
from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).resolve().parents[3]
BUNDLE = REPO_ROOT / "plugins" / "agentteams-teamharness"
ADAPTER = BUNDLE / "adapters" / "hermes"
INSTALL = ADAPTER / "install.sh"
PLUGIN_ID = "agentteams-teamharness"
EXPECTED_SKILLS = {
    "communication",
    "file-sharing",
    "find-skills",
    "mcporter",
    "organization",
    "project-management",
    "roomflow",
    "team-coordination",
    "task-delegation",
    "task-execution",
}

_hermes_root = os.environ.get("HERMES_416_ROOT") or "/tmp/hermes-416"
HERMES_ROOT = Path(_hermes_root) if Path(_hermes_root).is_dir() else None

pytestmark = pytest.mark.skipif(
    HERMES_ROOT is None,
    reason=(
        "no pinned Hermes checkout for the real-SDK regression "
        "(set HERMES_416_ROOT; shallow-clone "
        "https://github.com/NousResearch/hermes-agent at v2026.4.16)"
    ),
)


def _install(tmp_path: Path) -> tuple[Path, Path]:
    """Run the real installer into an isolated HOME and return (home, plugin dir)."""
    home = tmp_path / "home"
    (home / ".hermes").mkdir(parents=True)
    result = subprocess.run(
        ["bash", str(INSTALL), "--config", str(home / ".hermes" / "config.yaml")],
        capture_output=True,
        text=True,
        env={**os.environ, "HOME": str(home)},
        timeout=120,
    )
    assert result.returncode == 0, result.stderr
    return home, home / ".hermes" / "plugins" / PLUGIN_ID


def _sdk():
    root = str(HERMES_ROOT)
    if root not in sys.path:
        sys.path.insert(0, root)
    import hermes_cli.plugins as plugins  # noqa: PLC0415 - optional dependency

    return plugins


def test_installer_stages_a_directory_plugin(tmp_path):
    """The loader requires ``__init__.py``; the staged plugin must carry it."""
    home, plugin_dir = _install(tmp_path)
    assert plugin_dir.is_dir()
    for name in ("__init__.py", "plugin.yaml", "register.py"):
        assert (plugin_dir / name).is_file(), f"{name} missing from the staged plugin"
    assert (plugin_dir / "__init__.py").read_text().strip(), "__init__.py must be non-empty"
    assert (plugin_dir / "skills").is_dir()
    # The loader in this pin does not read a manifest entry point: the skills
    # are enumerated from plugin.yaml's own listing plus this directory.
    assert len(list((plugin_dir / "skills").glob("*/*/SKILL.md"))) == len(EXPECTED_SKILLS)


def test_real_loader_loads_the_staged_plugin(tmp_path):
    """The official directory loader must import the plugin instead of disabling it."""
    plugins = _sdk()
    _, plugin_dir = _install(tmp_path)
    manager = plugins.PluginManager()
    manifest = plugins.PluginManifest(name=PLUGIN_ID, path=str(plugin_dir))
    module = manager._load_directory_module(manifest)
    assert module.__name__.startswith("hermes_plugins.")
    assert callable(getattr(module, "register", None)), "register() must be exposed"


def test_real_context_registers_path_skills(tmp_path):
    """``register(ctx)`` must pass Paths; every skill resolves through the SDK."""
    plugins = _sdk()
    _, plugin_dir = _install(tmp_path)
    manager = plugins.PluginManager()
    manifest = plugins.PluginManifest(name=PLUGIN_ID, path=str(plugin_dir))
    module = manager._load_directory_module(manifest)
    context = plugins.PluginContext(manifest, manager)
    module.register(context)

    registered = manager._plugin_skills
    assert registered, "no plugin skills registered"
    for qualified, entry in registered.items():
        assert qualified.startswith(f"{PLUGIN_ID}:"), qualified
        assert isinstance(entry["path"], Path), (
            f"{qualified} path must be a Path, got {type(entry['path']).__name__}"
        )
        assert entry["path"].is_file(), f"{qualified} path does not exist: {entry['path']}"
    names = {qualified.split(":", 1)[1] for qualified in registered}
    assert names == EXPECTED_SKILLS, (
        f"registered skills mismatch: missing={EXPECTED_SKILLS - names} "
        f"extra={names - EXPECTED_SKILLS}"
    )
