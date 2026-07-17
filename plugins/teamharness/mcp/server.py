#!/usr/bin/env python3
"""TeamHarness MCP stdio server entry point."""

from __future__ import annotations

import _bootstrap  # noqa: F401 — plugins + agentteams_protocol on sys.path

import html
import hashlib
import datetime
import json
import mimetypes
import os
from pathlib import Path
import re
import subprocess
import sys
import threading
import time
from typing import Any
import urllib.error
import urllib.parse
import urllib.request
import uuid

from message_tool import MessageToolDeps, message as _message_impl
from roomflow_tool import RoomDescribeDeps, describe_room as _describe_room_impl
from common.runtime_config import load_runtime_config, section as _runtime_section, yaml_scalar as _yaml_scalar
from protocol_bridge import validate_task_graph as _protocol_validate_task_graph
import mcp_common as _mcp_common
from tools.matrix_format import md_to_html as _md_to_html
from tools.filesync import filesync as _filesync
from tools.filesync import _normalize_shared_path  # noqa: PLC2701 — server artifact publish helper
from tools.artifact import artifact as _artifact
from tools.projectflow import projectflow as _projectflow
from tools.taskflow import taskflow as _taskflow

_read_json = _mcp_common._read_json
_write_json = _mcp_common._write_json
_workspace_dir = _mcp_common._workspace_dir
_optional_workspace_dir = _mcp_common._optional_workspace_dir
_task_state_path = _mcp_common._task_state_path
_payload = _mcp_common._payload
_canonical_room_id = _mcp_common._canonical_room_id


TOOL_NAMES = ["health", "message", "roomflow", "filesync", "artifact", "projectflow", "taskflow"]
MESSAGE_TOOL_BLOCKED_ROLES = {"worker", "remote-member"}
MATRIX_USER_RE = re.compile(r"@[a-zA-Z0-9._=+/\-]+:[a-zA-Z0-9.\-]+(?::\d+)?")
MENTION_LOCAL_CHARS = r"a-zA-Z0-9._=+/\-"
SHORT_MATRIX_MENTION_RE = re.compile(
    rf"(?<![{MENTION_LOCAL_CHARS}])@([{MENTION_LOCAL_CHARS}]+)(?![{MENTION_LOCAL_CHARS}])(?!:[a-zA-Z0-9.\-])"
)
MATRIX_ROOM_RE = re.compile(r"^![^:\s]+:[^\s]+$")
LOW_INFORMATION_ACKS = {"ack", "acknowledged", "ok", "okay", "done", "received", "收到", "好的", "好"}
MC_ALIAS = "agentteams"
UNSAFE_SESSION_FILENAME_RE = re.compile(r'[\\/:*?"<>|]')
SESSION_WRITE_LOCKS: dict[str, threading.Lock] = {}
SENSITIVE_ARTIFACT_NAME_RE = re.compile(
    r"(secret|token|cookie|authorization|private[_-]?key|credential|client[_-]?secret)",
    re.IGNORECASE,
)
SENSITIVE_ARTIFACT_TEXT_RE = [
    re.compile(r"-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----", re.IGNORECASE),
    re.compile(r"\bAuthorization\s*:\s*(?:Bearer|Basic)\s+\S+", re.IGNORECASE),
    re.compile(
        r"\b(?:access[_-]?key[_-]?secret|client[_-]?secret|secret[_-]?key|api[_-]?key|token)\b"
        r"\s*[:=]\s*['\"]?[A-Za-z0-9_./+=:-]{16,}",
        re.IGNORECASE,
    ),
]
MATRIX_ATTACHMENT_REL_TYPE = "com.agentteams.attachment"
MATRIX_ATTACHMENT_CONTEXT_FILE = "teamharness-matrix-context.json"
MATRIX_ATTACHMENT_CONTEXT_TTL_SECONDS = 30 * 60
ATTACHMENT_PARENT_EVENT_KEYS = (
    "parentEventId",
    "parent_event_id",
    "attachmentParentEventId",
    "attachment_parent_event_id",
    "matrixAttachmentParentEventId",
    "matrix_attachment_parent_event_id",
)
TEXT_ARTIFACT_SAMPLE_BYTES = 256 * 1024

