"""TeamHarness Hermes adapter — plugin entry point.

hermes-agent (v2026.4.16) discovers third-party plugins by directory scan of
``~/.hermes/plugins/<dir>/`` (``hermes_cli/plugins.py``) and calls
``register(ctx)`` on the module in the plugin directory. An exception in
``register`` disables the plugin; the runtime itself keeps running.

Division of labor, matching what the runtime actually exposes:

* Skills are registered here through ``ctx.register_skill(name, path)`` —
  the skill API ``PluginContext`` provides. The bundle's skill directories
  are copied into ``<plugin>/skills/`` by ``install.sh`` (the upstream
  convention ``plugins/<name>/skills/<skill>/SKILL.md``), so this module
  finds them relative to itself.
* The MCP server is **not** registered here: hermes-agent discovers MCP
  servers from ``mcp_servers`` in ``~/.hermes/config.yaml``
  (``tools/mcp_tool.py::_load_mcp_config``), and ``PluginContext`` has no
  MCP registration API. ``install.sh`` projects the contract's MCP server
  entry into that file.

The module is stdlib-only and duck-types ``ctx`` so it can be imported and
checked without a live hermes runtime.
"""

from __future__ import annotations

import os
from pathlib import Path

PLUGIN_ROOT = Path(__file__).resolve().parent
SKILLS_DIR = Path(
    os.environ.get("AGENTTEAMS_TEAMHARNESS_SKILLS") or (PLUGIN_ROOT / "skills")
)


def discover_skills():
    """Yield ``(skill_name, skill_md_path)`` for every skill directory under
    the skills root (any directory containing a ``SKILL.md``), sorted by
    path. ``install.sh`` copies the bundle layout
    ``<root>/<skill>/SKILL.md`` as-is, so discovery is depth-agnostic."""
    if not SKILLS_DIR.is_dir():
        return
    for skill_md in sorted(SKILLS_DIR.rglob("SKILL.md")):
        yield skill_md.parent.name, skill_md


def register(ctx):
    """Register the bundle's skills with the hermes runtime.

    Returns the number of skills registered. A ``ctx`` without a callable
    ``register_skill`` (i.e. not a hermes PluginContext) yields 0 instead of
    raising, so importing this module outside hermes is always safe.
    """
    register_skill = getattr(ctx, "register_skill", None)
    if not callable(register_skill):
        return 0
    count = 0
    for name, skill_md in discover_skills():
        # PluginContext.register_skill expects a Path: it calls
        # path.exists() and stores the path for skill_view().
        register_skill(name, skill_md)
        count += 1
    return count
