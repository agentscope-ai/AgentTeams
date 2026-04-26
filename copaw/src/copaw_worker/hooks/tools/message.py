"""OpenClaw-compatible message tool for the HiClaw CoPaw runtime."""

from __future__ import annotations

import html
import json
import re
import urllib.parse
from dataclasses import dataclass
from typing import Any, Literal

from agentscope.message import TextBlock
from agentscope.tool import ToolResponse
from copaw_worker.hooks.message_filter import (
    extract_matrix_mentions,
    filter_outgoing_matrix_message,
)

_MATRIX_ROOM_ID_RE = re.compile(r"^![^:\s]+:[^\s]+$")
_MATRIX_USER_ID_RE = re.compile(
    r"@[a-zA-Z0-9._=+/\-]+:[a-zA-Z0-9.\-]+(?::\d+)?",
)


@dataclass(frozen=True)
class MatrixTarget:
    kind: Literal["room", "user"]
    identifier: str


class MessageToolError(ValueError):
    """Expected user-facing error from the message tool."""


def _response(payload: dict[str, Any]) -> ToolResponse:
    return ToolResponse(
        content=[
            TextBlock(
                type="text",
                text=json.dumps(payload, ensure_ascii=False),
            ),
        ],
    )


def _ok(**payload: Any) -> ToolResponse:
    return _response({"ok": True, **payload})


def _error(message: str, **payload: Any) -> ToolResponse:
    return _response({"ok": False, "error": message, **payload})


def parse_matrix_target(target: str) -> MatrixTarget:
    """Parse OpenClaw-style Matrix targets."""
    raw = (target or "").strip()
    if not raw:
        raise MessageToolError("target is required")

    if raw.startswith("matrix:"):
        raw = raw[len("matrix:") :]

    if raw.startswith("room:"):
        room_id = raw[len("room:") :].strip()
        if not _MATRIX_ROOM_ID_RE.match(room_id):
            raise MessageToolError(f"invalid Matrix room target: {target}")
        return MatrixTarget(kind="room", identifier=room_id)

    if raw.startswith("user:"):
        user_id = raw[len("user:") :].strip()
        if not _MATRIX_USER_ID_RE.fullmatch(user_id):
            raise MessageToolError(f"invalid Matrix user target: {target}")
        return MatrixTarget(kind="user", identifier=user_id)

    if raw.startswith("!") and _MATRIX_ROOM_ID_RE.match(raw):
        return MatrixTarget(kind="room", identifier=raw)

    if raw.startswith("@") and _MATRIX_USER_ID_RE.fullmatch(raw):
        return MatrixTarget(kind="user", identifier=raw)

    raise MessageToolError(
        "target must be a Matrix room or user target, e.g. "
        "room:!room:domain or user:@user:domain",
    )


def validate_matrix_message_policy(text: str, mentions: list[str]) -> str:
    """Filter outgoing messages and return the sanitized text."""
    result = filter_outgoing_matrix_message(text, mentions)
    if result.suppressed:
        raise MessageToolError(result.suppress_reason or "message suppressed")
    return result.text


def _render_inline_matrix_html(text: str) -> str:
    """Render a small, safe Markdown subset accepted by Matrix clients."""
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


def _render_matrix_formatted_body(text: str) -> str:
    """Render Matrix custom HTML without relying on optional Markdown deps."""
    lines = (text or "").splitlines()
    if not lines:
        return ""

    blocks: list[str] = []
    in_code_block = False
    code_lines: list[str] = []

    for line in lines:
        if line.strip().startswith("```"):
            if in_code_block:
                code = html.escape("\n".join(code_lines))
                blocks.append(f"<pre><code>{code}</code></pre>")
                code_lines = []
                in_code_block = False
            else:
                in_code_block = True
                code_lines = []
            continue

        if in_code_block:
            code_lines.append(line)
        else:
            blocks.append(_render_inline_matrix_html(line))

    if in_code_block:
        code = html.escape("\n".join(code_lines))
        blocks.append(f"<pre><code>{code}</code></pre>")

    return "<br>\n".join(blocks)


