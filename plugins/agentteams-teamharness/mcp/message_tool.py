#!/usr/bin/env python3
"""TeamHarness message MCP tool implementation."""

from __future__ import annotations

from dataclasses import dataclass
import json
import os
import time
from typing import Any, Callable
import urllib.error
import urllib.parse
import urllib.request


SELF_TRIGGER_MESSAGE_TYPES = {"PROJECT_REQUESTED"}
TEAMHARNESS_TRIGGER_CONTENT_KEY = "m.teamharness.trigger"


@dataclass(frozen=True)
class MessageToolDeps:
    reply_route: Callable[[dict[str, Any]], dict[str, Any]]
    qwenpaw_message: Callable[[dict[str, Any], dict[str, Any], str, str], dict[str, Any]]
    matrix_target: Callable[[str], tuple[str, str]]
    mentions: Callable[[str, str], list[str]]
    ping_pong_error: Callable[[str, list[str]], str | None]
    matrix_content: Callable[[str, list[str]], dict[str, Any]]
    record_matrix_outbound_to_session: Callable[[str, str, str | None, str], bool]


def _dict(value: Any) -> dict[str, Any]:
    return value if isinstance(value, dict) else {}


def _text(value: Any) -> str:
    return str(value).strip() if value is not None else ""


def _message_object(arguments: dict[str, Any]) -> dict[str, Any]:
    message = arguments.get("message")
    if isinstance(message, dict):
        return message
    if isinstance(message, str):
        raw = message.strip()
        if raw.startswith("{"):
            try:
                parsed = json.loads(raw)
            except json.JSONDecodeError:
                return {}
            return parsed if isinstance(parsed, dict) else {}
    return {}


def _message_text(arguments: dict[str, Any]) -> str:
    message = arguments.get("message")
    message_obj = _message_object(arguments)
    if message_obj:
        for key in ("text", "body", "message", "content"):
            value = message_obj.get(key)
            if value is not None:
                return str(value)
        return ""
    if message is not None:
        return str(message)
    return str(arguments.get("text") or arguments.get("body") or "")


def _message_type(arguments: dict[str, Any]) -> str:
    for key in ("type", "messageType", "message_type"):
        value = arguments.get(key)
        if value is not None:
            return _text(value)
    message = _message_object(arguments)
    if message:
        for key in ("type", "messageType", "message_type"):
            value = message.get(key)
            if value is not None:
                return _text(value)
    return ""


def _agent_from(value: Any) -> str:
    data = _dict(value)
    for key in ("agent", "agentId", "agent_id", "accountId", "account_id", "runtimeName", "runtime_name", "name"):
        text = _text(data.get(key))
        if text:
            return text
    return ""


def _current_agent(arguments: dict[str, Any]) -> str:
    for key in ("agentId", "agent_id", "accountId", "account_id"):
        value = _text(arguments.get(key))
        if value:
            return value
    return _text(os.getenv("AGENTTEAMS_WORKER_NAME")) or "default"


def _is_matrix_user_id(value: str) -> bool:
    text = value.strip()
    return text.startswith("@") and ":" in text


def _session_id_from(value: Any) -> str:
    if isinstance(value, str):
        return value.strip()
    data = _dict(value)
    session = data.get("session")
    if isinstance(session, str):
        return session.strip()
    if isinstance(session, dict):
        for key in (
            "id",
            "sessionId",
            "session_id",
            "targetSession",
            "target_session",
            "sourceSession",
            "source_session",
            "roomId",
            "room_id",
        ):
            text = _text(session.get(key))
            if text:
                return text
    for key in (
        "id",
        "sessionId",
        "session_id",
        "targetSession",
        "target_session",
        "sourceSession",
        "source_session",
        "roomId",
        "room_id",
        "target",
    ):
        text = _text(data.get(key))
        if text:
            return text
    return ""


