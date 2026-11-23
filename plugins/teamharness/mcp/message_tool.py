#!/usr/bin/env python3
"""TeamHarness message MCP tool implementation."""

from __future__ import annotations

from dataclasses import dataclass
import hashlib
import json
import os
import time
from pathlib import Path
from typing import Any, Callable
import urllib.error
import urllib.parse
import urllib.request

from mcp.common.paths import _default_global_shared_prefix, _default_shared_prefix, _optional_workspace_dir
from mcp.room.matrix import _matrix_user_id
from mcp.transport.coordination import deliver_event
from mcp.common import refs

_filesync = refs.proxy("_filesync")
_publish_workspace_artifact = refs.proxy("_publish_workspace_artifact")


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


def _manager_recipient(recipient: str) -> tuple[str, bool]:
    """Resolve registered aliases and keep standalone messages out of Leader coordination."""
    token = Path(os.environ["AGENTTEAMS_AUTH_TOKEN_FILE"]).read_text().strip()
    controller = os.environ["AGENTTEAMS_CONTROLLER_URL"].rstrip("/")
    request = urllib.request.Request(controller + "/api/v1/workers", headers={"Authorization": "Bearer " + token})
    with urllib.request.urlopen(request, timeout=5) as response:
        workers = json.loads(response.read().decode()).get("workers", [])
    matches = [worker for worker in workers if recipient and recipient in {
        worker.get("name"), worker.get("workerName"), worker.get("matrixUserID"), worker.get("matrixUserId"),
    }]
    if len(matches) != 1:
        raise ValueError("recipient must identify one registered Worker or Team Leader")
    worker = matches[0]
    matrix_id = str(worker.get("matrixUserID") or worker.get("matrixUserId") or "").strip()
    if not _is_matrix_user_id(matrix_id):
        raise ValueError("registered recipient has no Matrix identity")
    if worker.get("team") and worker.get("role") != "team_leader":
        raise ValueError("send the request to this Worker's Team Leader")
    return matrix_id, not bool(worker.get("team"))


