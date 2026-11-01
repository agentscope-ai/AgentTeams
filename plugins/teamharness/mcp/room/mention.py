# matrix mention parsing and markdown-to-matrix rendering (W1-a2: real module)
from __future__ import annotations

import html

import re

from typing import Any

import urllib.parse

# markdown_it / linkify_it stay lazy imports inside _md_to_html (optional
# dependency with a built-in fallback renderer).

MATRIX_USER_RE = re.compile(r"@[a-zA-Z0-9._=+/\-]+:[a-zA-Z0-9.\-]+(?::\d+)?")


MENTION_LOCAL_CHARS = r"a-zA-Z0-9._=+/\-"


SHORT_MATRIX_MENTION_RE = re.compile(
    rf"(?<![{MENTION_LOCAL_CHARS}])@([{MENTION_LOCAL_CHARS}]+)(?![{MENTION_LOCAL_CHARS}])(?!:[a-zA-Z0-9.\-])"
)


# Full Matrix user ID (MXID) gate for m.mentions user_ids: `@localpart:domain`.
# Short names or plain text must never be treated as mentionable identities.
MATRIX_MENTION_MXID_RE = re.compile(r"@[^\s:]+:[^\s]+$")


MATRIX_ROOM_RE = re.compile(r"^![^:\s]+:[^\s]+$")


LOW_INFORMATION_ACKS = {"ack", "acknowledged", "ok", "okay", "done", "received", "收到", "好的", "好"}


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