def _target_value(arguments: dict[str, Any], route: dict[str, Any]) -> str:
    for value in (
        arguments.get("target"),
        route.get("target"),
        arguments.get("room_id"),
        arguments.get("roomId"),
        route.get("room_id"),
        route.get("roomId"),
        route.get("targetRoom"),
        route.get("target_room"),
        route.get("targetSession"),
        route.get("target_session"),
    ):
        target = _session_id_from(value)
        if target:
            return target
    return ""


def _source_value(arguments: dict[str, Any], route: dict[str, Any]) -> str:
    for value in (
        arguments.get("sourceSession"),
        arguments.get("source_session"),
        arguments.get("senderSession"),
        arguments.get("sender_session"),
        arguments.get("currentSession"),
        arguments.get("current_session"),
        route.get("sourceSession"),
        route.get("source_session"),
        arguments.get("sender"),
        arguments.get("source"),
    ):
        source = _session_id_from(value)
        if source:
            return source
    return ""


def _channel_from(value: Any) -> str:
    data = _dict(value)
    for key in ("channel", "channelId", "channel_id"):
        text = _text(data.get(key)).lower()
        if text:
            return text
    session = data.get("session")
    if isinstance(session, dict):
        for key in ("channel", "channelId", "channel_id"):
            text = _text(session.get(key)).lower()
            if text:
                return text
    return ""


def _source_channel(arguments: dict[str, Any], route: dict[str, Any], source_session: str) -> str:
    for value in (
        arguments.get("sourceChannel"),
        arguments.get("source_channel"),
        arguments.get("senderChannel"),
        arguments.get("sender_channel"),
        route.get("sourceChannel"),
        route.get("source_channel"),
        arguments.get("sender"),
        arguments.get("source"),
    ):
        channel = _channel_from(value) if isinstance(value, dict) else _text(value).lower()
        if channel:
            return channel
    if _matrix_room_id_from_session(source_session):
        return "matrix"
    raw = source_session.strip()
    if ":" in raw:
        prefix = raw.split(":", 1)[0].strip().lower()
        if prefix and not prefix.startswith("!"):
            return prefix
    return ""


def _matrix_room_id_from_session(value: str) -> str:
    raw = (value or "").strip()
    if raw.startswith("matrix:"):
        raw = raw[len("matrix:") :]
    if raw.startswith("room:"):
        raw = raw[len("room:") :]
    return raw if raw.startswith("!") else ""


def _canonical_session(channel: str, session: str) -> str:
    raw = session.strip()
    source_channel = channel.strip().lower()
    if source_channel == "matrix":
        room_id = _matrix_room_id_from_session(raw)
        return f"matrix:{room_id}" if room_id else raw
    if source_channel and raw.startswith(f"{source_channel}:"):
        return raw
    return f"{source_channel}:{raw}" if source_channel else raw


def _route_bool(route: dict[str, Any], *names: str) -> bool | None:
    for name in names:
        if name not in route:
            continue
        value = route.get(name)
        if isinstance(value, bool):
            return value
        if isinstance(value, str):
            text = value.strip().lower()
            if text in {"1", "true", "yes", "y", "on"}:
                return True
            if text in {"0", "false", "no", "n", "off"}:
                return False
        if value is not None:
            return bool(value)
    return None


def _normalize_reply_route(route: dict[str, Any]) -> dict[str, Any]:
    channel = _text(route.get("channel")).lower()
    target_user = _text(
        route.get("targetUser")
        or route.get("target_user")
        or route.get("userId")
        or route.get("user_id"),
    )
    target_session = _text(
        route.get("targetSession")
        or route.get("target_session")
        or route.get("sessionId")
        or route.get("session_id"),
    )
    if channel == "matrix":
        target = _text(
            route.get("target")
            or route.get("roomId")
            or route.get("room_id")
            or route.get("targetRoom")
            or route.get("target_room")
            or target_session,
        )
        room_id = _matrix_room_id_from_session(target)
        if not room_id:
            return {}
        normalized: dict[str, Any] = {
            "channel": "matrix",
            "targetSession": room_id,
        }
        if target_user:
            normalized["targetUser"] = target_user
    else:
        if not (channel and target_user and target_session):
            return {}
        normalized = {
            "channel": channel,
            "targetUser": target_user,
            "targetSession": target_session,
        }
    mention_sender = _route_bool(route, "mentionSender", "mention_sender", "atSender", "at_sender")
    if mention_sender is not None:
        normalized["mentionSender"] = mention_sender
    return normalized


