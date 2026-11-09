# message tool implementation and session recording (W1-a2: real module)
from __future__ import annotations

import json

import os

import re

import threading

import time

import uuid

from pathlib import Path

from typing import Any

import urllib.error

import urllib.request

# Cross-group imports are top-level absolutes (see lifecycle/transitions.py
# note): mcp/ on sys.path keeps one module identity in both load modes.
from mcp.artifact.publish import _write_matrix_attachment_context_parent_event_id
from mcp.common.io import _read_json
from mcp.message_tool import MessageToolDeps, message as _message_impl
from mcp.room.mention import _matrix_content, _matrix_target, _mentions, _ping_pong_error
from .route import _reply_route, _route_bool, _route_value


UNSAFE_SESSION_FILENAME_RE = re.compile(r'[\\/:*?"<>|]')


SESSION_WRITE_LOCKS: dict[str, threading.Lock] = {}


def _qwenpaw_message(arguments: dict[str, Any], route: dict[str, Any], channel: str, message: str) -> dict[str, Any]:
    target_user = _route_value(arguments, route, "targetUser", "target_user", "userId", "user_id")
    target_session = _route_value(arguments, route, "targetSession", "target_session", "sessionId", "session_id")
    agent_id = str(arguments.get("agentId") or arguments.get("agent_id") or arguments.get("accountId") or "default").strip()
    mention_sender = _route_bool(arguments, route, "mentionSender", "mention_sender", "atSender", "at_sender")
    base: dict[str, Any] = {
        "ok": True,
        "tool": "message",
        "action": "send",
        "channel": channel,
        "targetUser": target_user,
        "targetSession": target_session,
        "agentId": agent_id or "default",
    }
    if message:
        base["message"] = message
    if mention_sender:
        base["mentionSender"] = True
    if not target_user:
        return {"ok": False, "tool": "message", "channel": channel, "error": "targetUser is required for non-Matrix channel sends"}
    if not target_session:
        return {"ok": False, "tool": "message", "channel": channel, "error": "targetSession is required for non-Matrix channel sends"}
    if not message:
        return {"ok": False, "tool": "message", "channel": channel, "error": "message text is required"}
    if arguments.get("dryRun"):
        base["dryRun"] = True
        return base

    if channel.strip().lower() == "dingtalk" and mention_sender:
        mention_result = _send_dingtalk_sender_mention(
            arguments=arguments,
            route=route,
            target_user=target_user,
            target_session=target_session,
            account_id=agent_id or "default",
            message=message,
        )
        if mention_result.get("ok"):
            base["response"] = mention_result.get("response", {})
            base["senderMentioned"] = True
            base["mentionedSender"] = mention_result.get("mentionedSender", "")
            try:
                base["sessionRecorded"] = _record_outbound_to_session(
                    channel=channel,
                    user_id=target_user,
                    session_id=target_session,
                    text=message,
                    message_id=None,
                    account_id=agent_id or "default",
                    metadata={
                        "user_id": target_user,
                        "session_id": target_session,
                        "sender_mentioned": True,
                        "mentioned_sender": mention_result.get("mentionedSender", ""),
                    },
                )
            except Exception:
                base["sessionRecorded"] = False
                base["warning"] = "message sent, but local session record failed"
            return base
        warning = str(mention_result.get("warning") or mention_result.get("error") or "DingTalk sender mention unavailable")
        base["ok"] = False
        base["error"] = warning
        base["delivery"] = {"failed": "dingtalk_sender_mention_required"}
        base["senderMentionWarning"] = warning
        return base

    api_base = (os.getenv("QWENPAW_API_BASE") or os.getenv("COPAW_API_BASE") or "http://127.0.0.1:8088").rstrip("/")
    api_path = "/messages/send" if api_base.endswith("/api") else "/api/messages/send"
    body = {
        "channel": channel,
        "target_user": target_user,
        "target_session": target_session,
        "text": message,
    }
    request = urllib.request.Request(
        f"{api_base}{api_path}",
        data=json.dumps(body).encode("utf-8"),
        headers={
            "Content-Type": "application/json",
            "X-Agent-Id": agent_id or "default",
        },
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            data = json.loads(response.read().decode("utf-8") or "{}")
        base["response"] = data
    except urllib.error.HTTPError as exc:
        body_text = exc.read().decode("utf-8", errors="replace")[:200]
        return {"ok": False, "tool": "message", "channel": channel, "error": f"QwenPaw message API error: HTTP {exc.code}: {body_text}"}
    except (urllib.error.URLError, TimeoutError, OSError) as exc:
        return {"ok": False, "tool": "message", "channel": channel, "error": f"QwenPaw message API error: {exc}"}
    try:
        base["sessionRecorded"] = _record_outbound_to_session(
            channel=channel,
            user_id=target_user,
            session_id=target_session,
            text=message,
            message_id=None,
            account_id=agent_id or "default",
        )
    except Exception:
        base["sessionRecorded"] = False
        base["warning"] = "message sent, but local session record failed"
    return base


def _send_dingtalk_sender_mention(
    *,
    arguments: dict[str, Any],
    route: dict[str, Any],
    target_user: str,
    target_session: str,
    account_id: str,
    message: str,
) -> dict[str, Any]:
    entry = _dingtalk_session_webhook_entry(target_user, target_session, account_id)
    webhook = str(entry.get("webhook") or "").strip()
    sender_staff_id = str(entry.get("sender_staff_id") or "").strip()
    explicit_sender = _route_value(arguments, route, "senderStaffId", "sender_staff_id", "senderUserId", "sender_user_id")
    conversation_type = str(entry.get("conversation_type") or "").strip().lower()

    if not webhook:
        return {"ok": False, "warning": "DingTalk session webhook not found; sent without sender mention"}
    if not sender_staff_id:
        return {"ok": False, "warning": "DingTalk sender_staff_id not found; sent without sender mention"}
    if explicit_sender and explicit_sender != sender_staff_id:
        return {"ok": False, "warning": "DingTalk senderStaffId does not match recorded sender; sent without sender mention"}
    if conversation_type and conversation_type != "group":
        return {"ok": False, "warning": "DingTalk sender mention is only applied to group conversations"}

    text = f"@{sender_staff_id}\n{message}"
    body: dict[str, Any]
    if len(text) > 3500:
        body = {
            "msgtype": "text",
            "text": {"content": text},
        }
    else:
        body = {
            "msgtype": "markdown",
            "markdown": {
                "title": "TeamHarness report",
                "text": text,
            },
        }
    body["at"] = {"atUserIds": [sender_staff_id]}

    request = urllib.request.Request(
        webhook,
        data=json.dumps(body).encode("utf-8"),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            response_text = response.read().decode("utf-8", errors="replace")
        data = json.loads(response_text or "{}")
    except urllib.error.HTTPError as exc:
        body_text = exc.read().decode("utf-8", errors="replace")[:200]
        return {"ok": False, "error": f"DingTalk session webhook error: HTTP {exc.code}: {body_text}"}
    except (json.JSONDecodeError, urllib.error.URLError, TimeoutError, OSError) as exc:
        return {"ok": False, "error": f"DingTalk session webhook error: {exc}"}

    if not isinstance(data, dict):
        return {"ok": False, "error": "DingTalk session webhook returned a non-object response"}
    errcode = data.get("errcode")
    if errcode not in (None, 0, "0"):
        return {"ok": False, "error": f"DingTalk session webhook rejected message: {data}"}
    return {
        "ok": True,
        "response": data,
        "mentionedSender": sender_staff_id,
    }


def _qwenpaw_workspace_dir(account_id: str) -> Path | None:
    working_dir = _qwenpaw_working_dir()
    if working_dir is None:
        return None

    workspace_name = account_id or "default"
    workspace_dir = working_dir / "workspaces" / workspace_name
    if not workspace_dir.exists() and workspace_name != "default":
        default_workspace = working_dir / "workspaces" / "default"
        if default_workspace.exists():
            workspace_dir = default_workspace
    return workspace_dir


def _dingtalk_session_webhook_entry(target_user: str, target_session: str, account_id: str) -> dict[str, Any]:
    workspace_dir = _qwenpaw_workspace_dir(account_id)
    if workspace_dir is None:
        return {}

    store_path = workspace_dir / "dingtalk_session_webhooks.json"
    try:
        store = _read_json(store_path)
    except Exception:
        return {}

    keys = []
    if target_user and target_session:
        keys.append(f"dingtalk:sw:{target_user}_{target_session}")
    if target_session:
        keys.append(f"dingtalk:sw:{target_session}")
        keys.append(target_session)

    for key in keys:
        value = store.get(key)
        if isinstance(value, dict):
            return dict(value)
        if isinstance(value, str):
            return {"webhook": value}
    return {}


def _session_safe(name: str) -> str:
    return UNSAFE_SESSION_FILENAME_RE.sub("--", name)


def _qwenpaw_working_dir() -> Path | None:
    for name in ("QWENPAW_WORKING_DIR", "COPAW_WORKING_DIR"):
        raw = os.getenv(name, "").strip()
        if raw:
            return Path(raw).expanduser()
    for name in ("AGENTTEAMS_AGENT_HOME", "AGENTTEAMS_WORKER_HOME", "HOME"):
        raw = os.getenv(name, "").strip()
        if raw:
            home = Path(raw).expanduser()
            return home / ".qwenpaw"
    return None


def _channel_session_path(channel: str, user_id: str, session_id: str, account_id: str) -> Path | None:
    workspace_dir = _qwenpaw_workspace_dir(account_id)
    if workspace_dir is None:
        return None

    filename = f"{_session_safe(user_id)}_{_session_safe(session_id)}.json" if user_id else f"{_session_safe(session_id)}.json"
    channel_dir = _session_safe(channel.strip().lower() or "default")
    current_path = workspace_dir / "sessions" / channel_dir / filename
    legacy_path = workspace_dir / "sessions" / filename
    if not current_path.exists() and legacy_path.exists():
        current_path.parent.mkdir(parents=True, exist_ok=True)
        current_path.write_bytes(legacy_path.read_bytes())
    return current_path


def _outbound_message_dict(channel: str, text: str, message_id: str | None, account_id: str, metadata: dict[str, Any]) -> dict[str, Any]:
    now = time.strftime("%Y-%m-%d %H:%M:%S", time.localtime())
    millis = int((time.time() % 1) * 1000)
    msg_metadata = {
        "channel": channel,
        "message_id": message_id or "",
        "source": "message_tool_outbound",
    }
    msg_metadata.update(metadata)
    return {
        "id": uuid.uuid4().hex,
        "name": account_id or "default",
        "role": "assistant",
        "content": [{"type": "text", "text": text}],
        "metadata": msg_metadata,
        "timestamp": f"{now}.{millis:03d}",
    }


def _record_outbound_to_session(
    *,
    channel: str,
    user_id: str,
    session_id: str,
    text: str,
    message_id: str | None,
    account_id: str,
    metadata: dict[str, Any] | None = None,
) -> bool:
    channel_key = channel.strip().lower() or "default"
    path = _channel_session_path(channel_key, user_id, session_id, account_id)
    if path is None:
        return False

    lock = SESSION_WRITE_LOCKS.setdefault(str(path), threading.Lock())
    with lock:
        states: dict[str, Any] = {}
        if path.exists():
            try:
                loaded = json.loads(path.read_text(encoding="utf-8"))
            except json.JSONDecodeError:
                return False
            if not isinstance(loaded, dict):
                return False
            states = loaded

        agent_state = states.setdefault("agent", {})
        if not isinstance(agent_state, dict):
            return False
        memory_state = agent_state.setdefault("memory", {})
        if not isinstance(memory_state, dict):
            return False
        content = memory_state.setdefault("content", [])
        if not isinstance(content, list):
            return False

        content.append([
            _outbound_message_dict(
                channel_key,
                text,
                message_id,
                account_id,
                metadata or {
                    "user_id": user_id,
                    "session_id": session_id,
                },
            ),
            [],
        ])
        path.parent.mkdir(parents=True, exist_ok=True)
        tmp = path.with_name(f".{path.name}.tmp")
        tmp.write_text(json.dumps(states, ensure_ascii=False), encoding="utf-8")
        tmp.replace(path)
    return True


def _record_matrix_outbound_to_session(room_id: str, text: str, message_id: str | None, account_id: str) -> bool:
    recorded = _record_outbound_to_session(
        channel="matrix",
        user_id=room_id,
        session_id=f"matrix:{room_id}",
        text=text,
        message_id=message_id,
        account_id=account_id,
        metadata={"room_id": room_id},
    )
    _write_matrix_attachment_context_parent_event_id(room_id, message_id)
    return recorded


def _message(arguments: dict[str, Any]) -> dict[str, Any]:
    return _message_impl(
        arguments,
        MessageToolDeps(
            reply_route=_reply_route,
            qwenpaw_message=_qwenpaw_message,
            matrix_target=_matrix_target,
            mentions=_mentions,
            ping_pong_error=_ping_pong_error,
            matrix_content=_matrix_content,
            record_matrix_outbound_to_session=_record_matrix_outbound_to_session,
        ),
    )


