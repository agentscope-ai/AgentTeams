"""Plugin version info (v0.2.0 B5: GET /version)."""

from __future__ import annotations

import json
import platform
import shutil
import time
from pathlib import Path

from . import state

_PLUGIN_ROOT = Path(__file__).resolve().parent.parent
_STARTED_AT = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def plugin_version() -> str:
    """Read ``version`` from plugin.json; "0.0.0" when unavailable."""
    try:
        data = json.loads(
            (_PLUGIN_ROOT / "plugin.json").read_text(encoding="utf-8")
        )
        return str(data.get("version") or "0.0.0")
    except (OSError, ValueError):
        return "0.0.0"


def version_info() -> dict:
    """Payload for ``GET /api/qwen-code-bridge/version``."""
    cli = ""
    try:
        cli = state.qwen_cli() or shutil.which("qwen") or ""
    except Exception:
        cli = ""
    return {
        "plugin": plugin_version(),
        "started_at": _STARTED_AT,
        "python": platform.python_version(),
        "qwen_cli": cli,
        "state_dir": str(state.state_dir()),
    }
