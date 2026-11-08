"""Documentation must describe what the adapter scripts actually do.

The maintainer's P2 on #1459: after the staging/`__init__.py` fixes the
READMEs and the shell headers still described the old in-place install (the
adapter directory registered directly, `../../skills` paths, "no staged
copies"). These tests pin the docs to the code so the next behaviour change
cannot leave them stale:

- every file the installer stages is named in the adapter README;
- a staged-root installer documents its staged root (and a custom `--config`);
- stale phrases from the previous lifecycle cannot reappear;
- the `mcp/` package initializer and the Hermes directory entry are both
  documented and present.
"""

from __future__ import annotations

import re
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[3]
ADAPTERS = REPO_ROOT / "plugins" / "agentteams-teamharness" / "adapters"


def _read(path: Path) -> str:
    assert path.is_file(), f"missing file: {path}"
    return path.read_text(encoding="utf-8")


def test_openclaw_docs_describe_the_staged_root():
    readme = _read(ADAPTERS / "openclaw" / "README.md")
    install = _read(ADAPTERS / "openclaw" / "install.sh")

    # The code stages a self-contained root; the README must say where it is
    # and that it is what gets registered.
    assert "stage_plugin" in install, "install.sh no longer stages a plugin root"
    assert "plugins/agentteams-teamharness" in readme, (
        "README must name the staged plugin root"
    )
    assert "--config" in readme and "plugins/agentteams-teamharness" in readme, (
        "README must explain the staged root for a custom --config"
    )
    # Every staged artifact is documented.
    for name in ("index.mjs", "openclaw.plugin.json", "skills/"):
        assert name in readme, f"README does not mention the staged {name}"
    # Stale lifecycle statements must not come back.
    for stale in ("../../skills", "no staged copies", "has no `__init__.py`"):
        assert stale not in readme, f"stale README statement: {stale!r}"
    assert "never copies" not in install, "install.sh header still says it never copies"
    assert "is the OpenClaw plugin root" not in readme, (
        "README still claims the adapter directory is the plugin root"
    )


def test_hermes_docs_list_every_staged_file():
    readme = _read(ADAPTERS / "hermes" / "README.md")
    install = _read(ADAPTERS / "hermes" / "install.sh")

    match = re.search(r"PLUGIN_FILES\s*=\s*\(([^)]*)\)", install, re.S)
    assert match, "hermes install.sh has no PLUGIN_FILES tuple"
    staged = re.findall(r'"([^"]+)"', match.group(1))
    assert "__init__.py" in staged, "the directory plugin must stage __init__.py"
    for name in staged:
        assert name in readme, f"hermes README does not list the staged {name}"
    # The entry point the loader requires exists in the source tree too.
    assert (ADAPTERS / "hermes" / "__init__.py").is_file()
    register = _read(ADAPTERS / "hermes" / "register.py")
    assert "str(skill_md)" not in register, (
        "register_skill expects a Path (path.exists()); passing str broke the SDK call"
    )


def test_mcp_package_initializer_is_documented():
    init = REPO_ROOT / "plugins" / "agentteams-teamharness" / "mcp" / "__init__.py"
    assert init.is_file(), "mcp/ must be a regular package (import mcp.server)"
    readme = _read(ADAPTERS / "openclaw" / "README.md")
    assert "regular package" in readme or "__init__.py" in readme, (
        "README must describe the mcp/ package initializer"
    )
