# payload argument normalization helpers (W1-a2: real module)
from __future__ import annotations

import json

import re

from typing import Any

def _string_list(value: Any) -> list[str]:
    if value is None:
        return []
    if isinstance(value, str):
        text = value.strip()
        if not text:
            return []
        try:
            decoded = json.loads(text)
        except json.JSONDecodeError:
            return [item.strip() for item in text.split(",") if item.strip()]
        return _string_list(decoded)
    if isinstance(value, list):
        return [str(item).strip() for item in value if str(item).strip()]
    return []


def _payload(arguments: dict[str, Any]) -> dict[str, Any]:
    payload = arguments.get("payload")
    if isinstance(payload, dict):
        data = dict(payload)
    elif isinstance(payload, str) and payload.strip():
        try:
            decoded = json.loads(payload)
        except json.JSONDecodeError:
            data = {}
        else:
            data = decoded if isinstance(decoded, dict) else {}
    else:
        data = {}

    aliases = {
        "projectId": ("projectId", "project_id"),
        "taskId": ("taskId", "task_id"),
        "loopId": ("loopId", "loop_id"),
        "roomId": ("roomId", "room_id"),
        "sourceRoomId": ("sourceRoomId", "source_room_id"),
        "sender": ("sender", "senderId", "sender_id", "senderUserId", "sender_user_id", "sourceUserId", "source_user_id"),
        "assignedTo": ("assignedTo", "assigned_to"),
        "dependsOn": ("dependsOn", "depends_on"),
        "replacementTaskId": ("replacementTaskId", "replacement_task_id"),
        "submissionId": ("submissionId", "submission_id"),
    }
    for canonical, keys in aliases.items():
        if any(data.get(key) for key in keys):
            continue
        for key in keys:
            value = arguments.get(key)
            if value is not None:
                data[canonical] = value
                break

    for key in (
        "title", "name", "source", "requester", "spec", "status", "summary", "notes", "topic", "admin",
        "invite", "replyRoute", "reply_route", "accepted", "resultStatus", "result_status", "reason",
        "cancelReason", "cancel_reason",
        "targetUser", "target_user",
    ):
        if key not in data and arguments.get(key) is not None:
            data[key] = arguments[key]

    if "deliverables" not in data:
        if arguments.get("deliverables") is not None:
            data["deliverables"] = arguments["deliverables"]
        elif arguments.get("deliverable") is not None:
            data["deliverables"] = [arguments["deliverable"]]
    if "tasks" not in data and arguments.get("tasks") is not None:
        data["tasks"] = arguments["tasks"]
    return data


def _safe_id(value: Any, field: str) -> str:
    text = str(value or "").strip()
    if not text or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]*", text):
        raise ValueError(f"{field} must be a safe id")
    return text


def _slugify(value: Any, fallback: str) -> str:
    text = re.sub(r"[^A-Za-z0-9]+", "-", str(value or "").strip().lower()).strip("-")
    return text or fallback


def _positive_int(value: Any, field: str) -> int:
    try:
        parsed = int(value)
    except (TypeError, ValueError):
        raise ValueError(f"{field} must be an integer") from None
    if parsed < 1:
        raise ValueError(f"{field} must be greater than zero")
    return parsed


def _non_negative_int(value: Any, field: str) -> int:
    try:
        parsed = int(value)
    except (TypeError, ValueError):
        raise ValueError(f"{field} must be an integer") from None
    if parsed < 0:
        raise ValueError(f"{field} must be zero or greater")
    return parsed


def _payload_bool(value: Any, default: bool) -> bool:
    if value is None:
        return default
    if isinstance(value, bool):
        return value
    text = str(value).strip().lower()
    if text in {"true", "1", "yes", "y", "accepted"}:
        return True
    if text in {"false", "0", "no", "n", "rejected"}:
        return False
    return default


def _payload_bool_field(payload: dict[str, Any], names: tuple[str, ...], default: bool) -> bool:
    for name in names:
        if name in payload:
            return _payload_bool(payload.get(name), default)
    return default


def _first_text(*values: Any) -> str:
    for value in values:
        text = str(value or "").strip()
        if text:
            return text
    return ""


