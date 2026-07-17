"""Ensure plugin and protocol packages are importable from MCP entrypoints."""

from __future__ import annotations

import sys
from pathlib import Path

_MCP_DIR = Path(__file__).resolve().parent
_PLUGINS_DIR = _MCP_DIR.parents[1]
_REPO_ROOT = _MCP_DIR.parents[2]
_PROTOCOL_SRC = _REPO_ROOT / "shared" / "python" / "agentteams_protocol" / "src"
_SYNC_SRC = _REPO_ROOT / "shared" / "python" / "agentteams_sync" / "src"


def ensure_import_paths() -> None:
    for path in (_PLUGINS_DIR, _PROTOCOL_SRC, _SYNC_SRC):
        text = str(path)
        if text not in sys.path:
            sys.path.insert(0, text)


ensure_import_paths()
