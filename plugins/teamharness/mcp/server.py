#!/usr/bin/env python3
"""TeamHarness MCP stdio server entry point."""

from __future__ import annotations

import html
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import threading
import time
from typing import Any
import urllib.parse
import urllib.request
import uuid


TOOL_NAMES = ["health", "message", "roomflow", "filesync", "projectflow", "taskflow"]
MESSAGE_TOOL_BLOCKED_ROLES = {"worker", "remote-member"}
MATRIX_USER_RE = re.compile(r"@[a-zA-Z0-9._=+/\-]+:[a-zA-Z0-9.\-]+(?::\d+)?")
MENTION_LOCAL_CHARS = r"a-zA-Z0-9._=+/\-"
SHORT_MATRIX_MENTION_RE = re.compile(
    rf"(?<![{MENTION_LOCAL_CHARS}])@([{MENTION_LOCAL_CHARS}]+)(?![{MENTION_LOCAL_CHARS}])(?!:[a-zA-Z0-9.\-])"
)
MATRIX_ROOM_RE = re.compile(r"^![^:\s]+:[^\s]+$")
LOW_INFORMATION_ACKS = {"ack", "acknowledged", "ok", "okay", "done", "received", "收到", "好的", "好"}
MC_ALIAS = "hiclaw"
UNSAFE_SESSION_FILENAME_RE = re.compile(r'[\\/:*?"<>|]')
SESSION_WRITE_LOCKS: dict[str, threading.Lock] = {}

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
                    "description": "matrix for Matrix room sends, or an external channel such as dingtalk.",
                },
                "target": {
                    "type": "string",
                    "description": "Matrix room target for cross-room sends, for example room:!room:domain.",
                },
                "replyRoute": {
                    "type": "object",
                    "description": "Preferred requester route for cross-session reports.",
                    "additionalProperties": True,
                    "properties": {
                        "channel": {"type": "string"},
                        "targetUser": {"type": "string"},
                        "targetSession": {"type": "string"},
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
                "text": {
                    "type": "string",
                    "description": "Message text. message and body aliases are also accepted.",
                },
                "dryRun": {
                    "type": "boolean",
                    "description": "Return the resolved payload without sending.",
                },
            },
            "additionalProperties": True,
        },
    },
    "roomflow": {
        "description": (
            "Manage Matrix task rooms for TeamHarness execution-channel "
            "isolation: create a dedicated room for a project or quick task, "
            "list joined rooms, or archive a task room. Task rooms are internal "
            "Leader/Worker execution channels, not requester reply channels."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "action": {
                    "type": "string",
                    "enum": ["create_task_room", "list_rooms", "archive_room"],
                    "description": "Room operation to perform.",
                },
                "taskId": {
                    "type": "string",
                    "description": "Safe task or project id used in the room topic.",
                },
                "name": {
                    "type": "string",
                    "description": "Human-readable Matrix room name for create_task_room.",
                },
                "source": {
                    "type": "string",
                    "description": "Optional requester/source label such as matrix, dingtalk, or wechat. Non-Matrix sources require sourceRoomId.",
                },
                "sourceRoomId": {
                    "type": "string",
                    "description": "Stable external requester room/conversation id. Required for non-Matrix sources; the same source+sourceRoomId reuses the same task room.",
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
                    "description": "Matrix room id for archive_room, with or without room: prefix.",
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
            "under shared/projects, shared/tasks, or read-only global-shared. "
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
                    "description": "Relative path beginning with shared/projects/, shared/tasks/, or global-shared/.",
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
    "projectflow": {
        "description": (
            "Manage durable TeamHarness project state only after Project Work "
            "mode is selected: create projects, plan or update DAG and Loop "
            "work, query ready nodes, and record loop iterations. Do not use "
            "for ordinary direct replies or one-off checks."
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
                    "enum": ["delegate_task", "ack_task", "submit_task", "check_task"],
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


def _render_inline_matrix_html(text: str) -> str:
    parts = re.split(r"(`[^`\n]+`)", text)
    rendered: list[str] = []
    for part in parts:
        if len(part) >= 2 and part.startswith("`") and part.endswith("`"):
            rendered.append(f"<code>{html.escape(part[1:-1])}</code>")
            continue
        escaped = html.escape(part)
        escaped = re.sub(r"\*\*(.+?)\*\*", r"<strong>\1</strong>", escaped)
        rendered.append(escaped)
    return "".join(rendered)


def _table_cells(line: str) -> list[str]:
    return [cell.strip() for cell in line.strip().strip("|").split("|")]


def _is_table_separator(line: str) -> bool:
    cells = _table_cells(line)
    return bool(cells) and all(re.fullmatch(r":?-{3,}:?", cell or "") for cell in cells)


def _render_fallback_table(lines: list[str]) -> str:
    header = _table_cells(lines[0])
    rows = [_table_cells(line) for line in lines[2:]]
    parts = ["<table>", "<thead><tr>"]
    parts.extend(f"<th>{_render_inline_matrix_html(cell)}</th>" for cell in header)
    parts.append("</tr></thead>")
    if rows:
        parts.append("<tbody>")
        for row in rows:
            parts.append("<tr>")
            parts.extend(f"<td>{_render_inline_matrix_html(cell)}</td>" for cell in row)
            parts.append("</tr>")
        parts.append("</tbody>")
    parts.append("</table>")
    return "".join(parts)


def _md_to_html(text: str) -> str:
    try:
        from markdown_it import MarkdownIt

        md = MarkdownIt(
            "commonmark",
            {
                "html": False,
                "linkify": True,
                "breaks": True,
                "typographer": False,
            },
        )
        md.enable("strikethrough")
        md.enable("table")
        try:
            from linkify_it import LinkifyIt

            md.linkify = LinkifyIt()
        except ImportError:
            pass
        return md.render(text).rstrip("\n")
    except ImportError:
        pass

    lines = (text or "").splitlines()
    if not lines:
        return ""

    blocks: list[str] = []
    in_code_block = False
    code_lines: list[str] = []
    index = 0

    while index < len(lines):
        line = lines[index]
        if line.strip().startswith("```"):
            if in_code_block:
                code = html.escape("\n".join(code_lines))
                blocks.append(f"<pre><code>{code}</code></pre>")
                code_lines = []
                in_code_block = False
            else:
                in_code_block = True
                code_lines = []
            index += 1
            continue
        if in_code_block:
            code_lines.append(line)
            index += 1
            continue

        if index + 1 < len(lines) and "|" in line and _is_table_separator(lines[index + 1]):
            table_lines = [line, lines[index + 1]]
            index += 2
            while index < len(lines) and "|" in lines[index] and lines[index].strip():
                table_lines.append(lines[index])
                index += 1
            blocks.append(_render_fallback_table(table_lines))
            continue

        heading = re.match(r"^(#{1,6})\s+(.+?)\s*$", line)
        if heading:
            level = len(heading.group(1))
            blocks.append(f"<h{level}>{_render_inline_matrix_html(heading.group(2))}</h{level}>")
            index += 1
            continue

        if re.match(r"^\s*[-*]\s+\S", line):
            items: list[str] = []
            while index < len(lines):
                item = re.match(r"^\s*[-*]\s+(.+?)\s*$", lines[index])
                if not item:
                    break
                items.append(item.group(1))
                index += 1
            blocks.append("<ul>" + "".join(f"<li>{_render_inline_matrix_html(item)}</li>" for item in items) + "</ul>")
            continue

        if line.strip():
            blocks.append(_render_inline_matrix_html(line))
        else:
            blocks.append("")
        index += 1

    if in_code_block:
        code = html.escape("\n".join(code_lines))
        blocks.append(f"<pre><code>{code}</code></pre>")

    return "<br>\n".join(blocks)


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


def _qwenpaw_message(arguments: dict[str, Any], route: dict[str, Any], channel: str, message: str) -> dict[str, Any]:
    target_user = _route_value(arguments, route, "targetUser", "target_user", "userId", "user_id")
    target_session = _route_value(arguments, route, "targetSession", "target_session", "sessionId", "session_id")
    agent_id = str(arguments.get("agentId") or arguments.get("agent_id") or arguments.get("accountId") or "default").strip()
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
    if not target_user:
        return {"ok": False, "tool": "message", "channel": channel, "error": "targetUser is required for non-Matrix channel sends"}
    if not target_session:
        return {"ok": False, "tool": "message", "channel": channel, "error": "targetSession is required for non-Matrix channel sends"}
    if not message:
        return {"ok": False, "tool": "message", "channel": channel, "error": "message text is required"}
    if arguments.get("dryRun"):
        base["dryRun"] = True
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


def _session_safe(name: str) -> str:
    return UNSAFE_SESSION_FILENAME_RE.sub("--", name)


def _qwenpaw_working_dir() -> Path | None:
    for name in ("QWENPAW_WORKING_DIR", "COPAW_WORKING_DIR"):
        raw = os.getenv(name, "").strip()
        if raw:
            return Path(raw).expanduser()
    for name in ("HICLAW_AGENT_HOME", "HICLAW_WORKER_HOME", "HOME"):
        raw = os.getenv(name, "").strip()
        if raw:
            home = Path(raw).expanduser()
            return home / ".qwenpaw"
    return None


def _channel_session_path(channel: str, user_id: str, session_id: str, account_id: str) -> Path | None:
    working_dir = _qwenpaw_working_dir()
    if working_dir is None:
        return None

    workspace_name = account_id or "default"
    workspace_dir = working_dir / "workspaces" / workspace_name
    if not workspace_dir.exists() and workspace_name != "default":
        default_workspace = working_dir / "workspaces" / "default"
        if default_workspace.exists():
            workspace_dir = default_workspace

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
    return _record_outbound_to_session(
        channel="matrix",
        user_id=room_id,
        session_id=f"matrix:{room_id}",
        text=text,
        message_id=message_id,
        account_id=account_id,
        metadata={"room_id": room_id},
    )


def _message(arguments: dict[str, Any]) -> dict[str, Any]:
    action = arguments.get("action") or "send"
    route = _reply_route(arguments)
    channel = str(route.get("channel") or arguments.get("channel") or "matrix")
    if action != "send":
        return {"ok": False, "tool": "message", "error": f"unsupported action: {action}"}
    if channel != "matrix":
        message = str(arguments.get("message") or arguments.get("text") or arguments.get("body") or "")
        return _qwenpaw_message(arguments, route, channel, message)

    message = str(arguments.get("message") or arguments.get("text") or arguments.get("body") or "")
    try:
        target = arguments.get("target") or arguments.get("room_id") or arguments.get("roomId") or ""
        target_kind, target_id = _matrix_target(str(target))
    except ValueError as exc:
        return {"ok": False, "tool": "message", "error": str(exc)}
    if target_kind == "user":
        return {"ok": False, "tool": "message", "error": "Matrix user targets are not supported yet"}

    mentions = _mentions(message, target_id)
    blocked = _ping_pong_error(message, mentions)
    if blocked:
        return {"ok": False, "tool": "message", "error": blocked}

    content = _matrix_content(message, mentions)
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
    if arguments.get("dryRun"):
        base["dryRun"] = True
        return base

    homeserver = os.getenv("HICLAW_MATRIX_URL", "").rstrip("/")
    token = os.getenv("HICLAW_WORKER_MATRIX_TOKEN", "")
    if not homeserver or not token:
        return {"ok": False, "tool": "message", "error": "HICLAW_MATRIX_URL and HICLAW_WORKER_MATRIX_TOKEN are required"}

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
    account_id = str(arguments.get("agentId") or arguments.get("agent_id") or arguments.get("accountId") or arguments.get("account_id") or "default").strip() or "default"
    try:
        base["sessionRecorded"] = _record_matrix_outbound_to_session(
            target_id,
            message,
            base.get("messageId"),
            account_id,
        )
    except Exception:
        base["sessionRecorded"] = False
        base["warning"] = "message sent, but local session record failed"
    return base


def _matrix_env(tool: str) -> tuple[str, str]:
    homeserver = os.getenv("HICLAW_MATRIX_URL", "").rstrip("/")
    token = os.getenv("HICLAW_WORKER_MATRIX_TOKEN", "")
    if not homeserver or not token:
        raise ValueError("HICLAW_MATRIX_URL and HICLAW_WORKER_MATRIX_TOKEN are required")
    return homeserver, token


def _matrix_user_id() -> str:
    explicit = os.getenv("HICLAW_MATRIX_USER_ID", "").strip()
    if explicit:
        return explicit
    member = _section(_load_runtime_config(), "member")
    matrix_user_id = str(member.get("matrixUserId") or member.get("matrix_user_id") or "").strip()
    if matrix_user_id:
        return matrix_user_id
    name = os.getenv("HICLAW_WORKER_NAME", "").strip()
    domain = os.getenv("HICLAW_MATRIX_DOMAIN", "").strip()
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


def _roomflow(arguments: dict[str, Any]) -> dict[str, Any]:
    action = str(arguments.get("action") or "create_task_room")
    payload = _payload(arguments)
    if action == "create_task_room":
        return _create_task_room(arguments, payload)
    if action == "list_rooms":
        return _list_rooms(arguments)
    if action == "archive_room":
        return _archive_room(arguments, payload)
    return {"ok": False, "tool": "roomflow", "action": action, "error": f"unsupported action: {action}"}


def _create_task_room(arguments: dict[str, Any], payload: dict[str, Any]) -> dict[str, Any]:
    try:
        task_id = _safe_id(payload.get("taskId") or payload.get("projectId"), "taskId")
    except ValueError as exc:
        return {"ok": False, "tool": "roomflow", "action": "create_task_room", "error": str(exc)}
    name = str(payload.get("name") or payload.get("title") or "").strip()
    if not name:
        return {"ok": False, "tool": "roomflow", "action": "create_task_room", "error": "name is required"}
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
    binding = _roomflow_source_room_binding(arguments, payload)
    if binding.get("error"):
        return {"ok": False, "tool": "roomflow", "action": "create_task_room", "error": binding["error"]}

    base: dict[str, Any] = {
        "ok": True,
        "tool": "roomflow",
        "action": "create_task_room",
        "taskId": task_id,
        "name": name,
        "source": source,
        "topic": topic,
        "invite": invite,
        "content": body,
    }
    if binding.get("sourceRoomKey"):
        base["sourceRoomKey"] = binding["sourceRoomKey"]
    if binding.get("sourceRoomId"):
        base["sourceRoomId"] = binding["sourceRoomId"]
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
    _write_roomflow_source_room_binding(binding, room_id, base)
    return base


def _roomflow_source_room_binding(arguments: dict[str, Any], payload: dict[str, Any]) -> dict[str, Any]:
    source, source_room_id = _external_source_room_ref(payload)
    if not source:
        return {}
    if not source_room_id:
        return {"error": "sourceRoomId is required for non-Matrix source task rooms"}
    source_room_key = f"{source}:{source_room_id}"
    binding: dict[str, Any] = {
        "source": source,
        "sourceRoomId": source_room_id,
        "sourceRoomKey": source_room_key,
    }
    workspace_dir = _optional_workspace_dir(arguments)
    if not workspace_dir:
        return {"error": "workspaceDir is required to persist external source task room bindings"}
    digest = hashlib.sha256(source_room_key.encode("utf-8")).hexdigest()[:16]
    path = workspace_dir / "shared" / "roomflow" / "source-rooms" / f"{source}-{digest}.json"
    record = _read_json(path)
    binding["path"] = path
    binding["record"] = record
    return binding


def _external_source_room_ref(payload: dict[str, Any]) -> tuple[str, str]:
    source = str(payload.get("source") or "").strip().lower()
    if not source or source == "matrix":
        return "", ""
    return source, str(payload.get("sourceRoomId") or payload.get("source_room_id") or "").strip()


def _bound_room_id(binding: dict[str, Any]) -> str:
    record = binding.get("record")
    if not isinstance(record, dict):
        return ""
    return str(record.get("roomId") or record.get("room_id") or "").strip()


def _write_roomflow_source_room_binding(binding: dict[str, Any], room_id: str, base: dict[str, Any]) -> None:
    path = binding.get("path")
    if not isinstance(path, Path):
        return
    record = dict(binding.get("record") if isinstance(binding.get("record"), dict) else {})
    record.update(
        {
            "source": binding.get("source"),
            "sourceRoomId": binding.get("sourceRoomId"),
            "sourceRoomKey": binding.get("sourceRoomKey"),
            "roomId": room_id,
            "target": f"room:{room_id}",
            "updatedAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "taskId": base.get("taskId"),
            "name": base.get("name"),
        }
    )
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


def _remote_root(value: str) -> str:
    text = (value or "").strip()
    if not text:
        raise ValueError("storage sharedPrefix is required")
    return text.rstrip("/") + "/"


def _load_runtime_config() -> dict[str, Any]:
    runtime_config = os.getenv("TEAMHARNESS_RUNTIME_CONFIG", "").strip()
    if not runtime_config:
        return {}
    path = Path(runtime_config)
    if not path.exists():
        return {}
    text = path.read_text(encoding="utf-8")
    try:
        data = json.loads(text)
    except json.JSONDecodeError:
        data = None
    if isinstance(data, dict):
        return data
    try:
        import yaml

        data = yaml.safe_load(text) or {}
    except Exception:
        data = _simple_yaml_sections(text)
    return data if isinstance(data, dict) else {}


def _simple_yaml_sections(text: str) -> dict[str, Any]:
    data: dict[str, Any] = {}
    section: str | None = None
    nested_section: str | None = None
    for line in text.splitlines():
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        top = re.match(r"^([A-Za-z0-9_]+):\s*(.*)$", line)
        if top:
            key, value = top.group(1), top.group(2).strip()
            if value:
                data[key] = _yaml_scalar(value)
                section = None
                nested_section = None
            else:
                data[key] = {}
                section = key
                nested_section = None
            continue
        nested = re.match(r"^\s{2}([A-Za-z0-9_]+):\s*(.*)$", line)
        if nested and section and isinstance(data.get(section), dict):
            key, value = nested.group(1), nested.group(2).strip()
            if value:
                data[section][key] = _yaml_scalar(value)
                nested_section = None
            else:
                data[section][key] = {}
                nested_section = key
            continue
        deep = re.match(r"^\s{4}([A-Za-z0-9_]+):\s*(.*)$", line)
        if deep and section and nested_section and isinstance(data.get(section), dict):
            parent = data[section].get(nested_section)
            if isinstance(parent, dict):
                parent[deep.group(1)] = _yaml_scalar(deep.group(2).strip())
    return data


def _yaml_scalar(value: str) -> Any:
    if value in {"", "null", "Null", "NULL", "~"}:
        return ""
    if value in {"true", "True", "TRUE"}:
        return True
    if value in {"false", "False", "FALSE"}:
        return False
    if (value.startswith("'") and value.endswith("'")) or (value.startswith('"') and value.endswith('"')):
        return value[1:-1]
    return value


def _section(data: dict[str, Any], name: str) -> dict[str, Any]:
    value = data.get(name)
    return value if isinstance(value, dict) else {}


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


def _runtime_team_room_id() -> str:
    team = _section(_load_runtime_config(), "team")
    return str(team.get("teamRoomId") or team.get("team_room_id") or "").strip()


def _storage_root_prefix() -> str:
    return os.getenv("HICLAW_STORAGE_PREFIX", "").strip().strip("/")


def _mc_host_url(endpoint: str, access_key: str, secret_key: str) -> str:
    url = endpoint.strip().rstrip("/")
    if not url.startswith(("http://", "https://")):
        url = f"http://{url}"
    parsed = urllib.parse.urlsplit(url)
    if "@" in parsed.netloc:
        return url
    user = urllib.parse.quote(access_key, safe="")
    password = urllib.parse.quote(secret_key, safe="")
    netloc = f"{user}:{password}@{parsed.netloc}"
    return urllib.parse.urlunsplit(
        (parsed.scheme, netloc, parsed.path, parsed.query, parsed.fragment)
    )


def _remote_uses_mc_alias(remote: str) -> bool:
    return remote.strip().startswith(f"{MC_ALIAS}/")


def _mc_alias_configured(env: dict[str, str]) -> bool:
    try:
        completed = subprocess.run(
            ["mc", "alias", "list", MC_ALIAS],
            check=False,
            capture_output=True,
            text=True,
            timeout=20,
            env=env,
        )
    except (OSError, subprocess.SubprocessError):
        return False
    output = f"{completed.stdout}\n{completed.stderr}"
    return completed.returncode == 0 and f"{MC_ALIAS}" in output


def _filesync_mc_env(remote: str) -> tuple[dict[str, str], str | None]:
    env = dict(os.environ)
    if not _remote_uses_mc_alias(remote):
        return env, None

    alias_env = f"MC_HOST_{MC_ALIAS}"
    if env.get(alias_env):
        return env, None

    endpoint = env.get("HICLAW_FS_ENDPOINT", "").strip()
    access_key = env.get("HICLAW_FS_ACCESS_KEY", "").strip()
    secret_key = env.get("HICLAW_FS_SECRET_KEY", "").strip()
    if endpoint and access_key and secret_key:
        env[alias_env] = _mc_host_url(endpoint, access_key, secret_key)
        return env, None

    if _mc_alias_configured(env):
        return env, None
    return env, (
        f"storage alias {MC_ALIAS} is not configured; missing "
        "HICLAW_FS_ENDPOINT/HICLAW_FS_ACCESS_KEY/HICLAW_FS_SECRET_KEY"
    )


def _with_storage_root(prefix: str) -> str:
    clean = (prefix or "").strip().strip("/")
    root = _storage_root_prefix()
    if not root:
        return clean
    if not clean:
        return root
    if clean == root or clean.startswith(f"{root}/"):
        return clean
    return f"{root}/{clean}"


def _default_workspace_dir() -> str:
    """Derive workspace dir from environment (set by qwenpaw-worker / copaw-worker)."""
    for env_key in ("QWENPAW_WORKING_DIR", "COPAW_WORKING_DIR"):
        working_dir = os.getenv(env_key, "").strip()
        if working_dir:
            return str(Path(working_dir) / "workspaces" / "default")
    shared_dir = os.getenv("TEAMHARNESS_SHARED_DIR", "").strip() or os.getenv("HICLAW_SHARED_DIR", "").strip()
    if shared_dir:
        return str(Path(shared_dir).parent)
    return ""


def _default_shared_prefix() -> str:
    """Derive storage shared prefix from environment or runtime.yaml."""
    configured = os.getenv("HICLAW_SHARED_STORAGE_PREFIX", "").strip()
    if configured:
        return _with_storage_root(configured)
    storage = _section(_load_runtime_config(), "storage")
    prefix = str(storage.get("sharedPrefix") or "").strip()
    if prefix:
        return _with_storage_root(prefix)
    storage_prefix = _storage_root_prefix()
    if storage_prefix:
        return f"{storage_prefix}/shared"
    return "shared"


def _default_global_shared_prefix() -> str:
    """Derive global-shared storage prefix from environment or runtime.yaml."""
    storage = _section(_load_runtime_config(), "storage")
    prefix = str(storage.get("globalSharedPrefix") or "").strip()
    if prefix:
        return _with_storage_root(prefix)
    storage_prefix = _storage_root_prefix()
    if storage_prefix:
        return f"{storage_prefix}/shared"
    return "shared"


def _workspace_dir(arguments: dict[str, Any]) -> Path:
    value = str(arguments.get("workspaceDir") or "").strip()
    if not value:
        value = _default_workspace_dir()
    if not value:
        raise ValueError("workspaceDir is required")
    return Path(value).expanduser()


def _optional_workspace_dir(arguments: dict[str, Any]) -> Path | None:
    value = str(arguments.get("workspaceDir") or "").strip()
    if not value:
        value = _default_workspace_dir()
    return Path(value).expanduser() if value else None


def _normalize_exclude(value: Any) -> list[str]:
    if not value:
        return []
    if isinstance(value, str):
        text = value.strip()
        if not text:
            return []
        if text.startswith("["):
            parsed = json.loads(text)
            if not isinstance(parsed, list):
                raise ValueError("exclude must be a list")
            return [str(item) for item in parsed if str(item).strip()]
        return [text]
    if isinstance(value, list):
        return [str(item) for item in value if str(item).strip()]
    raise ValueError("exclude must be a list")


def _normalize_shared_path(raw_path: str, action: str) -> tuple[str, bool]:
    raw = (raw_path or "").strip()
    if not raw or raw.startswith("/") or "\\" in raw:
        raise ValueError("path must be a relative shared path")
    parts = raw.strip("/").split("/")
    if any(part in {"", ".", ".."} for part in parts):
        raise ValueError("path must be a relative shared path without '.', '..', or empty segments")
    if parts[0] not in {"shared", "global-shared"}:
        raise ValueError("path must start with shared/ or global-shared/")
    # Allow shared/ root for list, but require projects/ or tasks/ for push/pull
    if parts[0] == "shared" and len(parts) >= 2 and parts[1] not in {"projects", "tasks"}:
        raise ValueError("shared path must be under shared/projects/ or shared/tasks/")
    if parts[0] == "shared" and action in {"push", "pull"} and len(parts) < 3:
        raise ValueError("shared push/pull requires a project or task path")
    if parts[0] == "global-shared" and len(parts) < 2:
        raise ValueError("global-shared path must include a subpath")
    is_directory = raw.endswith("/") or (
        action in {"pull", "push", "list"}
        and len(parts) <= 3
        and parts[0] in {"shared", "global-shared"}
    )
    normalized = "/".join(parts)
    if is_directory:
        normalized += "/"
    return normalized, is_directory


def _resolve_filesync(arguments: dict[str, Any]) -> tuple[str, str, Path, str, bool]:
    action = str(arguments.get("action") or "").strip()
    if action not in {"pull", "push", "list", "stat"}:
        raise ValueError("action is required; use pull, push, stat, or list")
    normalized, is_directory = _normalize_shared_path(str(arguments.get("path") or ""), action)
    parts = normalized.strip("/").split("/")
    kind = parts[0]
    if kind == "global-shared" and action == "push":
        raise ValueError("global-shared is read-only for TeamHarness filesync")

    storage = arguments.get("storage") if isinstance(arguments.get("storage"), dict) else {}
    shared_prefix = str(storage.get("sharedPrefix") or "").strip() or _default_shared_prefix()
    shared_root = _remote_root(shared_prefix)
    global_root = ""
    if kind == "global-shared":
        global_shared_prefix = str(storage.get("globalSharedPrefix") or "").strip() or _default_global_shared_prefix()
        global_root = _remote_root(global_shared_prefix)
    workspace = _workspace_dir(arguments)
    local = workspace / Path(*parts)
    remote_root = shared_root if kind == "shared" else global_root
    remote = remote_root + "/".join(parts[1:])
    if is_directory:
        remote = remote.rstrip("/") + "/"
    return action, normalized, local, remote, is_directory


def _filesync(arguments: dict[str, Any]) -> dict[str, Any]:
    try:
        action, normalized, local, remote, is_directory = _resolve_filesync(arguments)
        exclude = _normalize_exclude(arguments.get("exclude"))
    except (ValueError, json.JSONDecodeError) as exc:
        return {"ok": False, "tool": "filesync", "error": str(exc)}

    kind = normalized.split("/", 1)[0]
    command: list[str]
    if action == "list":
        command = ["mc", "ls", "--recursive", remote]
    elif action == "stat":
        command = ["mc", "stat", remote]
    elif action == "pull":
        if is_directory:
            command = ["mc", "mirror", remote, str(local), "--overwrite"]
        else:
            command = ["mc", "cp", remote, str(local)]
    else:
        if is_directory:
            source = str(local) + ("/" if not str(local).endswith("/") else "")
            command = ["mc", "mirror", source, remote, "--overwrite"]
            for pattern in exclude:
                command.extend(["--exclude", pattern])
        else:
            command = ["mc", "cp", str(local), remote]

    base: dict[str, Any] = {
        "ok": True,
        "tool": "filesync",
        "action": action,
        "kind": kind,
        "path": normalized,
        "localPath": str(local),
        "remotePath": remote,
        "command": command,
        "exclude": exclude,
    }
    if arguments.get("dryRun"):
        base["dryRun"] = True
        return base

    if action == "pull" and not is_directory:
        local.parent.mkdir(parents=True, exist_ok=True)
    mc_env, env_error = _filesync_mc_env(remote)
    if env_error:
        return {
            "ok": False,
            "tool": "filesync",
            "action": action,
            "path": normalized,
            "error": env_error,
        }
    completed = subprocess.run(
        command,
        check=False,
        capture_output=True,
        text=True,
        timeout=120,
        env=mc_env,
    )
    command_error = _filesync_command_error(completed)
    if command_error:
        return {
            "ok": False,
            "tool": "filesync",
            "action": action,
            "path": normalized,
            "error": command_error,
            "returncode": completed.returncode,
        }
    if action == "list":
        base["entries"] = [line for line in completed.stdout.splitlines() if line.strip()]
    if action == "stat":
        base["exists"] = True
    return base


def _filesync_command_error(completed: subprocess.CompletedProcess[str]) -> str:
    output = "\n".join(part.strip() for part in (completed.stderr, completed.stdout) if part.strip())
    if completed.returncode != 0:
        return output or "filesync command failed"
    if "<ERROR>" in output or "Access Denied" in output:
        return output
    return ""


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
        "roomId": ("roomId", "room_id"),
        "sourceRoomId": ("sourceRoomId", "source_room_id"),
        "assignedTo": ("assignedTo", "assigned_to"),
        "dependsOn": ("dependsOn", "depends_on"),
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


def _project_timestamp() -> str:
    return time.strftime("%Y%m%d-%H%M%S")


def _unique_project_id(arguments: dict[str, Any], base_id: str) -> str:
    project_id = _safe_id(base_id, "projectId")
    if not _project_state_path(arguments, project_id).exists():
        return project_id
    for index in range(1, 1000):
        candidate = _safe_id(f"{base_id}-{index:02d}", "projectId")
        if not _project_state_path(arguments, candidate).exists():
            return candidate
    raise ValueError(f"cannot allocate unique project id for: {base_id}")


def _project_id_from_payload(arguments: dict[str, Any], payload: dict[str, Any]) -> str:
    explicit = payload.get("projectId") or payload.get("project_id")
    if explicit:
        project_id = _safe_id(explicit, "projectId")
        if _project_state_path(arguments, project_id).exists():
            raise ValueError(f"project already exists: {project_id}")
        return project_id
    title = str(payload.get("title") or payload.get("name") or "project")
    base_id = f"{_slugify(title, 'project')}-{_project_timestamp()}"
    return _unique_project_id(arguments, base_id)


def _normalize_reply_route(raw: Any) -> dict[str, str]:
    route = raw if isinstance(raw, dict) else {}
    channel = str(route.get("channel") or "").strip()
    target_user = str(route.get("targetUser") or route.get("target_user") or route.get("userId") or route.get("user_id") or "").strip()
    target_session = str(route.get("targetSession") or route.get("target_session") or route.get("sessionId") or route.get("session_id") or "").strip()
    if not (channel and target_user and target_session):
        return {}
    return {
        "channel": channel,
        "target_user": target_user,
        "target_session": target_session,
    }


def _reply_route_from_requester(requester: Any) -> dict[str, str]:
    text = str(requester or "").strip()
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


def _source_room_id_from_payload(payload: dict[str, Any], reply_route: dict[str, str] | None = None) -> str:
    source_room_id = str(payload.get("sourceRoomId") or payload.get("source_room_id") or "").strip()
    if source_room_id:
        return source_room_id

    route = reply_route if isinstance(reply_route, dict) else {}
    channel = str(route.get("channel") or payload.get("source") or "").strip().lower()
    if channel and channel != "matrix":
        return str(route.get("target_session") or "").strip()

    requester_route = _reply_route_from_requester(payload.get("requester"))
    channel = str(requester_route.get("channel") or "").strip().lower()
    if channel and channel != "matrix":
        return str(requester_route.get("target_session") or "").strip()
    return ""


def _canonical_room_id(value: Any) -> str:
    text = str(value or "").strip()
    if text.startswith("room:"):
        text = text[len("room:") :].strip()
    return text


def _external_requester_channel(project: dict[str, Any]) -> str:
    reply_route = project.get("reply_route") if isinstance(project.get("reply_route"), dict) else {}
    channel = str(reply_route.get("channel") or project.get("source") or "").strip().lower()
    if channel and channel != "matrix":
        return channel
    requester_route = _reply_route_from_requester(project.get("requester"))
    channel = str(requester_route.get("channel") or "").strip().lower()
    return channel if channel and channel != "matrix" else ""


def _validate_assignment_room(project: dict[str, Any], room_id: str) -> None:
    channel = _external_requester_channel(project)
    if not channel:
        return
    team_room_id = _runtime_team_room_id()
    if not team_room_id:
        return
    if _canonical_room_id(room_id) == _canonical_room_id(team_room_id):
        raise ValueError(
            f"{channel} requester tasks require a dedicated task room; "
            "call roomflow create_task_room and pass its roomId"
        )


def _read_json(path: Path, default: dict[str, Any] | None = None) -> dict[str, Any]:
    if not path.exists():
        return dict(default or {})
    return json.loads(path.read_text(encoding="utf-8"))


def _write_json(path: Path, data: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(data, ensure_ascii=False, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def _project_dir(arguments: dict[str, Any], project_id: str) -> Path:
    return _workspace_dir(arguments) / "shared" / "projects" / project_id


def _task_dir(arguments: dict[str, Any], task_id: str) -> Path:
    return _workspace_dir(arguments) / "shared" / "tasks" / task_id


def _project_state_path(arguments: dict[str, Any], project_id: str) -> Path:
    return _project_dir(arguments, project_id) / "meta.json"


def _task_state_path(arguments: dict[str, Any], task_id: str) -> Path:
    return _task_dir(arguments, task_id) / "meta.json"


def _normalize_task(raw: dict[str, Any], previous: dict[str, Any] | None = None) -> dict[str, Any]:
    task_id = _safe_id(raw.get("taskId") or raw.get("task_id"), "taskId")
    previous = previous or {}
    status = str(raw.get("status") or previous.get("status") or "planned")
    if status == "pending":
        status = "planned"
    return {
        "task_id": task_id,
        "title": str(raw.get("title") or previous.get("title") or task_id),
        "assigned_to": str(raw.get("assignedTo") or raw.get("assigned_to") or previous.get("assigned_to") or ""),
        "depends_on": [str(item) for item in (raw.get("dependsOn") or raw.get("depends_on") or previous.get("depends_on") or [])],
        "status": status,
    }


def _validate_task_graph(tasks: list[dict[str, Any]]) -> None:
    seen: set[str] = set()
    task_ids: set[str] = set()
    for task in tasks:
        task_id = str(task.get("task_id") or "")
        if task_id in seen:
            raise ValueError(f"duplicate task id: {task_id}")
        seen.add(task_id)
        task_ids.add(task_id)

    for task in tasks:
        task_id = str(task.get("task_id") or "")
        for dep in task.get("depends_on", []):
            if dep not in task_ids:
                raise ValueError(f"task {task_id} depends on unknown task: {dep}")

    visiting: set[str] = set()
    visited: set[str] = set()
    deps_by_id = {
        str(task.get("task_id")): [str(dep) for dep in task.get("depends_on", [])]
        for task in tasks
    }

    def visit(task_id: str, path: list[str]) -> None:
        if task_id in visited:
            return
        if task_id in visiting:
            raise ValueError(f"task dependency cycle detected: {' -> '.join(path + [task_id])}")
        visiting.add(task_id)
        for dep in deps_by_id.get(task_id, []):
            visit(dep, path + [task_id])
        visiting.remove(task_id)
        visited.add(task_id)

    for task_id in deps_by_id:
        visit(task_id, [])


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


def _safe_loop_status(value: Any) -> str:
    status = str(value or "running").strip()
    allowed = {"running", "waiting_user", "completed", "blocked"}
    if status not in allowed:
        raise ValueError(f"status must be one of: {', '.join(sorted(allowed))}")
    return status


def _safe_loop_decision(value: Any) -> str:
    decision = str(value or "").strip()
    allowed = {"continue", "replan", "ask_user", "stop_success", "stop_blocked"}
    if decision not in allowed:
        raise ValueError(f"decision must be one of: {', '.join(sorted(allowed))}")
    return decision


def _write_project_plan(project_dir: Path, project: dict[str, Any]) -> None:
    lines = [
        f"# {project.get('title') or project.get('project_id')}",
        "",
        f"- Project ID: `{project.get('project_id')}`",
        f"- Status: `{project.get('status')}`",
    ]
    plan_type = str(project.get("plan_type") or "").strip()
    if plan_type:
        lines.append(f"- Plan Type: `{plan_type}`")
    requester = project.get("requester")
    if requester:
        lines.append(f"- Requester: `{requester}`")
    source_room_id = project.get("source_room_id")
    if source_room_id:
        lines.append(f"- Source Room ID: `{source_room_id}`")
    reply_route = project.get("reply_route")
    if isinstance(reply_route, dict):
        channel = str(reply_route.get("channel") or "").strip()
        target_user = str(reply_route.get("target_user") or "").strip()
        target_session = str(reply_route.get("target_session") or "").strip()
        if channel and target_user and target_session:
            lines.append(f"- Reply Route: `{channel}/{target_user}/{target_session}`")
    loop = project.get("loop") if isinstance(project.get("loop"), dict) else {}
    if plan_type == "loop":
        lines.extend([
            f"- Loop Goal: {loop.get('goal') or ''}",
            f"- Stop Condition: {loop.get('stop_condition') or ''}",
            f"- Current Iteration: `{loop.get('current_iteration', 0)}` / `{loop.get('max_iterations', 0)}`",
            f"- Loop Status: `{loop.get('status') or 'running'}`",
            "",
            "## Iteration Template",
            "",
            str(loop.get("iteration_template") or ""),
        ])
        tasks = loop.get("tasks", []) if isinstance(loop.get("tasks"), list) else []
    else:
        tasks = project.get("tasks", []) if isinstance(project.get("tasks"), list) else []
    lines.extend(["", "## Tasks"])
    for task in tasks:
        deps = ", ".join(task.get("depends_on", [])) or "none"
        owner = task.get("assigned_to") or "unassigned"
        lines.append(f"- `{task['task_id']}` {task.get('title')} -> {owner}; deps: {deps}; status: {task.get('status')}")
    if plan_type == "loop":
        history = loop.get("history", []) if isinstance(loop.get("history"), list) else []
        if history:
            lines.extend(["", "## Loop History"])
            for item in history:
                if isinstance(item, dict):
                    iteration = item.get("iteration")
                    decision = item.get("decision")
                    summary = item.get("summary")
                    next_action = item.get("next_action")
                    detail = f"- Iteration {iteration}: `{decision}` - {summary}"
                    if next_action:
                        detail += f" Next: {next_action}"
                    lines.append(detail)
                else:
                    lines.append(f"- {item}")
    project_dir.mkdir(parents=True, exist_ok=True)
    (project_dir / "plan.md").write_text("\n".join(lines) + "\n", encoding="utf-8")


def _ready_nodes(project: dict[str, Any]) -> list[dict[str, Any]]:
    if project.get("plan_type") == "loop":
        raise ValueError(f"project plan is not a DAG: {project.get('project_id')}")
    if str(project.get("status") or "active") != "active":
        return []
    tasks = project.get("tasks", []) if isinstance(project.get("tasks"), list) else []
    status_by_id = {task.get("task_id"): task.get("status") for task in tasks}
    ready: list[dict[str, Any]] = []
    for task in tasks:
        if task.get("status") not in {"planned", "assigned"}:
            continue
        if all(status_by_id.get(dep) == "completed" for dep in task.get("depends_on", [])):
            ready.append(task)
    return ready


def _ready_loop_nodes(project: dict[str, Any]) -> list[dict[str, Any]]:
    if str(project.get("status") or "active") != "active":
        return []
    loop = project.get("loop")
    if not isinstance(loop, dict):
        raise ValueError(f"project has no loop plan: {project.get('project_id')}")
    if str(loop.get("status") or "running") in {"completed", "blocked", "waiting_user"}:
        return []
    tasks = loop.get("tasks", []) if isinstance(loop.get("tasks"), list) else []
    status_by_id = {task.get("task_id"): task.get("status") for task in tasks}
    ready: list[dict[str, Any]] = []
    for task in tasks:
        if task.get("status") not in {"planned", "assigned"}:
            continue
        if all(status_by_id.get(dep) == "completed" for dep in task.get("depends_on", [])):
            ready.append(task)
    return ready


def _resolve_project(arguments: dict[str, Any], payload: dict[str, Any]) -> dict[str, Any]:
    task: dict[str, Any] = {}
    project_id_value = payload.get("projectId") or payload.get("project_id")
    task_id_value = payload.get("taskId") or payload.get("task_id")
    if task_id_value:
        task_id = _safe_id(task_id_value, "taskId")
        task = _read_json(_task_state_path(arguments, task_id))
        if not task:
            raise ValueError("task not found")
        project_id_value = task.get("project_id")
    if not project_id_value:
        raise ValueError("projectId or taskId is required")
    project_id = _safe_id(project_id_value, "projectId")
    project = _read_json(_project_state_path(arguments, project_id))
    if not project:
        raise ValueError("project not found")
    plan_type = str(project.get("plan_type") or "dag")
    ready = _ready_loop_nodes(project) if plan_type == "loop" else _ready_nodes(project)
    result = {
        "ok": True,
        "tool": "projectflow",
        "action": "resolve_project",
        "project": project,
        "planType": plan_type,
        "replyRoute": project.get("reply_route"),
        "sourceRoomId": project.get("source_room_id") or (task.get("source_room_id") if task else None),
        "readyNodes": ready,
    }
    if task:
        result["task"] = task
    return result


def _accepted_node_status(result_status: Any) -> str:
    status = str(result_status or "SUCCESS").strip()
    if status in {"SUCCESS", "SUCCESS_WITH_NOTES"}:
        return "completed"
    if status == "REVISION_NEEDED":
        return "revision"
    if status in {"BLOCKED", "INTERRUPTED"}:
        return "blocked"
    raise ValueError(f"unsupported result status: {status}")


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


def _accept_task_result(arguments: dict[str, Any], payload: dict[str, Any]) -> dict[str, Any]:
    project_id = _safe_id(payload.get("projectId") or payload.get("project_id"), "projectId")
    task_id = _safe_id(payload.get("taskId") or payload.get("task_id"), "taskId")
    project = _read_json(_project_state_path(arguments, project_id))
    if not project:
        raise ValueError("project not found")
    result_status_value = payload.get("resultStatus") or payload.get("result_status")
    accepted = _payload_bool(payload.get("accepted"), True)
    node_status = _accepted_node_status(result_status_value)
    if not accepted and node_status == "completed":
        result_status_value = "REVISION_NEEDED"
        node_status = "revision"
    changed = False
    for task in project.get("tasks", []):
        if task.get("task_id") == task_id:
            task["status"] = node_status
            changed = True
            break
    loop = project.get("loop") if isinstance(project.get("loop"), dict) else {}
    loop_tasks = loop.get("tasks", []) if isinstance(loop.get("tasks"), list) else []
    for task in loop_tasks:
        if task.get("task_id") == task_id:
            task["status"] = node_status
            project["loop"] = loop
            changed = True
            break
    if not changed:
        raise ValueError("task not found in project plan")
    result_status = str(result_status_value or "SUCCESS")
    if node_status == "completed":
        project["requester_report"] = {
            "pending": True,
            "reason": "task_result_accepted",
            "task_id": task_id,
            "result_status": result_status,
            "summary": str(payload.get("summary") or ""),
            "report_path": f"shared/projects/{project_id}/result.md",
        }
    else:
        requester_report = project.get("requester_report") if isinstance(project.get("requester_report"), dict) else {}
        if requester_report.get("task_id") == task_id:
            requester_report["pending"] = False
            requester_report["reason"] = f"task_result_{node_status}"
            project["requester_report"] = requester_report
    _write_json(_project_state_path(arguments, project_id), project)
    _write_project_plan(_project_dir(arguments, project_id), project)
    return {
        "ok": True,
        "tool": "projectflow",
        "action": "accept_task_result",
        "project": project,
        "taskId": task_id,
        "nodeStatus": node_status,
        "accepted": node_status == "completed",
    }


def _mark_requester_report_sent(arguments: dict[str, Any], payload: dict[str, Any]) -> dict[str, Any]:
    project_id = _safe_id(payload.get("projectId") or payload.get("project_id"), "projectId")
    project = _read_json(_project_state_path(arguments, project_id))
    if not project:
        raise ValueError("project not found")
    requester_report = project.get("requester_report") if isinstance(project.get("requester_report"), dict) else {}
    requester_report["pending"] = False
    requester_report["sent_at"] = str(payload.get("sentAt") or payload.get("sent_at") or time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()))
    project["requester_report"] = requester_report
    _write_json(_project_state_path(arguments, project_id), project)
    return {
        "ok": True,
        "tool": "projectflow",
        "action": "mark_requester_report_sent",
        "project": project,
    }


def _projectflow(arguments: dict[str, Any]) -> dict[str, Any]:
    action = str(arguments.get("action") or "").strip()
    payload = _payload(arguments)
    try:
        if action == "create_project":
            project_id = _project_id_from_payload(arguments, payload)
            project = {
                "project_id": project_id,
                "title": str(payload.get("title") or project_id),
                "source": str(payload.get("source") or ""),
                "requester": str(payload.get("requester") or ""),
                "status": "active",
                "tasks": [],
            }
            reply_route = _normalize_reply_route(payload.get("replyRoute") or payload.get("reply_route"))
            if not reply_route:
                reply_route = _reply_route_from_requester(project["requester"])
            if reply_route:
                project["reply_route"] = reply_route
            source_room_id = _source_room_id_from_payload(payload, reply_route)
            if source_room_id:
                project["source_room_id"] = source_room_id
            project_dir = _project_dir(arguments, project_id)
            _write_json(_project_state_path(arguments, project_id), project)
            _write_project_plan(project_dir, project)
            return {"ok": True, "tool": "projectflow", "action": action, "project": project}

        if action == "create_quick_project":
            project_id = _project_id_from_payload(arguments, payload)
            title = str(payload.get("title") or project_id)
            assigned_to = str(payload.get("assignedTo") or payload.get("assigned_to") or "").strip()
            if not assigned_to:
                raise ValueError("assignedTo is required")
            room_id = str(payload.get("roomId") or payload.get("room_id") or "").strip()
            if not room_id:
                raise ValueError("roomId is required")
            spec = str(payload.get("spec") or "").strip()
            if not spec:
                raise ValueError("spec is required")
            task_id = _safe_id(payload.get("taskId") or payload.get("task_id") or f"{project_id}-01", "taskId")
            if not task_id.startswith(f"{project_id}-"):
                raise ValueError("taskId must belong to projectId")
            if _task_state_path(arguments, task_id).exists():
                raise ValueError(f"task already exists: {task_id}")
            task_node = {
                "task_id": task_id,
                "title": title,
                "assigned_to": assigned_to,
                "depends_on": [],
                "status": "assigned",
            }
            project = {
                "project_id": project_id,
                "title": title,
                "source": str(payload.get("source") or ""),
                "requester": str(payload.get("requester") or ""),
                "status": "active",
                "mode": "quick",
                "plan_type": "dag",
                "tasks": [task_node],
            }
            reply_route = _normalize_reply_route(payload.get("replyRoute") or payload.get("reply_route"))
            if not reply_route:
                reply_route = _reply_route_from_requester(project["requester"])
            if reply_route:
                project["reply_route"] = reply_route
            source_room_id = _source_room_id_from_payload(payload, reply_route)
            if source_room_id:
                project["source_room_id"] = source_room_id
                task_node["source_room_id"] = source_room_id
            _validate_assignment_room(project, room_id)
            project_dir = _project_dir(arguments, project_id)
            _write_json(_project_state_path(arguments, project_id), project)
            _write_project_plan(project_dir, project)

            task_dir = _task_dir(arguments, task_id)
            task_dir.mkdir(parents=True, exist_ok=True)
            (task_dir / "spec.md").write_text(spec + "\n", encoding="utf-8")
            task = {
                "task_id": task_id,
                "project_id": project_id,
                "room_id": room_id,
                "status": "assigned",
                "spec_path": f"shared/tasks/{task_id}/spec.md",
            }
            if source_room_id:
                task["source_room_id"] = source_room_id
            _write_task(arguments, task)
            return {
                "ok": True,
                "tool": "projectflow",
                "action": action,
                "project": project,
                "task": task,
                "synced": _sync_task(arguments, task_id),
            }

        if action == "resolve_project":
            return _resolve_project(arguments, payload)

        if action == "accept_task_result":
            return _accept_task_result(arguments, payload)

        if action == "mark_requester_report_sent":
            return _mark_requester_report_sent(arguments, payload)

        if action == "plan_dag":
            project_id = _safe_id(payload.get("projectId") or payload.get("project_id"), "projectId")
            state_path = _project_state_path(arguments, project_id)
            project = _read_json(state_path, {"project_id": project_id, "title": project_id, "status": "active", "tasks": []})
            previous = {task.get("task_id"): task for task in project.get("tasks", [])}
            raw_tasks = payload.get("tasks")
            if not isinstance(raw_tasks, list):
                raise ValueError("tasks must be a list")
            planned_tasks = [
                _normalize_task(task, previous.get(str(task.get("taskId") or task.get("task_id"))))
                for task in raw_tasks
                if isinstance(task, dict)
            ]
            _validate_task_graph(planned_tasks)
            project["tasks"] = planned_tasks
            project["plan_type"] = "dag"
            project_dir = _project_dir(arguments, project_id)
            _write_json(state_path, project)
            _write_project_plan(project_dir, project)
            return {
                "ok": True,
                "tool": "projectflow",
                "action": action,
                "project": project,
                "readyNodes": _ready_nodes(project),
            }

        if action == "plan_loop":
            project_id = _safe_id(payload.get("projectId") or payload.get("project_id"), "projectId")
            state_path = _project_state_path(arguments, project_id)
            project = _read_json(state_path, {"project_id": project_id, "title": project_id, "status": "active", "tasks": []})
            previous_loop = project.get("loop") if isinstance(project.get("loop"), dict) else {}
            previous_tasks = {
                task.get("task_id"): task
                for task in (previous_loop.get("tasks", []) if isinstance(previous_loop.get("tasks"), list) else [])
            }
            raw_tasks = payload.get("tasks") or []
            if not isinstance(raw_tasks, list):
                raise ValueError("tasks must be a list")
            max_iterations = _positive_int(payload.get("maxIterations") or payload.get("max_iterations"), "maxIterations")
            current_iteration = _non_negative_int(
                payload.get("currentIteration") or payload.get("current_iteration") or previous_loop.get("current_iteration") or 0,
                "currentIteration",
            )
            if current_iteration > max_iterations:
                raise ValueError("currentIteration cannot exceed maxIterations")
            planned_tasks = [
                _normalize_task(task, previous_tasks.get(str(task.get("taskId") or task.get("task_id"))))
                for task in raw_tasks
                if isinstance(task, dict)
            ]
            _validate_task_graph(planned_tasks)
            loop = {
                "goal": str(payload.get("goal") or previous_loop.get("goal") or "").strip(),
                "stop_condition": str(payload.get("stopCondition") or payload.get("stop_condition") or previous_loop.get("stop_condition") or "").strip(),
                "iteration_template": str(payload.get("iterationTemplate") or payload.get("iteration_template") or previous_loop.get("iteration_template") or "").strip(),
                "max_iterations": max_iterations,
                "current_iteration": current_iteration,
                "status": _safe_loop_status(payload.get("status") or previous_loop.get("status") or "running"),
                "tasks": planned_tasks,
                "history": previous_loop.get("history", []) if isinstance(previous_loop.get("history"), list) else [],
            }
            if not loop["goal"]:
                raise ValueError("goal is required")
            if not loop["stop_condition"]:
                raise ValueError("stopCondition is required")
            if not loop["iteration_template"]:
                raise ValueError("iterationTemplate is required")
            project["plan_type"] = "loop"
            project["loop"] = loop
            project_dir = _project_dir(arguments, project_id)
            _write_json(state_path, project)
            _write_project_plan(project_dir, project)
            return {
                "ok": True,
                "tool": "projectflow",
                "action": action,
                "project": project,
                "loop": loop,
                "readyLoopNodes": _ready_loop_nodes(project),
            }

        if action == "ready_nodes":
            project_id = _safe_id(payload.get("projectId") or payload.get("project_id"), "projectId")
            project = _read_json(_project_state_path(arguments, project_id))
            if not project:
                raise ValueError("project not found")
            return {
                "ok": True,
                "tool": "projectflow",
                "action": action,
                "project": project,
                "readyNodes": _ready_nodes(project),
            }

        if action == "ready_loop_nodes":
            project_id = _safe_id(payload.get("projectId") or payload.get("project_id"), "projectId")
            project = _read_json(_project_state_path(arguments, project_id))
            if not project:
                raise ValueError("project not found")
            return {
                "ok": True,
                "tool": "projectflow",
                "action": action,
                "project": project,
                "loop": project.get("loop", {}),
                "readyLoopNodes": _ready_loop_nodes(project),
            }

        if action == "record_loop_iteration":
            project_id = _safe_id(payload.get("projectId") or payload.get("project_id"), "projectId")
            project = _read_json(_project_state_path(arguments, project_id))
            if not project:
                raise ValueError("project not found")
            loop = project.get("loop") if isinstance(project.get("loop"), dict) else {}
            if not loop:
                raise ValueError(f"project has no loop plan: {project_id}")
            iteration = _positive_int(payload.get("iteration"), "iteration")
            max_iterations = _positive_int(loop.get("max_iterations"), "maxIterations")
            if iteration > max_iterations:
                raise ValueError("iteration cannot exceed maxIterations")
            decision = _safe_loop_decision(payload.get("decision"))
            loop["status"] = {
                "continue": "running",
                "replan": "running",
                "ask_user": "waiting_user",
                "stop_success": "completed",
                "stop_blocked": "blocked",
            }[decision]
            loop["current_iteration"] = max(_non_negative_int(loop.get("current_iteration") or 0, "currentIteration"), iteration)
            history = loop.get("history", []) if isinstance(loop.get("history"), list) else []
            history.append({
                "iteration": iteration,
                "decision": decision,
                "summary": str(payload.get("summary") or "").strip(),
                "next_action": str(payload.get("nextAction") or payload.get("next_action") or "").strip(),
            })
            loop["history"] = history
            project["plan_type"] = "loop"
            project["loop"] = loop
            project_dir = _project_dir(arguments, project_id)
            _write_json(_project_state_path(arguments, project_id), project)
            _write_project_plan(project_dir, project)
            return {
                "ok": True,
                "tool": "projectflow",
                "action": action,
                "project": project,
                "loop": loop,
                "readyLoopNodes": _ready_loop_nodes(project),
            }

        if action in {"pause_project", "resume_project", "complete_project"}:
            project_id = _safe_id(payload.get("projectId") or payload.get("project_id"), "projectId")
            state_path = _project_state_path(arguments, project_id)
            project = _read_json(state_path)
            if not project:
                raise ValueError("project not found")
            if action == "pause_project":
                project["status"] = "paused"
            elif action == "resume_project":
                project["status"] = "active"
            else:
                project["status"] = "completed"
                loop = project.get("loop") if isinstance(project.get("loop"), dict) else {}
                if loop:
                    loop["status"] = "completed"
                    project["loop"] = loop
            project_dir = _project_dir(arguments, project_id)
            _write_json(state_path, project)
            _write_project_plan(project_dir, project)
            return {
                "ok": True,
                "tool": "projectflow",
                "action": action,
                "project": project,
            }
    except ValueError as exc:
        return {"ok": False, "tool": "projectflow", "action": action, "error": str(exc)}

    return {"ok": False, "tool": "projectflow", "action": action, "error": f"unsupported action: {action}"}


def _normalize_role(role: str) -> str:
    value = (role or "").strip().replace("_", "-").lower()
    return {
        "team-leader": "leader",
        "teamleader": "leader",
        "team-leader-agent": "leader",
        "remote": "remote-member",
        "remote-member-agent": "remote-member",
    }.get(value, value)


def _runtime_role() -> str:
    role = os.getenv("HICLAW_AGENT_ROLE", "").strip() or os.getenv("HICLAW_WORKER_ROLE", "").strip()
    if not role:
        role = str(_section(_load_runtime_config(), "member").get("role") or "").strip()
    return _normalize_role(role)


def _visible_tool_names() -> list[str]:
    if _message_tool_blocked_for_runtime_role():
        return [name for name in TOOL_NAMES if name != "message"]
    return list(TOOL_NAMES)


def _message_tool_blocked_for_runtime_role() -> bool:
    return _runtime_role() in MESSAGE_TOOL_BLOCKED_ROLES


def _role(arguments: dict[str, Any]) -> str:
    role = str(arguments.get("role") or "").strip()
    if not role:
        return _runtime_role()
    return _normalize_role(role)


def _load_task(arguments: dict[str, Any], task_id: str) -> dict[str, Any]:
    task = _read_json(_task_state_path(arguments, task_id))
    if not task:
        raise ValueError("task not found")
    return task


def _write_task(arguments: dict[str, Any], task: dict[str, Any]) -> None:
    _write_json(_task_state_path(arguments, task["task_id"]), task)


def _parse_task_result(path: Path) -> tuple[dict[str, Any], list[str]]:
    result: dict[str, Any] = {"status": "", "summary": "", "deliverables": []}
    errors: list[str] = []
    in_deliverables = False
    has_deliverables_section = False

    for line in path.read_text(encoding="utf-8").splitlines():
        stripped = line.strip()
        status_match = re.match(r"^(?:-\s*)?Status:\s*`?([^`]+?)`?\s*$", stripped, re.IGNORECASE)
        if status_match:
            result["status"] = status_match.group(1).strip()
            continue
        summary_match = re.match(r"^(?:-\s*)?Summary:\s*(.+?)\s*$", stripped, re.IGNORECASE)
        if summary_match:
            result["summary"] = summary_match.group(1).strip()
            continue
        if stripped.lower() in {"## deliverables", "deliverables:"}:
            in_deliverables = True
            has_deliverables_section = True
            continue
        if in_deliverables and stripped.startswith("- "):
            item = stripped[2:].strip().strip("`")
            if item:
                result["deliverables"].append(item)

    status = str(result.get("status") or "").strip()
    if not status:
        errors.append("missing result status")
    elif status not in {"SUCCESS", "SUCCESS_WITH_NOTES", "REVISION_NEEDED", "BLOCKED", "FAILED", "PARTIAL"}:
        errors.append(f"invalid result status: {status}")
    if not str(result.get("summary") or "").strip():
        errors.append("missing result summary")
    if not has_deliverables_section:
        errors.append("missing deliverables section")
    return result, errors


def _sync_task(arguments: dict[str, Any], task_id: str, exclude: list[str] | None = None) -> bool:
    sync_args = dict(arguments)
    sync_args.update({
        "action": "push",
        "path": f"shared/tasks/{task_id}",
    })
    if exclude is not None:
        sync_args["exclude"] = exclude
    result = _filesync(sync_args)
    return bool(result.get("ok"))


def _pull_task(arguments: dict[str, Any], task_id: str) -> bool:
    sync_args = dict(arguments)
    sync_args.update({
        "action": "pull",
        "path": f"shared/tasks/{task_id}",
    })
    result = _filesync(sync_args)
    return bool(result.get("ok"))


def _update_project_task(arguments: dict[str, Any], project_id: str, task_id: str, **updates: Any) -> None:
    path = _project_state_path(arguments, project_id)
    project = _read_json(path)
    if not project:
        return
    changed = False
    for task in project.get("tasks", []):
        if task.get("task_id") == task_id:
            task.update(updates)
            changed = True
            break
    loop = project.get("loop") if isinstance(project.get("loop"), dict) else {}
    loop_tasks = loop.get("tasks", []) if isinstance(loop.get("tasks"), list) else []
    for task in loop_tasks:
        if task.get("task_id") == task_id:
            task.update(updates)
            changed = True
            break
    if changed:
        _write_json(path, project)
        _write_project_plan(_project_dir(arguments, project_id), project)


def _taskflow(arguments: dict[str, Any]) -> dict[str, Any]:
    action = str(arguments.get("action") or "").strip()
    payload = _payload(arguments)
    role = _role(arguments)
    try:
        if action == "delegate_task":
            if role != "leader":
                raise ValueError("delegate_task requires leader role")
            project_id = _safe_id(payload.get("projectId") or payload.get("project_id"), "projectId")
            task_id = _safe_id(payload.get("taskId") or payload.get("task_id"), "taskId")
            room_id = str(payload.get("roomId") or payload.get("room_id") or "")
            if not room_id:
                raise ValueError("roomId is required")
            project = _read_json(_project_state_path(arguments, project_id))
            _validate_assignment_room(project, room_id)
            task_dir = _task_dir(arguments, task_id)
            task_dir.mkdir(parents=True, exist_ok=True)
            spec = str(payload.get("spec") or "")
            (task_dir / "spec.md").write_text(spec + ("\n" if spec else ""), encoding="utf-8")
            source_room_id = _source_room_id_from_payload(payload) or str(project.get("source_room_id") or "").strip()
            task = {
                "task_id": task_id,
                "project_id": project_id,
                "room_id": room_id,
                "status": "assigned",
                "spec_path": f"shared/tasks/{task_id}/spec.md",
            }
            if source_room_id:
                task["source_room_id"] = source_room_id
            _write_task(arguments, task)
            project_task_updates: dict[str, Any] = {"status": "assigned"}
            if source_room_id:
                project_task_updates["source_room_id"] = source_room_id
            _update_project_task(arguments, project_id, task_id, **project_task_updates)
            return {
                "ok": True,
                "tool": "taskflow",
                "action": action,
                "task": task,
                "synced": _sync_task(arguments, task_id),
            }

        if action == "ack_task":
            if role not in {"worker", "remote-member"}:
                raise ValueError("ack_task requires worker or remote-member role")
            task_id = _safe_id(payload.get("taskId") or payload.get("task_id"), "taskId")
            pulled = _pull_task(arguments, task_id)
            task = _load_task(arguments, task_id)
            task["status"] = "in_progress"
            task["acknowledged_by_role"] = role
            _write_task(arguments, task)
            _update_project_task(arguments, task.get("project_id", ""), task_id, status="in_progress")
            spec_path = _task_dir(arguments, task_id) / "spec.md"
            spec = spec_path.read_text(encoding="utf-8") if spec_path.exists() else ""
            return {
                "ok": True,
                "tool": "taskflow",
                "action": action,
                "task": task,
                "spec": spec,
                "pulled": pulled,
                "synced": _sync_task(arguments, task_id, exclude=["spec.md", "base/"]),
            }

        if action == "submit_task":
            if role not in {"worker", "remote-member"}:
                raise ValueError("submit_task requires worker or remote-member role")
            task_id = _safe_id(payload.get("taskId") or payload.get("task_id"), "taskId")
            task = _load_task(arguments, task_id)
            summary = str(payload.get("summary") or "")
            status = str(payload.get("status") or "SUCCESS")
            deliverables = payload.get("deliverables") or []
            if not isinstance(deliverables, list):
                raise ValueError("deliverables must be a list")
            result_lines = [
                "# Task Result",
                "",
                f"- Status: `{status}`",
                f"- Summary: {summary}",
                "",
                "## Deliverables",
            ]
            result_lines.extend(f"- `{item}`" for item in deliverables)
            task_dir = _task_dir(arguments, task_id)
            task_dir.mkdir(parents=True, exist_ok=True)
            (task_dir / "result.md").write_text("\n".join(result_lines) + "\n", encoding="utf-8")
            task.update({
                "status": "submitted",
                "result_status": status,
                "summary": summary,
                "deliverables": [str(item) for item in deliverables],
                "result_path": f"shared/tasks/{task_id}/result.md",
                "submitted_by_role": role,
            })
            _write_task(arguments, task)
            _update_project_task(arguments, task.get("project_id", ""), task_id, status="submitted")
            return {
                "ok": True,
                "tool": "taskflow",
                "action": action,
                "task": task,
                "synced": _sync_task(arguments, task_id, exclude=["spec.md", "base/"]),
            }

        if action == "check_task":
            if role != "leader":
                raise ValueError("check_task requires leader role")
            task_id = _safe_id(payload.get("taskId") or payload.get("task_id"), "taskId")
            pulled = _pull_task(arguments, task_id)
            task = _load_task(arguments, task_id)
            result_path = _task_dir(arguments, task_id) / "result.md"
            if result_path.exists():
                result, validation_errors = _parse_task_result(result_path)
            else:
                result, validation_errors = {}, ["missing result.md"]
            effective = task.get("status") == "submitted" and result_path.exists() and not validation_errors
            return {
                "ok": True,
                "tool": "taskflow",
                "action": action,
                "task": task,
                "result": result,
                "validationErrors": validation_errors,
                "effective": effective,
                "pulled": pulled,
            }
    except ValueError as exc:
        return {"ok": False, "tool": "taskflow", "action": action, "error": str(exc)}

    return {"ok": False, "tool": "taskflow", "action": action, "error": f"unsupported action: {action}"}


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
