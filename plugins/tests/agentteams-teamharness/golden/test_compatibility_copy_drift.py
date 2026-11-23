"""Both TeamHarness packages use the shared Core directories."""
from pathlib import Path

PLUGINS_ROOT = Path(__file__).resolve().parents[3]


def test_bundle_uses_shared_teamharness_sources():
    for name in ("contracts", "mcp", "prompts", "skills"):
        path = PLUGINS_ROOT / "agentteams-teamharness" / name
        assert path.is_symlink()
        assert path.resolve(strict=True) == PLUGINS_ROOT / "teamharness" / name