def build_matrix_text_content(text: str, mentions: list[str]) -> dict[str, Any]:
    """Build Matrix text content with visible mentions and m.mentions."""
    body = text or ""
    formatted_body = _render_matrix_formatted_body(body)

    visible_mentions: list[str] = []
    for mxid in mentions:
        if not mxid:
            continue
        if mxid not in body:
            body = f"{mxid} {body}" if body else mxid
        visible_mentions.append(mxid)

    for mxid in visible_mentions:
        mxid_enc = urllib.parse.quote(mxid, safe="")
        display = html.escape(mxid.split(":")[0].lstrip("@") or mxid)
        anchor = f'<a href="https://matrix.to/#/{mxid_enc}">{display}</a>'
        escaped_mxid = html.escape(mxid)
        if escaped_mxid in formatted_body:
            formatted_body = formatted_body.replace(escaped_mxid, anchor, 1)
        else:
            formatted_body = f"{anchor} {formatted_body}" if formatted_body else anchor

    content: dict[str, Any] = {
        "msgtype": "m.text",
        "body": body,
        "format": "org.matrix.custom.html",
        "formatted_body": formatted_body,
    }
    if visible_mentions:
        content["m.mentions"] = {"user_ids": visible_mentions}
    return content


def _read_config_value(obj: Any, *names: str) -> Any:
    for name in names:
        if isinstance(obj, dict) and name in obj:
            return obj[name]
        if hasattr(obj, name):
            return getattr(obj, name)
    return None


def _matrix_config_for_agent(account_id: str) -> tuple[str, str, str]:
    from copaw.config.config import load_agent_config

    agent_config = load_agent_config(account_id or "default")
    channels = _read_config_value(agent_config, "channels") or {}
    matrix_cfg = _read_config_value(channels, "matrix") or {}

    homeserver = _read_config_value(matrix_cfg, "homeserver") or ""
    access_token = _read_config_value(matrix_cfg, "access_token", "accessToken") or ""
    user_id = _read_config_value(matrix_cfg, "user_id", "userId") or ""

    missing = [
        name
        for name, value in (
            ("homeserver", homeserver),
            ("access_token", access_token),
            ("user_id", user_id),
        )
        if not value
    ]
    if missing:
        raise MessageToolError(
            f"matrix channel config is missing: {', '.join(missing)}",
        )
    return str(homeserver), str(access_token), str(user_id)


async def _send_matrix_room_message(
    *,
    room_id: str,
    content: dict[str, Any],
    account_id: str,
) -> str | None:
    from nio import AsyncClient

    homeserver, access_token, user_id = _matrix_config_for_agent(account_id)
    client = AsyncClient(homeserver, user=user_id)
    client.access_token = access_token
    try:
        response = await client.room_send(
            room_id,
            "m.room.message",
            content,
            ignore_unverified_devices=True,
        )
        event_id = getattr(response, "event_id", None)
        if event_id:
            return str(event_id)

        message = getattr(response, "message", None) or str(response)
        raise MessageToolError(f"matrix room_send failed: {message}")
    finally:
        await client.close()


async def message(
    action: str,
    channel: str = "matrix",
    target: str | None = None,
    message: str | None = None,
    accountId: str = "default",
    dryRun: bool = False,
) -> ToolResponse:
    """Send a message using OpenClaw-compatible routing semantics."""
    try:
        if action != "send":
            raise MessageToolError("only action='send' is supported")
        if channel != "matrix":
            raise MessageToolError("only channel='matrix' is supported")
        if message is None or not message.strip():
            raise MessageToolError("message is required")

        parsed_target = parse_matrix_target(target or "")
        filtered_message = validate_matrix_message_policy(
            message,
            extract_matrix_mentions(message),
        )
        mentions = extract_matrix_mentions(filtered_message)
        content = build_matrix_text_content(filtered_message, mentions)

        if parsed_target.kind != "room":
            raise MessageToolError(
                "user targets are not supported yet; use a room target",
            )

        result: dict[str, Any] = {
            "channel": "matrix",
            "target": target,
            "targetKind": parsed_target.kind,
            "roomId": parsed_target.identifier,
            "mentions": mentions,
        }

        if dryRun:
            return _ok(dryRun=True, content=content, **result)

        event_id = await _send_matrix_room_message(
            room_id=parsed_target.identifier,
            content=content,
            account_id=accountId or "default",
        )
        return _ok(messageId=event_id, **result)
    except MessageToolError as exc:
        return _error(str(exc), channel=channel, target=target)
    except Exception as exc:  # pragma: no cover - defensive runtime boundary
        return _error(f"matrix message send failed: {exc}", channel=channel, target=target)