TOOL_SCHEMAS: dict[str, dict[str, Any]] = {
    "health": {
        "description": (
            "Check TeamHarness MCP server availability and basic tool wiring. "
            "This is not runtime worker health, QwenPaw process health, storage "
            "status, or controller readiness."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {},
            "additionalProperties": True,
        },
    },
    "message": {
        "description": (
            "Send a TeamHarness message only when the output must leave the "
            "current runtime conversation: Matrix cross-room sends, external "
            "cross-channel sends, or requester replyRoute/cross-session reports. "
            "For same-agent Project Work handoff from any requester/source "
            "session into a Matrix task room, use the PROJECT_REQUESTED "
            "self-trigger payload with sender.session and target.session; the "
            "tool sends a Matrix event with a TeamHarness trigger marker so "
            "the target task room receives it as the current event. "
            "Do not use this tool for normal replies in the current room/session; "
            "answer directly instead."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "action": {
                    "type": "string",
                    "enum": ["send"],
                    "description": "Only send is supported.",
                },
                "channel": {
                    "type": "string",
                    "description": "Delivery target channel: matrix for Matrix room sends and PROJECT_REQUESTED task-room triggers, or an external channel such as dingtalk for external sends.",
                },
                "sender": {
                    "type": "object",
                    "description": (
                        "Source Matrix account and requester/source session for "
                        "same-agent self-trigger handoff. For PROJECT_REQUESTED, "
                        "sender.agent must be the current runtime Matrix user id "
                        "such as @leader:matrix.local, not a role name like "
                        "leader or a workspace name like default."
                    ),
                    "additionalProperties": True,
                    "properties": {
                        "agent": {
                            "type": "string",
                            "description": (
                                "Current runtime Matrix user id for the sender, "
                                "for example @leader:matrix.local. Must match "
                                "agentId for PROJECT_REQUESTED self-trigger."
                            ),
                        },
                        "session": {
                            "type": "object",
                            "additionalProperties": True,
                            "properties": {
                                "channel": {"type": "string"},
                                "id": {"type": "string"},
                            },
                        },
                    },
                },
                "target": {
                    "type": "string",
                    "description": (
                        "Matrix room target for cross-room sends, for example "
                        "room:!room:domain. For PROJECT_REQUESTED self-trigger, "
                        "pass the target task room as a room:!room:domain string."
                    ),
                },
                "replyRoute": {
                    "type": "object",
                    "description": (
                        "Structured requester route for cross-session reports. "
                        "Required for PROJECT_REQUESTED; do not put the route "
                        "only in message text."
                    ),
                    "additionalProperties": True,
                    "properties": {
                        "channel": {"type": "string"},
                        "target": {"type": "string"},
                        "targetUser": {"type": "string"},
                        "targetSession": {"type": "string"},
                        "mentionSender": {"type": "boolean"},
                    },
                },
                "targetUser": {
                    "type": "string",
                    "description": "External-channel recipient user id; required for non-Matrix sends.",
                },
                "targetSession": {
                    "type": "string",
                    "description": "External-channel session id; required for non-Matrix sends.",
                },
                "agentId": {
                    "type": "string",
                    "description": (
                        "Current runtime Matrix user id, for example "
                        "@leader:matrix.local. For PROJECT_REQUESTED, this "
                        "must match sender.agent; do not pass role names such "
                        "as leader or workspace names such as default."
                    ),
                },
                "type": {
                    "type": "string",
                    "enum": ["PROJECT_REQUESTED"],
                    "description": "Top-level message type alias for trigger messages. PROJECT_REQUESTED is the v1 allowlisted self-trigger type.",
                },
                "message": {
                    "oneOf": [
                        {
                            "type": "object",
                            "required": ["type", "text"],
                            "additionalProperties": True,
                            "properties": {
                                "type": {
                                    "type": "string",
                                    "enum": ["PROJECT_REQUESTED"],
                                    "description": "Trigger type. v1 only allows PROJECT_REQUESTED.",
                                },
                                "text": {
                                    "type": "string",
                                    "description": "Synthetic current-event body to enqueue in the target Matrix task room.",
                                },
                            },
                        },
                        {"type": "string"},
                    ],
                    "description": (
                        "Message body. For PROJECT_REQUESTED handoff, pass a "
                        "JSON object with type and text, not a serialized JSON "
                        "string. Serialized JSON object strings are accepted "
                        "only for compatibility."
                    ),
                },
                "text": {
                    "type": "string",
                    "description": "Plain message text for ordinary cross-room or external sends. message and body aliases are also accepted.",
                },
                "dryRun": {
                    "type": "boolean",
                    "description": "Return the resolved payload without sending.",
                },
                "mentionSender": {
                    "type": "boolean",
                    "description": "For DingTalk requester reports, mention the original sender when session metadata has sender_staff_id.",
                },
            },
            "additionalProperties": True,
        },
    },
    "roomflow": {
        "description": (
            "Manage Matrix task rooms for TeamHarness execution-channel "
            "isolation: create a dedicated room for a project or quick task, "
            "list joined rooms, describe one Matrix room by room name/topic/tags, "
            "or archive a task room. Task rooms are internal Leader/Worker "
            "execution channels, not requester reply channels."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "action": {
                    "type": "string",
                    "enum": ["create_task_room", "list_rooms", "describe_room", "archive_room"],
                    "description": "Room operation to perform.",
                },
                "taskId": {
                    "type": "string",
                    "description": "Safe task id or compatibility project id used when projectId is omitted.",
                },
                "projectId": {
                    "type": "string",
                    "description": "Safe project id. Project task rooms are named TASK：<projectId> and reused only by this id.",
                },
                "name": {
                    "type": "string",
                    "description": "Human-readable title for create_task_room. When projectId is present, Matrix room name uses TASK：<projectId>.",
                },
                "source": {
                    "type": "string",
                    "description": "Optional requester/source label such as matrix, dingtalk, or wechat. Source metadata is kept for context only and does not decide task-room reuse.",
                },
                "sourceRoomId": {
                    "type": "string",
                    "description": "Optional original requester room/conversation id kept as metadata for project/requester routing.",
                },
                "sender": {
                    "type": "string",
                    "description": "Optional original external sender identity kept as metadata. It does not decide task-room reuse.",
                },
                "topic": {
                    "type": "string",
                    "description": "Optional Matrix room topic. Defaults to a task-room topic.",
                },
                "invite": {
                    "type": "array",
                    "items": {"type": "string"},
                    "description": "Matrix user ids to invite. A comma-separated string is also accepted.",
                },
                "admin": {
                    "type": "string",
                    "description": "Optional Team Admin Matrix user id to invite and grant power level 100.",
                },
                "roomId": {
                    "type": "string",
                    "description": "Matrix room id for describe_room or archive_room, with or without room: prefix.",
                },
                "sessionId": {
                    "type": "string",
                    "description": "Matrix session id accepted by describe_room, for example matrix:!room:domain.",
                },
                "payload": {
                    "type": "object",
                    "description": "Room payload; flat arguments are also accepted.",
                    "additionalProperties": True,
                },
                "dryRun": {
                    "type": "boolean",
                    "description": "Return the resolved room operation without calling Matrix.",
                },
            },
            "additionalProperties": True,
        },
    },
    "filesync": {
        "description": (
            "Explicitly list, stat, pull, or push TeamHarness shared artifacts "
            "under shared/ or read-only global-shared. "
            "Use this for deliberate shared file operations, not periodic "
            "workspace sync or runtime package updates."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "action": {
                    "type": "string",
                    "enum": ["list", "stat", "pull", "push"],
                    "description": "Shared artifact operation to perform.",
                },
                "path": {
                    "type": "string",
                    "description": "Relative path beginning with shared/ or global-shared/.",
                },
                "workspaceDir": {
                    "type": "string",
                    "description": "Runtime workspace containing the shared/ tree; usually inferred.",
                },
                "storage": {
                    "type": "object",
                    "description": "Optional storage prefixes such as sharedPrefix or globalSharedPrefix.",
                    "additionalProperties": True,
                },
                "exclude": {
                    "type": "array",
                    "description": "Optional patterns excluded during push.",
                    "items": {"type": "string"},
                },
                "dryRun": {
                    "type": "boolean",
                    "description": "Return the mc command without executing it.",
                },
            },
            "additionalProperties": True,
        },
    },
    "artifact": {
        "description": (
            "Publish a workspace file to a Matrix room as a standard m.file event. "
            "Use this for explicit user-visible room files, not shared storage sync. "
            "The file path must be relative to workspaceDir and must not escape it."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "action": {
                    "type": "string",
                    "enum": ["publish_file"],
                    "description": "Publish one local workspace file to Matrix media and send an m.file event.",
                },
                "path": {
                    "type": "string",
                    "description": "Workspace-relative file path, for example reports/summary.md or shared/tasks/task-001/result.md.",
                },
                "filename": {
                    "type": "string",
                    "description": "Optional display filename for the Matrix room file. Defaults to the source basename.",
                },
                "target": {
                    "type": "string",
                    "description": "Matrix room target, for example room:!room:domain.",
                },
                "roomId": {
                    "type": "string",
                    "description": "Matrix room id target, with or without room: prefix.",
                },
                "parentEventId": {
                    "type": "string",
                    "description": "Optional Matrix text event id that this file should attach to.",
                },
                "attachmentParentEventId": {
                    "type": "string",
                    "description": "Alias for parentEventId.",
                },
                "matrixAttachmentParentEventId": {
                    "type": "string",
                    "description": "Alias for parentEventId.",
                },
                "replyRoute": {
                    "type": "object",
                    "description": "Optional Matrix replyRoute whose targetSession identifies the target room.",
                    "additionalProperties": True,
                },
                "workspaceDir": {
                    "type": "string",
                    "description": "Runtime workspace containing the file.",
                },
            },
            "additionalProperties": True,
        },
    },
    "projectflow": {
        "description": (
            "Manage durable TeamHarness project state only after Quick Task "
            "or Project Work mode is selected: create quick projects, create "
            "projects, plan or update DAG and Loop work, query ready nodes, "
            "and record loop iterations. Do not use for ordinary direct "
            "replies or one-off checks."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "action": {
                    "type": "string",
                    "enum": [
                        "create_project",
                        "create_quick_project",
                        "resolve_project",
                        "plan_dag",
                        "plan_loop",
                        "ready_nodes",
                        "ready_loop_nodes",
                        "record_loop_iteration",
                        "accept_task_result",
                        "mark_requester_report_sent",
                        "pause_project",
                        "resume_project",
                        "complete_project",
                    ],
                    "description": "Project operation to perform.",
                },
                "projectId": {
                    "type": "string",
                    "description": "Safe project id used under shared/projects/{projectId}.",
                },
                "payload": {
                    "type": "object",
                    "description": "Project payload; flat arguments are also accepted.",
                    "additionalProperties": True,
                },
                "tasks": {
                    "type": "array",
                    "description": "DAG or Loop task nodes for planning actions.",
                    "items": {"type": "object", "additionalProperties": True},
                },
                "replyRoute": {
                    "type": "object",
                    "description": "Requester route for accepted outcome reports from external or cross-session requests.",
                    "additionalProperties": True,
                },
                "sourceRoomId": {
                    "type": "string",
                    "description": "Stable external requester room/conversation id to persist with external project and task state.",
                },
                "accepted": {
                    "type": "boolean",
                    "description": "For accept_task_result, false records a revision state instead of accepting the result.",
                },
                "publishArtifacts": {
                    "type": "boolean",
                    "description": "For accept_task_result or complete_project, true explicitly publishes project artifacts immediately. Default is false so callers can publish after the requester report message exists.",
                },
                "workspaceDir": {
                    "type": "string",
                    "description": "Runtime workspace containing shared/projects.",
                },
            },
            "additionalProperties": True,
        },
    },
    "taskflow": {
        "description": (
            "Coordinate bounded TeamHarness tasks after a project node is ready: "
            "leader delegates and checks tasks; worker or remote-member "
            "acknowledges and submits results. Do not use for direct questions, "
            "readiness checks, or ordinary conversation."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "role": {
                    "type": "string",
                    "enum": ["leader", "worker", "remote-member"],
                    "description": "Caller TeamHarness role; inferred from runtime config when omitted.",
                },
                "action": {
                    "type": "string",
                    "enum": ["delegate_task", "ack_task", "submit_task", "check_task", "cancel_task"],
                    "description": "Task lifecycle operation.",
                },
                "projectId": {
                    "type": "string",
                    "description": "Safe project id associated with a delegated task.",
                },
                "taskId": {
                    "type": "string",
                    "description": "Safe task id used under shared/tasks/{taskId}.",
                },
                "payload": {
                    "type": "object",
                    "description": "Task payload; flat arguments are also accepted.",
                    "additionalProperties": True,
                },
                "spec": {
                    "type": "string",
                    "description": "Task execution contract for delegate_task.",
                },
                "summary": {
                    "type": "string",
                    "description": "Worker result summary for submit_task.",
                },
                "deliverables": {
                    "type": "array",
                    "description": "Shared deliverable paths included in submit_task.",
                    "items": {"type": "string"},
                },
                "reason": {
                    "type": "string",
                    "description": "Required cancellation reason for cancel_task.",
                },
                "replacementTaskId": {
                    "type": "string",
                    "description": "Optional replacement task id for cancel_task.",
                },
                "assignedTo": {
                    "type": "string",
                    "description": "Worker Matrix id or stable member name assigned to delegate_task.",
                },
                "workspaceDir": {
                    "type": "string",
                    "description": "Runtime workspace containing shared/tasks.",
                },
            },
            "additionalProperties": True,
        },
    },
}