def _visible_reply_route_error(message_text: str, reply_route: dict[str, Any]) -> str:
    channel = _text(reply_route.get("channel")).lower()
    if channel and channel != "matrix" and channel not in message_text:
        return "PROJECT_REQUESTED message text must include replyRoute.channel so the task-room Leader can pass it to projectflow"
    target_user = _text(reply_route.get("targetUser") or reply_route.get("target_user"))
    if channel and channel != "matrix" and target_user and target_user not in message_text:
        return "PROJECT_REQUESTED message text must include replyRoute.targetUser so the task-room Leader can pass it to projectflow"
    target_session = _text(reply_route.get("targetSession") or reply_route.get("target_session"))
    if target_session and target_session not in message_text:
        return "PROJECT_REQUESTED message text must include replyRoute.targetSession so the task-room Leader can pass it to projectflow"
    return ""


def _self_trigger_intent(
    arguments: dict[str, Any],
    route: dict[str, Any],
    *,
    channel: str,
    target_id: str,
) -> dict[str, Any] | None:
    message_type = _message_type(arguments)
    if message_type not in SELF_TRIGGER_MESSAGE_TYPES:
        return None
    source_session = _source_value(arguments, route)
    if not source_session:
        return None
    source_channel = _source_channel(arguments, route, source_session)
    if not source_channel or channel.strip().lower() != "matrix":
        return None
    target_session = f"matrix:{target_id}"
    canonical_source_session = _canonical_session(source_channel, source_session)
    if canonical_source_session == target_session:
        return None

    current_agent = _current_agent(arguments)
    source_agent = _agent_from(arguments.get("sender")) or _agent_from(arguments.get("source")) or current_agent
    target_agent = _agent_from(arguments.get("target")) or current_agent
    if source_agent != target_agent:
        return None
    if not _is_matrix_user_id(source_agent):
        return {
            "status": "invalid",
            "kind": "self_cross_session",
            "type": message_type,
            "error": "PROJECT_REQUESTED sender.agent and agentId must be the current runtime Matrix user id, not a role or workspace name",
        }
    reply_route = _normalize_reply_route(route)
    if not reply_route:
        return {
            "status": "invalid",
            "kind": "self_cross_session",
            "type": message_type,
            "error": "PROJECT_REQUESTED requires structured replyRoute with channel and targetSession",
        }

    return {
        "status": "requested",
        "kind": "self_cross_session",
        "type": message_type,
        "agentId": target_agent,
        "sourceChannel": source_channel,
        "targetChannel": "matrix",
        "sourceSession": canonical_source_session,
        "targetSession": target_session,
        "sourceRoomId": source_session.strip(),
        "targetRoomId": target_id,
        "replyRoute": reply_route,
    }


