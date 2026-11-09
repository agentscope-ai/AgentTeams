# replyRoute parsing and normalization (W1-a2: real module)
from __future__ import annotations

from typing import Any

# Cross-group imports are top-level absolutes (see lifecycle/transitions.py
# note): mcp/ on sys.path keeps one module identity in both load modes.
from mcp.common.payload import _payload_bool
from mcp.room.mention import _matrix_target


def _reply_route(arguments: dict[str, Any]) -> dict[str, Any]:
    route = arguments.get("replyRoute") or arguments.get("reply_route")
    return route if isinstance(route, dict) else {}


def _route_value(arguments: dict[str, Any], route: dict[str, Any], *names: str) -> str:
    for name in names:
        value = route.get(name)
        if value is None:
            value = arguments.get(name)
        if value is not None:
            return str(value).strip()
    return ""


def _route_bool(arguments: dict[str, Any], route: dict[str, Any], *names: str, default: bool = False) -> bool:
    for name in names:
        if name in route:
            return _payload_bool(route.get(name), default)
        if name in arguments:
            return _payload_bool(arguments.get(name), default)
    return default


def _normalize_reply_route(raw: Any) -> dict[str, str]:
    route = raw if isinstance(raw, dict) else {}
    channel = str(route.get("channel") or "").strip()
    target_user = str(route.get("targetUser") or route.get("target_user") or route.get("userId") or route.get("user_id") or "").strip()
    target_session = str(route.get("targetSession") or route.get("target_session") or route.get("sessionId") or route.get("session_id") or "").strip()
    if channel.lower() == "matrix":
        target = str(
            route.get("target")
            or route.get("roomId")
            or route.get("room_id")
            or route.get("targetRoom")
            or route.get("target_room")
            or target_session
            or ""
        ).strip()
        if not target:
            return {}
        try:
            target_kind, target_id = _matrix_target(target)
        except ValueError:
            return {}
        if target_kind != "room":
            return {}
        normalized = {
            "channel": "matrix",
            "target_session": target_id,
        }
        if target_user:
            normalized["target_user"] = target_user
        return normalized
    if not (channel and target_user and target_session):
        return {}
    return {
        "channel": channel,
        "target_user": target_user,
        "target_session": target_session,
    }


def _reply_route_from_requester(requester: Any) -> dict[str, str]:
    text = str(requester or "").strip()
    if text.startswith("matrix:"):
        try:
            target_kind, target_id = _matrix_target(text)
        except ValueError:
            return {}
        if target_kind != "room":
            return {}
        return {
            "channel": "matrix",
            "target_session": target_id,
        }
    if not text.startswith("dingtalk:"):
        return {}
    parts = text.split(":", 2)
    if len(parts) != 3 or not parts[1] or not parts[2]:
        return {}
    return {
        "channel": "dingtalk",
        "target_user": parts[1],
        "target_session": parts[2],
    }