def _tool_schema(name: str) -> dict[str, Any]:
    schema = TOOL_SCHEMAS[name]
    return {
        "name": name,
        "description": schema["description"],
        "inputSchema": schema["inputSchema"],
    }


def list_tools() -> list[dict[str, Any]]:
    return [_tool_schema(name) for name in _visible_tool_names()]


def call_tool(name: str, arguments: dict[str, Any] | None = None) -> dict[str, Any]:
    args = arguments or {}
    if name not in TOOL_NAMES:
        payload = {"ok": False, "error": "unknown_tool", "tool": name}
    elif name == "message" and _message_tool_blocked_for_runtime_role():
        payload = {
            "ok": False,
            "error": "forbidden_tool",
            "tool": name,
            "message": "message tool is not available to worker roles",
        }
    elif name == "health":
        payload = {"ok": True, "tool": name, "status": "ok"}
    elif name == "message":
        payload = _message(args)
    elif name == "roomflow":
        payload = _roomflow(args)
    elif name == "filesync":
        payload = _filesync(args)
    elif name == "artifact":
        payload = _artifact(args)
    elif name == "projectflow":
        payload = _projectflow(args)
    elif name == "taskflow":
        payload = _taskflow(args)
    else:
        payload = {
            "ok": True,
            "tool": name,
            "implemented": False,
            "reason": "tool behavior is defined by later TeamHarness behavior slices",
            "arguments": args,
        }
    result: dict[str, Any] = {
        "content": [
            {
                "type": "text",
                "text": json.dumps(payload, ensure_ascii=False),
            }
        ]
    }
    if payload.get("ok") is False:
        result["isError"] = True
    return result


def _matrix_target(target: str) -> tuple[str, str]:
    raw = (target or "").strip()
    if raw.startswith("matrix:"):
        raw = raw[len("matrix:") :]
    if raw.startswith("room:"):
        room_id = raw[len("room:") :].strip()
        if MATRIX_ROOM_RE.match(room_id):
            return ("room", room_id)
    if raw.startswith("!") and MATRIX_ROOM_RE.match(raw):
        return ("room", raw)
    if raw.startswith("user:") or raw.startswith("@"):
        return ("user", raw[len("user:") :] if raw.startswith("user:") else raw)
    raise ValueError("target must be a Matrix room target such as room:!room:domain")


def _matrix_room_domain(room_id: str) -> str:
    return room_id.split(":", 1)[1] if ":" in room_id else ""


def _mentions(text: str, room_id: str = "") -> list[str]:
    mentions = list(MATRIX_USER_RE.findall(text or ""))
    domain = _matrix_room_domain(room_id)
    if domain:
        for local in SHORT_MATRIX_MENTION_RE.findall(text or ""):
            mentions.append(f"@{local}:{domain}")
    return list(dict.fromkeys(mentions))


def _compact_without_mentions(text: str, mentions: list[str]) -> str:
    without_mentions = MATRIX_USER_RE.sub("", text or "")
    for mxid in mentions:
        local = mxid.split(":", 1)[0]
        without_mentions = re.sub(
            rf"(?<![{MENTION_LOCAL_CHARS}]){re.escape(local)}(?![{MENTION_LOCAL_CHARS}])(?!:[a-zA-Z0-9.\-])",
            "",
            without_mentions,
        )
    return "".join(re.findall(r"[0-9A-Za-z\u4e00-\u9fff]+", without_mentions)).lower()


def _ping_pong_error(text: str, mentions: list[str]) -> str | None:
    if not mentions:
        return None
    compact = _compact_without_mentions(text, mentions)
    if not compact or compact in LOW_INFORMATION_ACKS:
        return "message blocked: low-information mention acknowledgements can create ping-pong loops"
    return None


def _formatted_body(text: str, mentions: list[str]) -> str:
    body = _md_to_html(text or "")
    for mxid in mentions:
        encoded = urllib.parse.quote(mxid, safe="")
        local = mxid.split(":", 1)[0]
        display = html.escape(local.lstrip("@") or mxid)
        anchor = f'<a href="https://matrix.to/#/{encoded}">{display}</a>'
        if mxid in body:
            body = body.replace(mxid, anchor, 1)
        else:
            escaped = html.escape(mxid)
            if escaped in body:
                body = body.replace(escaped, anchor, 1)
            else:
                body = re.sub(
                    rf"(?<![{MENTION_LOCAL_CHARS}]){re.escape(html.escape(local))}(?![{MENTION_LOCAL_CHARS}])(?!:[a-zA-Z0-9.\-])",
                    anchor,
                    body,
                    count=1,
                )
    return body


