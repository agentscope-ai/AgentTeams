"""#1471 parity: the task meta conditional commit in the bundle copy.

Runs the source-tree regressions against `plugins/agentteams-teamharness`,
so a divergence between the two shipped bundles fails here instead of in
the field.
"""

from __future__ import annotations

import importlib.util
import sys
from pathlib import Path
from typing import Any

import pytest

REPO_ROOT = Path(__file__).resolve().parents[3]
BUNDLE_MCP_DIR = REPO_ROOT / "plugins" / "agentteams-teamharness" / "mcp"
LEGACY_MCP_DIR = REPO_ROOT / "plugins" / "teamharness" / "mcp"
LEGACY_TASK_META = REPO_ROOT / "plugins" / "tests" / "teamharness" / "test_task_meta_commit.py"


def _load_legacy_tests() -> Any:
    spec = importlib.util.spec_from_file_location(
        "legacy_task_meta_commit_tests", LEGACY_TASK_META
    )
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def _evict_mcp_modules() -> None:
    """Evict every cached ``mcp`` module — from BOTH trees.

    The mcp subpackages (artifact / common / lifecycle / project / room /
    transport) are namespace packages WITHOUT ``__file__``, so the old
    ``__file__``-prefix filter left them behind: the surviving package
    objects kept their stale submodule attributes, and the re-exec'd
    bundle server's ``from mcp.artifact import sync`` bound the STALE
    sync module while the later submodule imports re-registered the
    fresh one — monkeypatches on ``server.sync`` then missed the module
    actually executing. Evicting by name (plus the import-cache
    invalidation) makes every ``mcp.*`` import a fresh one resolved from
    the tree at the front of sys.path.
    """
    importlib.invalidate_caches()
    for name in [
        name for name in sys.modules if name == "mcp" or name.startswith("mcp.")
    ]:
        del sys.modules[name]


def _load_bundle_server() -> Any:
    # The legacy suite (run first in the gate) leaves its own mcp.* in
    # sys.modules; evicting ALL of them (namespace packages included) is
    # safe — nothing later in the gate needs the legacy modules.
    _evict_mcp_modules()
    spec = importlib.util.spec_from_file_location(
        "teamharness_bundle_server_task_meta_test",
        BUNDLE_MCP_DIR / "server.py",
    )
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    sys.path.insert(0, str(BUNDLE_MCP_DIR))
    try:
        spec.loader.exec_module(module)
    finally:
        sys.path.remove(str(BUNDLE_MCP_DIR))
    # The tests patch server.sync; the executing code must come from the
    # SAME module. This identity check is the guard against the loader
    # bug above: a stale sync from the other tree makes every
    # monkeypatch a no-op and the real _commit_task_meta fail closed on
    # the missing AGENTTEAMS_CONTROLLER_URL.
    sync_file = str(getattr(module.sync, "__file__", "") or "")
    assert sync_file.startswith(BUNDLE_MCP_DIR.as_posix() + "/"), (
        f"the bundle server bound a stale sync module: {sync_file!r} "
        f"(expected under {BUNDLE_MCP_DIR.as_posix()})"
    )
    return module


@pytest.fixture()
def server() -> Any:
    module = _load_bundle_server()
    yield module
    _evict_mcp_modules()


def test_bundle_commits_meta_conditionally(server: Any, tmp_path: Path, monkeypatch: Any) -> None:
    _load_legacy_tests().test_sync_task_commits_meta_conditionally(server, tmp_path, monkeypatch)


def test_bundle_keeps_the_remote_cancellation(server: Any, tmp_path: Path, monkeypatch: Any) -> None:
    _load_legacy_tests().test_sync_task_keeps_the_remote_cancellation(server, tmp_path, monkeypatch)


def test_bundle_retries_after_a_conflict(server: Any, tmp_path: Path, monkeypatch: Any) -> None:
    _load_legacy_tests().test_sync_task_retries_after_a_conflict(server, tmp_path, monkeypatch)


def test_bundle_fails_closed_on_a_permanent_refusal(server: Any, tmp_path: Path, monkeypatch: Any) -> None:
    _load_legacy_tests().test_sync_task_fails_closed_on_a_permanent_refusal(server, tmp_path, monkeypatch)


def test_bundle_first_push_includes_meta(server: Any, tmp_path: Path, monkeypatch: Any) -> None:
    _load_legacy_tests().test_sync_task_first_push_includes_meta(server, tmp_path, monkeypatch)


def test_bundle_fails_closed_when_the_version_read_fails(server: Any, tmp_path: Path, monkeypatch: Any) -> None:
    _load_legacy_tests().test_sync_task_fails_closed_when_the_version_read_fails(server, tmp_path, monkeypatch)
