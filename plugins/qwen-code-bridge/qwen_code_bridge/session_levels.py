# -*- coding: utf-8 -*-
"""Last-observed session approval level (per-session, client-only today).

Why this exists
---------------
QwenPaw's chat-level approval switch is **client state**: the console puts
``request_context.approval_level`` on each UI turn but never persists it, so a
server-injected wake turn has no way to read "the mode the user is actually
in". The bridge used to pin the level captured at **dispatch** time, which is
stale the moment the user changes the switch (2026-10-25 incident: the wake
overrode the user's OFF session with the dispatch-time AUTO, so a benign
cleanup command was held for approval).

The middleware sees every UI turn's ``request_context``; recording the level
per session gives the wake path the freshest value the server can possibly
know, without changing QwenPaw.

State: ``<state>/session-levels.json`` — ``{session_id: {level, ts}}``,
bounded to the 200 most recently seen sessions, atomic tmp+rename writes.
"""

from __future__ import annotations

import json
import os
import threading
import time
from typing import Any

from .state import state_dir

LEVELS = frozenset({"STRICT", "SMART", "AUTO", "OFF"})
MAX_ENTRIES = 200

_lock = threading.Lock()
_cache: dict[str, dict[str, Any]] | None = None


def _path():
    return state_dir() / "session-levels.json"


def _load_locked() -> dict[str, dict[str, Any]]:
    global _cache
    if _cache is not None:
        return _cache
    data: dict[str, dict[str, Any]] = {}
    try:
        raw = json.loads(_path().read_text(encoding="utf-8"))
        if isinstance(raw, dict):
            for key, value in raw.items():
                if (
                    isinstance(key, str)
                    and isinstance(value, dict)
                    and str(value.get("level") or "").upper() in LEVELS
                ):
                    data[key] = {
                        "level": str(value["level"]).upper(),
                        "ts": float(value.get("ts") or 0.0),
                    }
    except Exception:
        data = {}
    _cache = data
    return data


def _save_locked(data: dict[str, dict[str, Any]]) -> None:
    path = _path()
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(path.name + ".tmp")
    tmp.write_text(
        json.dumps(data, ensure_ascii=False, indent=1), encoding="utf-8"
    )
    os.replace(tmp, path)


def record(session_id: str, level: str) -> None:
    """Persist the level seen on a turn (no-op for empty/invalid values)."""
    sid = str(session_id or "").strip()
    value = str(level or "").strip().upper()
    if not sid or value not in LEVELS:
        return
    with _lock:
        data = _load_locked()
        if (data.get(sid) or {}).get("level") == value:
            return
        data[sid] = {"level": value, "ts": time.time()}
        if len(data) > MAX_ENTRIES:
            overflow = len(data) - MAX_ENTRIES
            for key in sorted(
                data, key=lambda k: float(data[k].get("ts") or 0.0)
            )[:overflow]:
                data.pop(key, None)
        try:
            _save_locked(data)
        except Exception:
            pass


def last_level(session_id: str) -> str:
    """Return the last recorded level for a session ("" when unknown)."""
    sid = str(session_id or "").strip()
    if not sid:
        return ""
    with _lock:
        return str((_load_locked().get(sid) or {}).get("level") or "")


def reset_cache() -> None:
    """Test hook: force the next read to hit disk."""
    global _cache
    with _lock:
        _cache = None


__all__ = ["LEVELS", "record", "last_level", "reset_cache"]