def message(arguments: dict[str, Any], deps: MessageToolDeps) -> dict[str, Any]:
    action = arguments.get("action") or "send"
    route = deps.reply_route(arguments)
    channel = str(arguments.get("channel") or route.get("channel") or "matrix")
    if action != "send":
        return {"ok": False, "tool": "message", "error": f"unsupported action: {action}"}
    message_text = _message_text(arguments)
    if channel != "matrix":
        return deps.qwenpaw_message(arguments, route, channel, message_text)

    try:
        target_kind, target_id = deps.matrix_target(_target_value(arguments, route))
    except ValueError as exc:
        return {"ok": False, "tool": "message", "error": str(exc)}
    if target_kind == "user":
        return {"ok": False, "tool": "message", "error": "Matrix user targets are not supported yet"}

    mentions = deps.mentions(message_text, target_id)
    blocked = deps.ping_pong_error(message_text, mentions)
    if blocked:
        return {"ok": False, "tool": "message", "error": blocked}

    content = deps.matrix_content(message_text, mentions)
    trigger = _self_trigger_intent(arguments, route, channel=channel, target_id=target_id)
    if _message_type(arguments) in SELF_TRIGGER_MESSAGE_TYPES and trigger is None:
        return {
            "ok": False,
            "tool": "message",
            "action": "send",
            "channel": "matrix",
            "target": f"room:{target_id}",
            "error": "PROJECT_REQUESTED must be sent as a same-agent Matrix self-trigger with sender.session, target room, matching sender.agent/agentId, and structured replyRoute",
        }
    if trigger is not None and trigger.get("status") == "invalid":
        return {
            "ok": False,
            "tool": "message",
            "action": "send",
            "channel": "matrix",
            "target": f"room:{target_id}",
            "trigger": trigger,
            "error": str(trigger.get("error") or "invalid PROJECT_REQUESTED trigger"),
        }
    if trigger is not None:
        visible_route_error = _visible_reply_route_error(message_text, _dict(trigger.get("replyRoute")))
        if visible_route_error:
            return {
                "ok": False,
                "tool": "message",
                "action": "send",
                "channel": "matrix",
                "target": f"room:{target_id}",
                "trigger": trigger,
                "error": visible_route_error,
            }
    if trigger is not None:
        content = dict(content)
        content[TEAMHARNESS_TRIGGER_CONTENT_KEY] = dict(trigger)
    base: dict[str, Any] = {
        "ok": True,
        "tool": "message",
        "action": "send",
        "channel": "matrix",
        "target": f"room:{target_id}",
        "targetKind": "room",
        "mentions": mentions,
        "content": content,
    }
    if trigger is not None:
        base["trigger"] = trigger
    if arguments.get("dryRun"):
        base["dryRun"] = True
        return base

    homeserver = os.getenv("AGENTTEAMS_MATRIX_URL", "").rstrip("/")
    token = os.getenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "")
    if not homeserver or not token:
        return {"ok": False, "tool": "message", "error": "AGENTTEAMS_MATRIX_URL and AGENTTEAMS_WORKER_MATRIX_TOKEN are required"}

    room_id = urllib.parse.quote(target_id, safe="")
    txn_id = f"teamharness-{os.getpid()}-{int(time.time() * 1000)}"
    url = f"{homeserver}/_matrix/client/v3/rooms/{room_id}/send/m.room.message/{txn_id}"
    request = urllib.request.Request(
        url,
        data=json.dumps(content).encode("utf-8"),
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
        },
        method="PUT",
    )
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            data = json.loads(response.read().decode("utf-8") or "{}")
        base["messageId"] = data.get("event_id")
    except urllib.error.HTTPError as exc:
        body = exc.read().decode("utf-8", errors="replace")[:200]
        return {"ok": False, "tool": "message", "error": f"Matrix API error: HTTP {exc.code}: {body}"}
    except (urllib.error.URLError, TimeoutError, OSError) as exc:
        return {"ok": False, "tool": "message", "error": f"Matrix API error: {exc}"}
    if trigger is not None:
        trigger["status"] = "sent"
        trigger["targetCurrentEvent"] = base.get("messageId")
        base["delivery"] = {"sent": "matrix_self_trigger"}
        base["context"] = {"via": "matrix_current_event"}
        return base
    account_id = str(arguments.get("agentId") or arguments.get("agent_id") or arguments.get("accountId") or arguments.get("account_id") or "default").strip() or "default"
    try:
        base["sessionRecorded"] = deps.record_matrix_outbound_to_session(
            target_id,
            message_text,
            base.get("messageId"),
            account_id,
        )
    except Exception:
        base["sessionRecorded"] = False
        base["warning"] = "message sent, but local session record failed"
    return base
