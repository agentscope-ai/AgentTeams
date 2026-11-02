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


def _load_bundle_server() -> Any:
    legacy_root = LEGACY_MCP_DIR.as_posix() + "/"
    for name in [
        name
        for name, module in sys.modules.items()
        if name == "mcp" or name.startswith("mcp.")
    ]:
        file = getattr(sys.modules[name], "__file__", None)
        if file and file.replace("\\", "/").startswith(legacy_root):
            del sys.modules[name]
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
    return module


@pytest.fixture()
def server(monkeypatch: pytest.MonkeyPatch) -> Any:
    module = _load_bundle_server()
    yield module
    legacy_root = BUNDLE_MCP_DIR.as_posix() + "/"
    for name in [
        name
        for name, module in sys.modules.items()
        if name == "mcp" or name.startswith("mcp.")
    ]:
        file = getattr(sys.modules[name], "__file__", None)
        if file and file.replace("\\", "/").startswith(legacy_root):
            del sys.modules[name]


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