def _matrix_content(text: str, mentions: list[str]) -> dict[str, Any]:
    content: dict[str, Any] = {
        "msgtype": "m.text",
        "body": text,
        "format": "org.matrix.custom.html",
        "formatted_body": _formatted_body(text, mentions),
    }
    if mentions:
        content["m.mentions"] = {"user_ids": mentions}
    return content


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


def _matrix_env(tool: str) -> tuple[str, str]:
    homeserver = os.getenv("AGENTTEAMS_MATRIX_URL", "").rstrip("/")
    token = os.getenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "")
    if not homeserver or not token:
        raise ValueError("AGENTTEAMS_MATRIX_URL and AGENTTEAMS_WORKER_MATRIX_TOKEN are required")
    return homeserver, token


def _attachment_parent_event_id(*sources: dict[str, Any]) -> str:
    for source in sources:
        if not isinstance(source, dict):
            continue
        for key in ATTACHMENT_PARENT_EVENT_KEYS:
            value = str(source.get(key) or "").strip()
            if value:
                return value
    return ""


def _matrix_attachment_context_path() -> Path | None:
    raw = os.getenv("TEAMHARNESS_MATRIX_CONTEXT_FILE", "").strip()
    if raw:
        return Path(raw)
    qwenpaw_dir = os.getenv("QWENPAW_WORKING_DIR", "").strip()
    if qwenpaw_dir:
        return Path(qwenpaw_dir) / MATRIX_ATTACHMENT_CONTEXT_FILE
    return None


def _matrix_attachment_context_parent_event_id(room_id: str) -> str:
    path = _matrix_attachment_context_path()
    if not path or not path.is_file():
        return ""
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return ""
    if not isinstance(data, dict):
        return ""
    rooms = data.get("rooms")
    if not isinstance(rooms, dict):
        return ""
    record = rooms.get(_canonical_room_id(room_id)) or rooms.get(room_id)
    if not isinstance(record, dict):
        return ""
    try:
        updated_at = float(record.get("updatedAt") or record.get("updated_at") or 0)
    except (TypeError, ValueError):
        updated_at = 0
    if updated_at and time.time() - updated_at > MATRIX_ATTACHMENT_CONTEXT_TTL_SECONDS:
        return ""
    for key in ("attachmentParentEventId", "parentEventId", "eventId", "event_id"):
        value = str(record.get(key) or "").strip()
        if value:
            return value
    return ""


def _write_matrix_attachment_context_parent_event_id(room_id: str, event_id: str | None) -> bool:
    parent_event_id = str(event_id or "").strip()
    if not parent_event_id:
        return False
    path = _matrix_attachment_context_path()
    if not path:
        return False
    data: dict[str, Any] = {}
    try:
        if path.is_file():
            loaded = json.loads(path.read_text(encoding="utf-8"))
            if isinstance(loaded, dict):
                data = loaded
    except (OSError, json.JSONDecodeError):
        data = {}
    rooms = data.get("rooms")
    if not isinstance(rooms, dict):
        rooms = {}
        data["rooms"] = rooms
    rooms[_canonical_room_id(room_id)] = {
        "attachmentParentEventId": parent_event_id,
        "eventId": parent_event_id,
        "updatedAt": time.time(),
    }
    try:
        path.parent.mkdir(parents=True, exist_ok=True)
        tmp = path.with_name(f"{path.name}.{os.getpid()}.tmp")
        tmp.write_text(json.dumps(data, ensure_ascii=False, sort_keys=True), encoding="utf-8")
        tmp.replace(path)
    except OSError:
        return False
    return True


def _artifact_publish_result(source_path: str, filename: str = "", parent_event_id: str = "") -> dict[str, Any]:
    return {
        "sourcePath": source_path,
        "filename": filename,
        "size": None,
        "mimetype": "",
        "mxcUri": "",
        "eventId": "",
        "parentEventId": parent_event_id,
        "status": "skipped",
        "error": "",
    }


def _path_is_under(normalized: str, prefix: str) -> bool:
    return normalized == prefix or normalized.startswith(f"{prefix}/")


def _shared_dir_candidates() -> list[Path]:
    candidates: list[Path] = []
    for env_key in ("TEAMHARNESS_SHARED_DIR", "AGENTTEAMS_SHARED_DIR"):
        raw = os.getenv(env_key, "").strip()
        if raw:
            candidates.append(Path(raw).expanduser())
    return candidates


def _artifact_is_under_runtime_shared(workspace: Path, local_path: Path, normalized: str) -> bool:
    if not _path_is_under(normalized, "shared"):
        return False
    candidates = _shared_dir_candidates()
    if not candidates:
        return False
    try:
        workspace_shared = (workspace / "shared").resolve()
        local_resolved = local_path.resolve()
    except (OSError, RuntimeError):
        return False
    for shared_dir in candidates:
        try:
            shared_resolved = shared_dir.resolve()
            local_resolved.relative_to(shared_resolved)
        except (OSError, RuntimeError, ValueError):
            continue
        if workspace_shared == shared_resolved:
            return True
    return False


def _normalize_workspace_artifact_path(raw_path: str) -> tuple[str, bool]:
    raw = (raw_path or "").strip()
    if not raw or raw.startswith("/") or "\\" in raw:
        raise ValueError("artifact path must be a relative workspace path")
    parts = raw.strip("/").split("/")
    if any(part in {"", ".", ".."} for part in parts):
        raise ValueError("artifact path must be a relative workspace path without '.', '..', or empty segments")
    is_directory = raw.endswith("/")
    normalized = "/".join(parts)
    if is_directory:
        normalized += "/"
    return normalized, is_directory


def _resolve_workspace_artifact_path(arguments: dict[str, Any], source_path: str, expected_prefix: str) -> tuple[str, Path]:
    normalized, is_directory = _normalize_workspace_artifact_path(source_path)
    if is_directory:
        raise ValueError("artifact path must be a file")
    if expected_prefix and not _path_is_under(normalized, expected_prefix):
        raise ValueError(f"artifact path must be under {expected_prefix}/")
    workspace = _workspace_dir(arguments)
    parts = normalized.split("/")
    local_path = workspace / Path(*parts)
    try:
        local_path.resolve().relative_to(workspace.resolve())
    except ValueError as exc:
        if _artifact_is_under_runtime_shared(workspace, local_path, normalized):
            return normalized, local_path
        raise ValueError("artifact path must stay under workspace shared/") from exc
    return normalized, local_path


def _artifact_mimetype(path: Path) -> str:
    return mimetypes.guess_type(str(path))[0] or "application/octet-stream"


def _artifact_is_text(path: Path, mimetype: str) -> bool:
    if mimetype.startswith("text/"):
        return True
    if mimetype in {
        "application/json",
        "application/javascript",
        "application/xml",
        "application/x-yaml",
        "application/yaml",
        "application/toml",
    }:
        return True
    return path.suffix.lower() in {
        ".cfg",
        ".conf",
        ".css",
        ".csv",
        ".html",
        ".ini",
        ".js",
        ".json",
        ".jsx",
        ".log",
        ".md",
        ".py",
        ".rb",
        ".sh",
        ".sql",
        ".toml",
        ".ts",
        ".tsx",
        ".txt",
        ".xml",
        ".yaml",
        ".yml",
    }


def _artifact_text_has_sensitive_content(path: Path, mimetype: str) -> bool:
    if not _artifact_is_text(path, mimetype):
        return False
    sample = path.read_bytes()[:TEXT_ARTIFACT_SAMPLE_BYTES]
    if b"\x00" in sample:
        return False
    text = sample.decode("utf-8", errors="replace")
    return any(pattern.search(text) for pattern in SENSITIVE_ARTIFACT_TEXT_RE)


