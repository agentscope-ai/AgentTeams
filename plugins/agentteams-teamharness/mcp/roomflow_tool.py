#!/usr/bin/env python3
"""TeamHarness roomflow MCP helper implementation."""

from __future__ import annotations

from dataclasses import dataclass
import json
from typing import Any, Callable
import urllib.error
import urllib.parse
import urllib.request


@dataclass(frozen=True)
class RoomDescribeDeps:
    matrix_env: Callable[[str], tuple[str, str]]
    matrix_user_id: Callable[[], str]
    canonical_room_id: Callable[[Any], str]


def _session_room_id(arguments: dict[str, Any], payload: dict[str, Any], deps: RoomDescribeDeps) -> str:
    raw = str(
        payload.get("sessionId")
        or payload.get("session_id")
        or payload.get("roomId")
        or payload.get("room_id")
        or arguments.get("sessionId")
        or arguments.get("session_id")
        or arguments.get("roomId")
        or arguments.get("room_id")
        or arguments.get("target")
        or ""
    ).strip()
    if raw.startswith("matrix:"):
        raw = raw[len("matrix:") :].strip()
    return deps.canonical_room_id(raw)


def _matrix_get_json(path: str, token: str) -> dict[str, Any]:
    request = urllib.request.Request(
        path,
        headers={"Authorization": f"Bearer {token}"},
        method="GET",
    )
    with urllib.request.urlopen(request, timeout=10) as response:
        data = json.loads(response.read().decode("utf-8") or "{}")
    return data if isinstance(data, dict) else {}


def _matrix_get_json_optional(path: str, token: str) -> dict[str, Any]:
    try:
        return _matrix_get_json(path, token)
    except urllib.error.HTTPError as exc:
        if exc.code == 404:
            return {}
        raise


def describe_room(arguments: dict[str, Any], payload: dict[str, Any], deps: RoomDescribeDeps) -> dict[str, Any]:
    room_id = _session_room_id(arguments, payload, deps)
    if not room_id:
        return {"ok": False, "tool": "roomflow", "action": "describe_room", "error": "sessionId or roomId is required"}
    base: dict[str, Any] = {
        "ok": True,
        "tool": "roomflow",
        "action": "describe_room",
        "roomId": room_id,
        "sessionId": f"matrix:{room_id}",
        "name": "",
        "topic": "",
        "tags": {},
    }
    if arguments.get("dryRun"):
        base["dryRun"] = True
        return base
    try:
        homeserver, token = deps.matrix_env("roomflow")
    except ValueError as exc:
        return {"ok": False, "tool": "roomflow", "action": "describe_room", "error": str(exc)}

    encoded_room = urllib.parse.quote(room_id, safe="")
    try:
        name = _matrix_get_json_optional(
            f"{homeserver}/_matrix/client/v3/rooms/{encoded_room}/state/m.room.name",
            token,
        )
        topic = _matrix_get_json_optional(
            f"{homeserver}/_matrix/client/v3/rooms/{encoded_room}/state/m.room.topic",
            token,
        )
        user_id = deps.matrix_user_id()
        tags: dict[str, Any] = {}
        if user_id:
            encoded_user = urllib.parse.quote(user_id, safe="")
            tag_data = _matrix_get_json_optional(
                f"{homeserver}/_matrix/client/v3/user/{encoded_user}/rooms/{encoded_room}/tags",
                token,
            )
            raw_tags = tag_data.get("tags")
            tags = raw_tags if isinstance(raw_tags, dict) else {}
    except urllib.error.HTTPError as exc:
        error = exc.read().decode("utf-8", errors="replace")[:200]
        return {"ok": False, "tool": "roomflow", "action": "describe_room", "error": f"Matrix API error: HTTP {exc.code}: {error}"}
    except (urllib.error.URLError, TimeoutError, OSError) as exc:
        return {"ok": False, "tool": "roomflow", "action": "describe_room", "error": f"Matrix API error: {exc}"}

    base["name"] = str(name.get("name") or "").strip()
    base["topic"] = str(topic.get("topic") or "").strip()
    base["tags"] = tags
    return base
