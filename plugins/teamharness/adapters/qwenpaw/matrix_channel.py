"""TeamHarness Matrix channel helpers for the QwenPaw overlay.

These helpers keep TeamHarness-specific trigger and task-room policy out of the
generic Matrix channel overlay. The overlay imports this module at runtime.
"""

from __future__ import annotations

import json
import logging
import re
import time
from pathlib import Path
from typing import Any, Mapping, MutableMapping, Optional

logger = logging.getLogger(__name__)

TASK_ROOM_CACHE_TTL_MS = 30_000
TEAMHARNESS_TRIGGER_CONTENT_KEY = "m.teamharness.trigger"
TEAMHARNESS_SELF_TRIGGER_TYPES = frozenset({"PROJECT_REQUESTED"})
TEAMHARNESS_TOOL_DISPLAY_RE = re.compile(
    r"^\s*(?:[^\n:]{1,80}:\s*)?🔧\s+(?:\*\*)?[A-Za-z0-9_.-]+(?:\*\*)?",
)


def is_teamharness_tool_display(text: str) -> bool:
    return bool(text and TEAMHARNESS_TOOL_DISPLAY_RE.match(text))


def parse_self_cross_session_trigger(room_id: str, event: Any) -> dict[str, Any] | None:
    """Return TeamHarness self-cross-session trigger metadata when event matches."""
    content = getattr(event, "source", {}).get("content", {})
    if not isinstance(content, dict):
        return None
    trigger = content.get(TEAMHARNESS_TRIGGER_CONTENT_KEY)
    if not isinstance(trigger, dict):
        return None
    if trigger.get("kind") != "self_cross_session":
        return None
    if trigger.get("type") not in TEAMHARNESS_SELF_TRIGGER_TYPES:
        return None
    target_room_id = str(trigger.get("targetRoomId") or "").strip()
    target_session = str(trigger.get("targetSession") or "").strip()
    if target_session.startswith("matrix:"):
        target_session = target_session[len("matrix:") :]
    if target_session.startswith("room:"):
        target_session = target_session[len("room:") :]
    if room_id not in {target_room_id, target_session}:
        return None
    return trigger


def room_has_task_marker(room: Any) -> bool:
    """Return True when room state looks like a TeamHarness task room."""
    if room is None:
        return False
    for attr in ("topic", "name", "display_name"):
        value = getattr(room, attr, "")
        if callable(value):
            continue
        text = str(value or "").strip()
        if text.startswith("Task room for ") or "Task room for " in text:
            return True
    return False


def is_known_task_room(
    room_id: str,
    *,
    workspace_dir: Path,
    cache: MutableMapping[str, Mapping[str, Any]],
    now_ms: Optional[int] = None,
) -> bool:
    """Check shared task metadata for an assignment room id."""
    if not room_id:
        return False
    now = now_ms if now_ms is not None else int(time.time() * 1000)
    cached = cache.get(room_id)
    if cached and (now - cached["ts"]) < TASK_ROOM_CACHE_TTL_MS:
        return bool(cached["is_task_room"])

    is_task_room = False
    tasks_dir = workspace_dir / "shared" / "tasks"
    if tasks_dir.is_dir():
        for meta_path in tasks_dir.glob("*/meta.json"):
            try:
                task = json.loads(meta_path.read_text(encoding="utf-8"))
            except (OSError, json.JSONDecodeError) as exc:
                logger.debug(
                    "TeamHarness matrix: failed to read task room metadata %s: %s",
                    meta_path,
                    exc,
                )
                continue
            task_room_id = str(task.get("room_id") or task.get("roomId") or "").strip()
            if task_room_id == room_id:
                is_task_room = True
                break

    cache[room_id] = {"is_task_room": is_task_room, "ts": now}
    return is_task_room


def looks_like_task_room(
    room_id: str,
    room: Any,
    *,
    workspace_dir: Path,
    cache: MutableMapping[str, Mapping[str, Any]],
) -> bool:
    if room_has_task_marker(room):
        return True
    return is_known_task_room(room_id, workspace_dir=workspace_dir, cache=cache)