def _matrix_upload_artifact(homeserver: str, token: str, path: Path, filename: str, mimetype: str) -> str:
    url = f"{homeserver}/_matrix/media/v3/upload?filename={urllib.parse.quote(filename)}"
    request = urllib.request.Request(
        url,
        data=path.read_bytes(),
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": mimetype,
        },
        method="POST",
    )
    with urllib.request.urlopen(request, timeout=30) as response:
        data = json.loads(response.read().decode("utf-8") or "{}")
    mxc_uri = str(data.get("content_uri") or "").strip()
    if not mxc_uri:
        raise ValueError("Matrix media upload response missing content_uri")
    return mxc_uri


def _matrix_send_file_event(
    homeserver: str,
    token: str,
    room_id: str,
    filename: str,
    mxc_uri: str,
    size: int,
    mimetype: str,
    parent_event_id: str = "",
) -> str:
    content = {
        "msgtype": "m.file",
        "body": filename,
        "url": mxc_uri,
        "info": {
            "size": size,
            "mimetype": mimetype,
        },
    }
    if parent_event_id:
        content["m.relates_to"] = {
            "rel_type": MATRIX_ATTACHMENT_REL_TYPE,
            "event_id": parent_event_id,
        }
    encoded_room_id = urllib.parse.quote(_canonical_room_id(room_id), safe="")
    txn_id = f"teamharness-file-{os.getpid()}-{int(time.time() * 1000)}-{uuid.uuid4().hex}"
    url = f"{homeserver}/_matrix/client/v3/rooms/{encoded_room_id}/send/m.room.message/{txn_id}"
    request = urllib.request.Request(
        url,
        data=json.dumps(content).encode("utf-8"),
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
        },
        method="PUT",
    )
    with urllib.request.urlopen(request, timeout=30) as response:
        data = json.loads(response.read().decode("utf-8") or "{}")
    event_id = str(data.get("event_id") or "").strip()
    if not event_id:
        raise ValueError("Matrix file event response missing event_id")
    return event_id


def _publish_workspace_artifact(
    arguments: dict[str, Any],
    *,
    room_id: str,
    source_path: str,
    filename: str,
    expected_prefix: str,
    parent_event_id: str = "",
) -> dict[str, Any]:
    parent_event_id = str(parent_event_id or "").strip()
    result = _artifact_publish_result(str(source_path), filename, parent_event_id)
    try:
        normalized, path = _resolve_workspace_artifact_path(arguments, str(source_path), expected_prefix)
        result["sourcePath"] = normalized
    except ValueError as exc:
        result["status"] = "failed"
        result["error"] = str(exc)
        return result

    if not path.exists():
        result["status"] = "failed"
        result["error"] = "artifact file not found"
        return result
    if path.is_dir():
        result["status"] = "failed"
        result["error"] = "artifact path must be a file"
        return result

    if SENSITIVE_ARTIFACT_NAME_RE.search(result["sourcePath"]) or SENSITIVE_ARTIFACT_NAME_RE.search(filename):
        result["status"] = "failed"
        result["error"] = "artifact appears sensitive and was not published"
        return result

    size = path.stat().st_size
    mimetype = _artifact_mimetype(path)
    result["size"] = size
    result["mimetype"] = mimetype

    try:
        if _artifact_text_has_sensitive_content(path, mimetype):
            result["status"] = "failed"
            result["error"] = "artifact appears sensitive and was not published"
            return result
    except OSError:
        result["status"] = "failed"
        result["error"] = "artifact file could not be read"
        return result

    canonical_room_id = _canonical_room_id(room_id)
    if not canonical_room_id:
        result["status"] = "skipped"
        result["error"] = "Matrix room is unavailable"
        return result
    if not parent_event_id:
        parent_event_id = _matrix_attachment_context_parent_event_id(canonical_room_id)
        result["parentEventId"] = parent_event_id

    try:
        homeserver, token = _matrix_env("artifact publish")
    except ValueError as exc:
        result["status"] = "skipped"
        result["error"] = str(exc)
        return result

    try:
        mxc_uri = _matrix_upload_artifact(homeserver, token, path, filename, mimetype)
        event_id = _matrix_send_file_event(
            homeserver,
            token,
            canonical_room_id,
            filename,
            mxc_uri,
            size,
            mimetype,
            parent_event_id,
        )
    except urllib.error.HTTPError as exc:
        result["status"] = "failed"
        result["error"] = f"Matrix artifact publish failed: HTTP {exc.code}"
        return result
    except (urllib.error.URLError, TimeoutError, OSError, ValueError) as exc:
        result["status"] = "failed"
        result["error"] = f"Matrix artifact publish failed: {exc}"
        return result

    result["mxcUri"] = mxc_uri
    result["eventId"] = event_id
    result["status"] = "published"
    return result


def _artifact_room_id(arguments: dict[str, Any]) -> str:
    route = _reply_route(arguments)
    target = (
        arguments.get("target")
        or arguments.get("roomId")
        or arguments.get("room_id")
        or route.get("target")
        or route.get("roomId")
        or route.get("room_id")
        or route.get("targetRoom")
        or route.get("target_room")
        or route.get("targetSession")
        or route.get("target_session")
        or ""
    )
    target_kind, target_id = _matrix_target(str(target))
    if target_kind != "room":
        raise ValueError("artifact target must be a Matrix room")
    return target_id


def _artifact(arguments: dict[str, Any]) -> dict[str, Any]:
    action = str(arguments.get("action") or "publish_file").strip()
    if action != "publish_file":
        return {"ok": False, "tool": "artifact", "action": action, "error": f"unsupported action: {action}"}
    source_path = str(
        arguments.get("path")
        or arguments.get("sourcePath")
        or arguments.get("source_path")
        or arguments.get("file")
        or ""
    ).strip()
    if not source_path:
        return {"ok": False, "tool": "artifact", "action": action, "error": "path is required"}
    filename = str(arguments.get("filename") or arguments.get("displayName") or arguments.get("display_name") or "").strip()
    if not filename:
        filename = Path(source_path.rstrip("/")).name or "artifact"
    try:
        room_id = _artifact_room_id(arguments)
    except ValueError as exc:
        return {"ok": False, "tool": "artifact", "action": action, "error": str(exc)}
    parent_event_id = (
        _attachment_parent_event_id(arguments)
        or _matrix_attachment_context_parent_event_id(room_id)
    )
    artifact = _publish_workspace_artifact(
        arguments,
        room_id=room_id,
        source_path=source_path,
        filename=filename,
        expected_prefix="",
        parent_event_id=parent_event_id,
    )
    return {
        "ok": artifact.get("status") == "published",
        "tool": "artifact",
        "action": action,
        "artifact": artifact,
        "error": artifact.get("error") or "",
    }


def _task_artifact_filename(task_id: str, source_path: str, result_artifact: bool = False) -> str:
    if result_artifact:
        suffix = Path(source_path).suffix or ".md"
        return f"{task_id}-result{suffix}"
    name = Path(str(source_path).rstrip("/")).name or "artifact"
    return f"{task_id}-{name}"