def _direct_message_room(homeserver: str, token: str, recipient: str) -> str:
    """Resolve the current sender's private pair, never an admin-owned DM."""
    if not _is_matrix_user_id(recipient) or any(c.isspace() for c in recipient):
        raise ValueError("a full recipient Matrix user ID is required")

    def request(path: str, body: dict[str, Any] | None = None) -> Any:
        req = urllib.request.Request(
            f"{homeserver}/_matrix/client/v3/{path}",
            data=json.dumps(body).encode() if body is not None else None,
            headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
            method="POST" if body is not None else "GET",
        )
        with urllib.request.urlopen(req, timeout=10) as response:
            return json.loads(response.read().decode())

    sender = request("account/whoami")["user_id"]
    if recipient == sender:
        raise ValueError("self-trigger messages require an explicit room target")
    pair = "\0".join(sorted([sender, recipient]))
    local_alias = "agentteams-dm-" + hashlib.sha256(pair.encode()).hexdigest()[:24]
    alias = f"#{local_alias}:{sender.split(':', 1)[1]}"
    directory = "directory/room/" + urllib.parse.quote(alias, safe="")
    try:
        room_id = request(directory)["room_id"]
    except urllib.error.HTTPError as exc:
        if exc.code != 404:
            raise
        try:
            return request("createRoom", {
                "room_alias_name": local_alias, "preset": "private_chat",
                "is_direct": True, "invite": [recipient],
            })["room_id"]
        except urllib.error.HTTPError as create_error:
            if create_error.code != 409:
                raise
            room_id = request(directory)["room_id"]
    room = urllib.parse.quote(room_id, safe="")
    joined = set(request(f"rooms/{room}/joined_members").get("joined", {}))
    if sender not in joined or not joined.issubset({sender, recipient}):
        raise ValueError("direct room membership does not match the sender and recipient")
    if recipient not in joined:
        member = request(f"rooms/{room}/state/m.room.member/" + urllib.parse.quote(recipient, safe=""))
        if member.get("membership") != "invite":
            raise ValueError("recipient is not joined or invited to the direct room")
    return room_id


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
    direct_handoff = target_kind == "user"
    manager_handoff = direct_handoff and refs.resolve("_runtime_role")() == "manager"
    standalone_worker = False
    if manager_handoff:
        if arguments.get("dryRun"):
            return {"ok": True, "tool": "message", "dryRun": True, "targetKind": "user", "target": target_id}
        if os.getenv("AGENTTEAMS_CONTROLLER_URL", "").strip():
            try:
                target_id, standalone_worker = _manager_recipient(target_id)
            except (OSError, ValueError, KeyError) as exc:
                error = str(exc) if isinstance(exc, ValueError) else f"registered recipient lookup failed: {type(exc).__name__}"
                return {"ok": False, "tool": "message", "error": error}
        if standalone_worker and arguments.get("tasks"):
            return {"ok": False, "tool": "message", "error": "structured task handoff requires a Team Leader; send an ordinary message to a standalone Worker"}
    if manager_handoff and not standalone_worker:
        handoff_room = ""
        if (os.getenv("TEAMHARNESS_COORDINATION_URL", "").strip() or arguments.get("tasks")) and not arguments.get("dryRun"):
            try:
                handoff_room = _direct_message_room(os.getenv("AGENTTEAMS_MATRIX_URL", "").rstrip("/"),
                    os.getenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", ""), target_id)
            except (OSError, ValueError, KeyError) as exc:
                return {"ok": False, "tool": "message", "error": f"handoff room unavailable: {exc}"}
        if arguments.get("tasks") and not (os.getenv("TEAMHARNESS_COORDINATION_URL", "").strip() or os.getenv("AGENTTEAMS_CONTROLLER_URL", "").strip()):
            return {"ok": False, "tool": "message", "error": "task handoff requires Controller coordination to register pending main tasks"}
        delivery = deliver_event("handoff", recipient=target_id, text=message_text, tasks=arguments.get("tasks"),
            origin_event_id=arguments.get("originEventId"), source_route=arguments.get("requesterReplyRoute") or {},
            room_id=handoff_room or (arguments.get("requesterReplyRoute") or {}).get("target_session"))
        if delivery is not None:
            return {"ok": bool(delivery.get("sent")), "tool": "message", "transport": "agent-api",
                    "delivery": delivery, "messageId": delivery.get("eventId"), "error": delivery.get("error")}
    if target_kind == "user":
        if _message_type(arguments) in SELF_TRIGGER_MESSAGE_TYPES:
            return {"ok": False, "tool": "message", "error": "PROJECT_REQUESTED requires an explicit room target"}
        if arguments.get("dryRun"):
            return {"ok": True, "tool": "message", "dryRun": True, "targetKind": "user", "target": target_id}
        try:
            target_id = _direct_message_room(
                os.getenv("AGENTTEAMS_MATRIX_URL", "").rstrip("/"),
                os.getenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", ""), target_id,
            )
        except (urllib.error.URLError, TimeoutError, OSError, ValueError, KeyError) as exc:
            return {"ok": False, "tool": "message", "error": f"direct room resolution failed: {exc}"}
        target_kind = "room"

    mentions = deps.mentions(message_text, target_id)
    blocked = deps.ping_pong_error(message_text, mentions)
    if blocked:
        return {"ok": False, "tool": "message", "error": blocked}

    # TH-11: report bodies are capped at 5 lines in the room. A longer
    # body is truncated to the first 5 lines plus a pointer to the full
    # text under shared/reports/ (written only when the message is
    # actually sent, so a dryRun already shows the exact pointer).
    # Mentions were computed above on the full text, so the @target
    # survives the truncation via m.mentions. Self-trigger types
    # (PROJECT_REQUESTED) keep their exact body contract.
    send_text = message_text
    report_path: Path | None = None
    attach_report = False
    if _message_type(arguments) not in SELF_TRIGGER_MESSAGE_TYPES:
        lines = message_text.splitlines()
        if len(lines) > 5:
            workspace_dir = _optional_workspace_dir(arguments)
            if workspace_dir is not None:
                digest = hashlib.sha256(f"{target_id}\0{message_text}".encode("utf-8")).hexdigest()[:16]
                report_relpath = f"shared/reports/{digest}/report.md"
                report_path = workspace_dir / report_relpath
                storage = arguments.get("storage") if isinstance(arguments.get("storage"), dict) else {}
                shared_prefix = str(storage.get("sharedPrefix") or _default_shared_prefix()).strip("/")
                global_prefix = str(storage.get("globalSharedPrefix") or _default_global_shared_prefix()).strip("/")
                # A Manager's shared tree is global; a Team receiver's shared
                # tree is not. Use the existing read-only global namespace.
                read_path = report_relpath
                if shared_prefix == global_prefix:
                    read_path = report_relpath.replace("shared/", "global-shared/", 1)
                    send_text = "\n".join(lines[:5]) + f"\n…中间 {len(lines) - 5} 行见 {read_path}"
                else:
                    attach_report = True
                    send_text = "\n".join(lines[:5]) + "\n…完整报告见本房间附件 report.md"
    content = deps.matrix_content(send_text, mentions)
    api_coordination = bool(os.getenv("TEAMHARNESS_COORDINATION_URL", "").strip()) and not standalone_worker
    if api_coordination:
        if _message_type(arguments) in SELF_TRIGGER_MESSAGE_TYPES:
            return {"ok": False, "error": "API coordination does not dispatch through Matrix; use projectflow start_project or taskflow delegate_task"}
        content["org.agentteams.progress_only"] = True
    coordinator_route = arguments.get("coordinatorReplyRoute")
    if manager_handoff:
        coordinator_route = {"channel": "matrix", "target_session": target_id, "target_user": _matrix_user_id()}
    if isinstance(coordinator_route, dict) and coordinator_route.get("target_session"):
        content["org.agentteams.coordinator_route"] = coordinator_route
        content["body"] = "CoordinatorReplyRoute: " + json.dumps(coordinator_route, ensure_ascii=False) + "\n" + content["body"]
    if isinstance(arguments.get("requesterReplyRoute"), dict):
        content["org.agentteams.requester_route"] = arguments["requesterReplyRoute"]
    if isinstance(arguments.get("workflowEvent"), dict):
        content["org.agentteams.workflow"] = arguments["workflowEvent"]
    if report_path is not None and len(message_text.encode("utf-8")) <= 16384:
        full_text = message_text
        if isinstance(coordinator_route, dict) and coordinator_route.get("target_session"):
            full_text = "CoordinatorReplyRoute: " + json.dumps(coordinator_route, ensure_ascii=False) + "\n" + full_text
        content["org.agentteams.report"] = {"text": full_text}
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
    if api_coordination:
        base["projectionOnly"] = True
    if trigger is not None:
        base["trigger"] = trigger
    if report_path is not None:
        base["fullReportPath"] = str(report_path.relative_to(workspace_dir).as_posix())
        base["reportDelivery"] = {"status": "planned", "readerPath": "room_attachment" if attach_report else read_path}
    if arguments.get("dryRun"):
        base["dryRun"] = True
        return base

    homeserver = os.getenv("AGENTTEAMS_MATRIX_URL", "").rstrip("/")
    token = os.getenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "")
    if not homeserver or not token:
        return {"ok": False, "tool": "message", "error": "AGENTTEAMS_MATRIX_URL and AGENTTEAMS_WORKER_MATRIX_TOKEN are required"}

    if report_path is not None:
        try:
            report_path.parent.mkdir(parents=True, exist_ok=True)
            report_body = message_text if message_text.endswith("\n") else message_text + "\n"
            report_path.write_text(report_body, encoding="utf-8")
        except OSError as exc:
            return {"ok": False, "tool": "message", "error": f"full report could not be written: {exc}"}
        for sync_action in ("push", "stat"):
            sync_args = dict(arguments)
            sync_args.update(action=sync_action, path=base["fullReportPath"])
            if not _filesync(sync_args).get("ok"):
                return {"ok": False, "tool": "message", "error": "full report could not be published to shared storage"}
        if attach_report:
            attachment = _publish_workspace_artifact(
                {**arguments, "publishId": f"report-{digest}"},
                room_id=target_id, source_path=base["fullReportPath"],
                filename="report.md", expected_prefix="shared/reports",
                summary="", mentions=[],
            )
            if attachment.get("status") != "published":
                return {"ok": False, "tool": "message", "error": f"full report attachment failed: {attachment.get('error', 'unknown error')}"}
            base["reportAttachment"] = attachment
        base["reportDelivery"]["status"] = "published"

    room_id = urllib.parse.quote(target_id, safe="")
    txn_id = urllib.parse.quote(str(arguments.get("transactionId") or f"teamharness-{os.getpid()}-{int(time.time() * 1000)}"), safe="")
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
        if not base["messageId"]:
            return {"ok": False, "tool": "message", "error": "Matrix did not return a message event ID; delivery is not confirmed"}
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
        base.setdefault("warning", "message sent, but local session record failed")
    return base
