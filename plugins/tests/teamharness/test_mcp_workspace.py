from __future__ import annotations

import importlib.util
from pathlib import Path
import sys


REPO_ROOT = Path(__file__).resolve().parents[3]
MCP_DIR = REPO_ROOT / "plugins" / "teamharness" / "mcp"


def _load_server():
    spec = importlib.util.spec_from_file_location(
        "teamharness_mcp_server_workspace_test",
        MCP_DIR / "server.py",
    )
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    sys.path.insert(0, str(MCP_DIR))
    try:
        spec.loader.exec_module(module)
    finally:
        sys.path.remove(str(MCP_DIR))
    return module


def test_default_workspace_prefers_worker_shared_dir(
    tmp_path: Path,
    monkeypatch,
) -> None:
    server = _load_server()
    shared_dir = tmp_path / "teams" / "demo-team" / "shared"
    monkeypatch.setenv("TEAMHARNESS_SHARED_DIR", str(shared_dir))
    monkeypatch.setenv("QWENPAW_WORKING_DIR", str(tmp_path / "agent" / ".qwenpaw"))

    assert server._default_workspace_dir() == str(shared_dir.parent)