def _publish_task_artifacts(
    arguments: dict[str, Any],
    task: dict[str, Any],
    task_id: str,
    deliverables: list[Any],
    parent_event_id: str = "",
) -> list[dict[str, Any]]:
    room_id = str(task.get("room_id") or "")
    expected_prefix = f"shared/tasks/{task_id}"
    result_source = f"{expected_prefix}/result.md"
    artifacts: list[tuple[str, str]] = []
    try:
        _normalized_result, result_path = _resolve_workspace_artifact_path(arguments, result_source, expected_prefix)
        if result_path.is_file():
            artifacts.append((result_source, _task_artifact_filename(task_id, "result.md", result_artifact=True)))
    except ValueError:
        pass
    seen = {source for source, _filename in artifacts}
    for item in deliverables:
        source = str(item or "").strip()
        if not source:
            continue
        try:
            normalized, is_directory = _normalize_shared_path(source, "stat")
            seen_key = normalized.rstrip("/") + ("/" if is_directory else "")
        except ValueError:
            seen_key = source
        if seen_key in seen:
            continue
        seen.add(seen_key)
        artifacts.append((source, _task_artifact_filename(task_id, source)))
    return [
        _publish_workspace_artifact(
            arguments,
            room_id=room_id,
            source_path=source,
            filename=filename,
            expected_prefix=expected_prefix,
            parent_event_id=parent_event_id,
        )
        for source, filename in artifacts
    ]


def _project_artifact_room(project: dict[str, Any], task: dict[str, Any]) -> str:
    reply_route = project.get("reply_route") if isinstance(project.get("reply_route"), dict) else {}
    if str(reply_route.get("channel") or "").strip().lower() == "matrix":
        target_session = str(reply_route.get("target_session") or "").strip()
        if target_session:
            return target_session
    room_id = str(task.get("room_id") or "").strip()
    if room_id:
        return room_id
    source_room_id = str(project.get("source_room_id") or "").strip()
    if MATRIX_ROOM_RE.fullmatch(_canonical_room_id(source_room_id)):
        return source_room_id
    return ""


def _publish_project_artifacts(
    arguments: dict[str, Any],
    project: dict[str, Any],
    project_id: str,
    task_id: str,
    parent_event_id: str = "",
) -> list[dict[str, Any]]:
    task = _read_json(_task_state_path(arguments, task_id)) if task_id else {}
    source_path = f"shared/projects/{project_id}/result.md"
    return [
        _publish_workspace_artifact(
            arguments,
            room_id=_project_artifact_room(project, task),
            source_path=source_path,
            filename=f"{project_id}-project-result.md",
            expected_prefix=f"shared/projects/{project_id}",
            parent_event_id=parent_event_id,
        )
    ]


def _matrix_user_id() -> str:
    explicit = os.getenv("AGENTTEAMS_MATRIX_USER_ID", "").strip()
    if explicit:
        return explicit
    member = _section(_load_runtime_config(), "member")
    matrix_user_id = str(member.get("matrixUserId") or member.get("matrix_user_id") or "").strip()
    if matrix_user_id:
        return matrix_user_id
    name = os.getenv("AGENTTEAMS_WORKER_NAME", "").strip()
    domain = os.getenv("AGENTTEAMS_MATRIX_DOMAIN", "").strip()
    if name and domain:
        return f"@{name}:{domain}"
    return ""


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


def _roomflow_room_meta() -> dict[str, Any]:
    config = _load_runtime_config()
    team = _section(config, "team")
    meta: dict[str, Any] = {
        "schemaVersion": 1,
        "roomKind": "task_room",
        "lifecycle": "ephemeral",
        "createdBy": "teamharness",
    }
    team_name = str(team.get("name") or "").strip()
    if team_name:
        meta["teamName"] = team_name

    admin = _section(team, "admin")
    admin_user_id = str(admin.get("matrixUserId") or admin.get("matrix_user_id") or "").strip()
    if admin_user_id:
        admin_meta: dict[str, Any] = {"userId": admin_user_id}
        admin_name = str(admin.get("name") or admin.get("runtimeName") or admin.get("runtime_name") or "").strip()
        if admin_name:
            admin_meta["name"] = admin_name
        meta["teamAdmin"] = admin_meta

    members = team.get("members")
    if isinstance(members, list):
        worker_members: list[dict[str, Any]] = []
        human_members: list[dict[str, Any]] = []
        for member in members:
            if not isinstance(member, dict):
                continue
            user_id = str(member.get("matrixUserId") or member.get("matrix_user_id") or "").strip()
            if not user_id:
                continue
            role = str(member.get("role") or "").strip().lower().replace("_", "-")
            display_name = str(member.get("name") or member.get("runtimeName") or member.get("runtime_name") or "").strip()
            if role in {"team-leader", "teamleader", "leader"}:
                if "leaderWorker" not in meta:
                    leader_meta: dict[str, Any] = {"userId": user_id}
                    if display_name:
                        leader_meta["workerName"] = display_name
                    meta["leaderWorker"] = leader_meta
                continue
            if role == "worker":
                worker_meta: dict[str, Any] = {"userId": user_id}
                if display_name:
                    worker_meta["workerName"] = display_name
                worker_members.append(worker_meta)
                continue
            human_meta: dict[str, Any] = {"userId": user_id}
            if display_name:
                human_meta["name"] = display_name
            human_members.append(human_meta)
        if worker_members:
            meta["workerMembers"] = worker_members
        if human_members:
            meta["humanMembers"] = human_members
    return meta


def _write_matrix_room_meta(room_id: str, content: dict[str, Any]) -> None:
    homeserver, token = _matrix_env("roomflow")
    encoded_room = urllib.parse.quote(room_id, safe="")
    request = urllib.request.Request(
        f"{homeserver}/_matrix/client/v3/rooms/{encoded_room}/state/room.meta/",
        data=json.dumps(content).encode("utf-8"),
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
        },
        method="PUT",
    )
    with urllib.request.urlopen(request, timeout=10) as response:
        response.read()


def _roomflow(arguments: dict[str, Any]) -> dict[str, Any]:
    action = str(arguments.get("action") or "create_task_room")
    payload = _payload(arguments)
    if action == "create_task_room":
        return _create_task_room(arguments, payload)
    if action == "list_rooms":
        return _list_rooms(arguments)
    if action == "describe_room":
        return _describe_room_impl(
            arguments,
            payload,
            RoomDescribeDeps(
                matrix_env=_matrix_env,
                matrix_user_id=_matrix_user_id,
                canonical_room_id=_canonical_room_id,
            ),
        )
    if action == "archive_room":
        return _archive_room(arguments, payload)
    return {"ok": False, "tool": "roomflow", "action": action, "error": f"unsupported action: {action}"}


def _task_room_name(value: Any) -> str:
    name = str(value or "").strip()
    lowered = name.lower()
    for prefix in ("task:", "task\uff1a"):
        if lowered.startswith(prefix):
            name = name[len(prefix) :].strip()
            break
    return f"TASK\uff1a{name}" if name else ""


def _create_task_room(arguments: dict[str, Any], payload: dict[str, Any]) -> dict[str, Any]:
    try:
        raw_project_id = payload.get("projectId") or payload.get("project_id") or payload.get("taskId")
        project_id = _safe_id(raw_project_id, "projectId")
    except ValueError as exc:
        return {"ok": False, "tool": "roomflow", "action": "create_task_room", "error": str(exc)}
    task_id = project_id
    name = _task_room_name(project_id)
    if not name:
        return {"ok": False, "tool": "roomflow", "action": "create_task_room", "error": "projectId is required"}
    source = str(payload.get("source") or "").strip()
    topic = str(payload.get("topic") or "").strip()
    if not topic:
        suffix = f" [source: {source}]" if source else ""
        topic = f"Task room for {task_id}{suffix}"
    invite = _string_list(payload.get("invite") if "invite" in payload else arguments.get("invite"))
    admin = str(payload.get("admin") or payload.get("adminUser") or payload.get("admin_user") or _runtime_team_admin_user_id()).strip()
    if admin and admin not in invite:
        invite.append(admin)

    creator = _matrix_user_id()
    power_users: dict[str, int] = {}
    if creator:
        power_users[creator] = 100
    if admin:
        power_users[admin] = 100

    body: dict[str, Any] = {
        "name": name,
        "topic": topic,
        "invite": invite,
        "preset": "trusted_private_chat",
    }
    if power_users:
        body["power_level_content_override"] = {"users": power_users}
    binding = _roomflow_project_room_binding(arguments, payload, project_id)
    if binding.get("error"):
        return {"ok": False, "tool": "roomflow", "action": "create_task_room", "error": binding["error"]}
    room_meta = _roomflow_room_meta()

    base: dict[str, Any] = {
        "ok": True,
        "tool": "roomflow",
        "action": "create_task_room",
        "projectId": project_id,
        "taskId": task_id,
        "name": name,
        "source": source,
        "topic": topic,
        "invite": invite,
        "content": body,
    }
    if binding.get("projectRoomKey"):
        base["projectRoomKey"] = binding["projectRoomKey"]
    if binding.get("sourceRoomId"):
        base["sourceRoomId"] = binding["sourceRoomId"]
    if binding.get("sender"):
        base["sender"] = binding["sender"]
    existing_room_id = _bound_room_id(binding)
    if existing_room_id:
        base["roomId"] = existing_room_id
        base["target"] = f"room:{existing_room_id}"
        base["reused"] = True
        if arguments.get("dryRun"):
            base["dryRun"] = True
            return base
        try:
            _ensure_matrix_room_members(existing_room_id, invite)
            _write_matrix_room_meta(existing_room_id, room_meta)
        except ValueError as exc:
            return {"ok": False, "tool": "roomflow", "action": "create_task_room", "error": str(exc)}
        except urllib.error.HTTPError as exc:
            error = exc.read().decode("utf-8", errors="replace")[:200]
            return {"ok": False, "tool": "roomflow", "action": "create_task_room", "error": f"Matrix API error: HTTP {exc.code}: {error}"}
        except (urllib.error.URLError, TimeoutError, OSError) as exc:
            return {"ok": False, "tool": "roomflow", "action": "create_task_room", "error": f"Matrix API error: {exc}"}
        return base

    if arguments.get("dryRun"):
        base["dryRun"] = True
        return base

    try:
        homeserver, token = _matrix_env("roomflow")
    except ValueError as exc:
        return {"ok": False, "tool": "roomflow", "action": "create_task_room", "error": str(exc)}
    request = urllib.request.Request(
        f"{homeserver}/_matrix/client/v3/createRoom",
        data=json.dumps(body).encode("utf-8"),
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
        },
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            data = json.loads(response.read().decode("utf-8") or "{}")
    except urllib.error.HTTPError as exc:
        error = exc.read().decode("utf-8", errors="replace")[:200]
        return {"ok": False, "tool": "roomflow", "action": "create_task_room", "error": f"Matrix API error: HTTP {exc.code}: {error}"}
    except (urllib.error.URLError, TimeoutError, OSError) as exc:
        return {"ok": False, "tool": "roomflow", "action": "create_task_room", "error": f"Matrix API error: {exc}"}
    room_id = str(data.get("room_id") or "")
    if not room_id:
        return {"ok": False, "tool": "roomflow", "action": "create_task_room", "error": "Matrix createRoom response missing room_id", "response": data}
    base["roomId"] = room_id
    base["target"] = f"room:{room_id}"
    try:
        _write_matrix_room_meta(room_id, room_meta)
    except urllib.error.HTTPError as exc:
        error = exc.read().decode("utf-8", errors="replace")[:200]
        return {"ok": False, "tool": "roomflow", "action": "create_task_room", "error": f"Matrix API error: HTTP {exc.code}: {error}"}
    except (urllib.error.URLError, TimeoutError, OSError) as exc:
        return {"ok": False, "tool": "roomflow", "action": "create_task_room", "error": f"Matrix API error: {exc}"}
    _write_roomflow_project_room_binding(binding, room_id, base)
    return base


def _roomflow_project_room_binding(arguments: dict[str, Any], payload: dict[str, Any], project_id: str) -> dict[str, Any]:
    source, source_room_id = _external_source_room_ref(payload)
    sender = _external_sender_ref(payload)
    project_room_key = f"project:{project_id}"
    binding: dict[str, Any] = {
        "projectId": project_id,
        "source": source,
        "sourceRoomId": source_room_id,
        "projectRoomKey": project_room_key,
    }
    if sender:
        binding["sender"] = sender
    workspace_dir = _optional_workspace_dir(arguments)
    if not workspace_dir:
        return binding
    digest = hashlib.sha256(project_room_key.encode("utf-8")).hexdigest()[:16]
    path = workspace_dir / "shared" / "roomflow" / "project-rooms" / f"{project_id}-{digest}.json"
    record = _read_json(path)
    binding["path"] = path
    binding["record"] = record
    return binding


def _external_source_room_ref(payload: dict[str, Any]) -> tuple[str, str]:
    source = str(payload.get("source") or "").strip().lower()
    if not source or source == "matrix":
        return "", ""
    return source, str(payload.get("sourceRoomId") or payload.get("source_room_id") or "").strip()


def _external_sender_ref(payload: dict[str, Any]) -> str:
    for key in (
        "sender",
        "senderId",
        "sender_id",
        "senderUserId",
        "sender_user_id",
        "sourceUserId",
        "source_user_id",
        "targetUser",
        "target_user",
    ):
        value = str(payload.get(key) or "").strip()
        if value:
            return value
    route = payload.get("replyRoute") or payload.get("reply_route")
    if isinstance(route, dict):
        for key in ("targetUser", "target_user", "sender", "senderId", "sender_id"):
            value = str(route.get(key) or "").strip()
            if value:
                return value
    requester_route = _reply_route_from_requester(payload.get("requester"))
    return str(requester_route.get("target_user") or "").strip()


def _bound_room_id(binding: dict[str, Any]) -> str:
    record = binding.get("record")
    if not isinstance(record, dict):
        return ""
    return str(record.get("roomId") or record.get("room_id") or "").strip()


def _write_roomflow_project_room_binding(binding: dict[str, Any], room_id: str, base: dict[str, Any]) -> None:
    path = binding.get("path")
    if not isinstance(path, Path):
        return
    record = dict(binding.get("record") if isinstance(binding.get("record"), dict) else {})
    record.update(
        {
            "projectId": binding.get("projectId"),
            "source": binding.get("source"),
            "sourceRoomId": binding.get("sourceRoomId"),
            "projectRoomKey": binding.get("projectRoomKey"),
            "roomId": room_id,
            "target": f"room:{room_id}",
            "updatedAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "taskId": base.get("taskId"),
            "name": base.get("name"),
        }
    )
    if binding.get("sender"):
        record["sender"] = binding.get("sender")
    if "createdAt" not in record:
        record["createdAt"] = record["updatedAt"]
    _write_json(path, record)


def _ensure_matrix_room_members(room_id: str, invite: list[str]) -> None:
    current = set(_matrix_room_member_user_ids(room_id))
    for user_id in invite:
        if user_id and user_id not in current:
            _matrix_invite_to_room(room_id, user_id)


def _matrix_invite_to_room(room_id: str, user_id: str) -> None:
    homeserver, token = _matrix_env("roomflow")
    encoded_room = urllib.parse.quote(room_id, safe="")
    request = urllib.request.Request(
        f"{homeserver}/_matrix/client/v3/rooms/{encoded_room}/invite",
        data=json.dumps({"user_id": user_id}).encode("utf-8"),
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
        },
        method="POST",
    )
    with urllib.request.urlopen(request, timeout=10) as response:
        response.read()


def _list_rooms(arguments: dict[str, Any]) -> dict[str, Any]:
    if arguments.get("dryRun"):
        return {"ok": True, "tool": "roomflow", "action": "list_rooms", "dryRun": True}
    try:
        homeserver, token = _matrix_env("roomflow")
    except ValueError as exc:
        return {"ok": False, "tool": "roomflow", "action": "list_rooms", "error": str(exc)}
    request = urllib.request.Request(
        f"{homeserver}/_matrix/client/v3/joined_rooms",
        headers={"Authorization": f"Bearer {token}"},
        method="GET",
    )
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            data = json.loads(response.read().decode("utf-8") or "{}")
    except urllib.error.HTTPError as exc:
        error = exc.read().decode("utf-8", errors="replace")[:200]
        return {"ok": False, "tool": "roomflow", "action": "list_rooms", "error": f"Matrix API error: HTTP {exc.code}: {error}"}
    except (urllib.error.URLError, TimeoutError, OSError) as exc:
        return {"ok": False, "tool": "roomflow", "action": "list_rooms", "error": f"Matrix API error: {exc}"}
    rooms = data.get("joined_rooms") if isinstance(data.get("joined_rooms"), list) else []
    return {"ok": True, "tool": "roomflow", "action": "list_rooms", "rooms": rooms, "count": len(rooms)}


def _archive_room(arguments: dict[str, Any], payload: dict[str, Any]) -> dict[str, Any]:
    target = str(payload.get("roomId") or payload.get("room_id") or arguments.get("target") or "").strip()
    try:
        target_kind, room_id = _matrix_target(target)
    except ValueError as exc:
        return {"ok": False, "tool": "roomflow", "action": "archive_room", "error": str(exc)}
    if target_kind != "room":
        return {"ok": False, "tool": "roomflow", "action": "archive_room", "error": "archive_room requires a Matrix room target"}
    base = {"ok": True, "tool": "roomflow", "action": "archive_room", "roomId": room_id, "target": f"room:{room_id}"}
    if arguments.get("dryRun"):
        base["dryRun"] = True
        return base
    try:
        homeserver, token = _matrix_env("roomflow")
    except ValueError as exc:
        return {"ok": False, "tool": "roomflow", "action": "archive_room", "error": str(exc)}
    encoded_room = urllib.parse.quote(room_id, safe="")
    request = urllib.request.Request(
        f"{homeserver}/_matrix/client/v3/rooms/{encoded_room}/leave",
        data=b"{}",
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
        },
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=10):
            pass
    except urllib.error.HTTPError as exc:
        error = exc.read().decode("utf-8", errors="replace")[:200]
        if "M_NOT_FOUND" in error:
            base["note"] = "already left"
            return base
        return {"ok": False, "tool": "roomflow", "action": "archive_room", "error": f"Matrix API error: HTTP {exc.code}: {error}"}
    except (urllib.error.URLError, TimeoutError, OSError) as exc:
        return {"ok": False, "tool": "roomflow", "action": "archive_room", "error": f"Matrix API error: {exc}"}
    base["archived"] = True
    return base








def _runtime_team_admin_user_id() -> str:
    config = _load_runtime_config()
    team = _section(config, "team")
    admin = _section(team, "admin")
    matrix_user_id = str(admin.get("matrixUserId") or admin.get("matrix_user_id") or "").strip()
    if matrix_user_id:
        return matrix_user_id
    return _runtime_leader_dm_admin_user_id(config)


def _runtime_leader_dm_admin_user_id(config: dict[str, Any]) -> str:
    team = _section(config, "team")
    room_id = str(team.get("leaderDmRoomId") or team.get("leader_dm_room_id") or "").strip()
    if not room_id:
        return ""
    leader_id = str(_section(config, "member").get("matrixUserId") or _matrix_user_id()).strip()
    try:
        members = _matrix_room_member_user_ids(room_id)
    except (ValueError, urllib.error.HTTPError, urllib.error.URLError, TimeoutError, OSError, json.JSONDecodeError):
        return ""
    for user_id in members:
        if user_id and user_id != leader_id:
            return user_id
    return ""


def _matrix_room_member_user_ids(room_id: str) -> list[str]:
    homeserver, token = _matrix_env("roomflow")
    encoded_room = urllib.parse.quote(room_id, safe="")
    request = urllib.request.Request(
        f"{homeserver}/_matrix/client/v3/rooms/{encoded_room}/members",
        headers={"Authorization": f"Bearer {token}"},
    )
    with urllib.request.urlopen(request, timeout=10) as response:
        data = json.loads(response.read().decode("utf-8") or "{}")
    members: list[str] = []
    for event in data.get("chunk", []):
        if not isinstance(event, dict):
            continue
        user_id = str(event.get("state_key") or "").strip()
        content = event.get("content") if isinstance(event.get("content"), dict) else {}
        membership = str(content.get("membership") or "").strip()
        if user_id and membership in {"join", "invite"}:
            members.append(user_id)
    return members










































































































def _visible_tool_names() -> list[str]:
    if _message_tool_blocked_for_runtime_role():
        return [name for name in TOOL_NAMES if name != "message"]
    return list(TOOL_NAMES)


def _message_tool_blocked_for_runtime_role() -> bool:
    return _runtime_role() in MESSAGE_TOOL_BLOCKED_ROLES


















ALLOWED_TASK_RESULT_STATUSES = {"SUCCESS", "SUCCESS_WITH_NOTES", "REVISION_NEEDED", "BLOCKED", "FAILED", "PARTIAL"}










TERMINAL_TASK_STATUSES = {"completed", "revision", "blocked", "cancelled"}










def handle_request(request: dict[str, Any]) -> dict[str, Any] | None:
    method = request.get("method")
    request_id = request.get("id")
    if request_id is None and isinstance(method, str) and method.startswith("notifications/"):
        return None
    if method == "initialize":
        result = {
            "protocolVersion": "2024-11-05",
            "serverInfo": {"name": "teamharness", "version": "0.1.0"},
            "capabilities": {"tools": {}},
        }
    elif method == "tools/list":
        result = {"tools": list_tools()}
    elif method == "tools/call":
        params = request.get("params", {}) or {}
        result = call_tool(str(params.get("name", "")), params.get("arguments", {}) or {})
    else:
        result = {
            "content": [
                {
                    "type": "text",
                    "text": json.dumps({"ok": False, "error": "unknown_method", "method": method}, ensure_ascii=False),
                }
            ]
        }
    return {"jsonrpc": "2.0", "id": request_id, "result": result}


def main() -> int:
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            request = json.loads(line)
        except json.JSONDecodeError:
            response = {
                "jsonrpc": "2.0",
                "id": None,
                "error": {"code": -32700, "message": "Parse error"},
            }
            print(json.dumps(response, ensure_ascii=False), flush=True)
            continue
        response = handle_request(request)
        if response is not None:
            print(json.dumps(response, ensure_ascii=False), flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
